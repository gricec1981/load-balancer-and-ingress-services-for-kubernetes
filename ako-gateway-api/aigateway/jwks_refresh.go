/*
 * Copyright © 2025 Broadcom Inc. and/or its subsidiaries. All Rights Reserved.
 * All Rights Reserved.
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*   http://www.apache.org/licenses/LICENSE-2.0
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*/

package aigateway

import (
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// Periodic JWKS refresh
// ─────────────────────
// A JWT-mode AIGatewayAuthPolicy's keyset is fetched by AKO when a route that
// realizes it is rebuilt, and AKO's informers have no periodic resync. Without
// a schedule the keyset would be refetched only when something unrelated
// touched the route: rotated IdP keys would never reach the SE, and the
// last-known-good window (JWKSMaxStale, measured from the last successful
// fetch) would run out on a quiet route, so a brief IdP outage after a quiet
// day would fail the route closed although its keys were fine.
//
// So a single ticker re-enqueues, every JWKSRefreshInterval, each HTTPRoute
// whose rebuild realizes a JWT-mode policy: the routes such a policy targets,
// and the MCP / A2A routes whose route policy's authRef names one. The rebuild
// refetches the keyset (rewriting the JWTServerProfile when the IdP rotated) or,
// if the IdP is down, keeps the last-known-good keyset as usual.

// JWKSRefreshIntervalEnv names the environment variable that sets the refresh
// interval as a Go duration ("30m", "2h"). "0" disables the refresh.
const JWKSRefreshIntervalEnv = "AI_GATEWAY_JWKS_REFRESH_INTERVAL"

// DefaultJWKSRefreshInterval applies when JWKSRefreshIntervalEnv is unset or
// invalid.
const DefaultJWKSRefreshInterval = time.Hour

// minJWKSRefreshInterval is the shortest interval accepted, so a typo ("1s")
// cannot turn the refresh into a rebuild storm and an IdP hammering.
const minJWKSRefreshInterval = time.Minute

// JWKSRefreshInterval returns the configured refresh interval; 0 means the
// periodic refresh is disabled.
func JWKSRefreshInterval() time.Duration {
	return parseJWKSRefreshInterval(os.Getenv(JWKSRefreshIntervalEnv))
}

func parseJWKSRefreshInterval(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultJWKSRefreshInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		utils.AviLog.Warnf("%s=%q is not a non-negative Go duration; using %s", JWKSRefreshIntervalEnv, v, DefaultJWKSRefreshInterval)
		return DefaultJWKSRefreshInterval
	}
	if d == 0 {
		return 0
	}
	if d < minJWKSRefreshInterval {
		utils.AviLog.Warnf("%s=%s is below the minimum; using %s", JWKSRefreshIntervalEnv, d, minJWKSRefreshInterval)
		d = minJWKSRefreshInterval
	}
	if d > JWKSMaxStale-jwksProvenanceRewriteAfter {
		utils.AviLog.Warnf("%s=%s: a keyset may not be refreshed within its %s last-known-good window; routes can fail closed on a brief IdP outage",
			JWKSRefreshIntervalEnv, d, JWKSMaxStale)
	}
	return d
}

// jwksRefreshRoutes returns "ns/name" of every HTTPRoute whose rebuild
// realizes a JWT-mode AIGatewayAuthPolicy, sorted and without duplicates.
func (s *PolicyStore) jwksRefreshRoutes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := map[string]struct{}{}
	add := func(ns string, ref PolicyTargetRef) {
		// Route policies are indexed by target name alone (routeTo* maps), and
		// only HTTPRoute targets are rebuilt from those indexes.
		if ref.Name == "" || (ref.Kind != "" && ref.Kind != lib.HTTPRoute) {
			return
		}
		set[ns+"/"+ref.Name] = struct{}{}
	}
	isJWT := func(p *AIGatewayAuthPolicy) bool {
		return p != nil && p.Spec.EffectiveAuthMode().IsJWTMode()
	}
	for _, p := range s.authPolicyByNsName {
		if isJWT(p) {
			add(p.Namespace, p.Spec.TargetRef)
		}
	}
	refersToJWT := func(ns string, ref *AuthPolicyRef) bool {
		return ref != nil && ref.Name != "" && isJWT(s.authPolicyByNsName[ns+"/"+ref.Name])
	}
	for _, p := range s.mcpRoutePolicyByNsName {
		if p != nil && refersToJWT(p.Namespace, p.Spec.AuthRef) {
			add(p.Namespace, p.Spec.TargetRef)
		}
	}
	for _, p := range s.a2aRoutePolicyByNsName {
		if p != nil && refersToJWT(p.Namespace, p.Spec.AuthRef) {
			add(p.Namespace, p.Spec.TargetRef)
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// refreshJWKSRoutes re-enqueues every route jwksRefreshRoutes names in s, via
// the same hook the fail-closed retry uses, and returns how many it enqueued.
func refreshJWKSRoutes(s *PolicyStore) int {
	authRetryMu.RLock()
	hook := authRetryHook
	authRetryMu.RUnlock()
	if hook == nil {
		utils.AviLog.Warnf("JWKS refresh: no re-enqueue hook installed; skipping this round")
		return 0
	}
	routes := s.jwksRefreshRoutes()
	for _, r := range routes {
		ns, name, _ := strings.Cut(r, "/")
		hook(ns, name, 0)
	}
	if len(routes) > 0 {
		utils.AviLog.Infof("JWKS refresh: re-enqueued %d HTTPRoute(s) with JWT-mode auth: %v", len(routes), routes)
	}
	return len(routes)
}

var (
	jwksRefreshMu      sync.Mutex
	jwksRefreshRunning bool
)

// StartJWKSRefresh starts the periodic JWKS refresh at JWKSRefreshInterval,
// until stopCh closes. At most one refresh loop runs per process: a call while
// one is running does nothing. It reports whether it started one.
func StartJWKSRefresh(stopCh <-chan struct{}) bool {
	interval := JWKSRefreshInterval()
	if interval <= 0 {
		utils.AviLog.Infof("JWKS refresh disabled (%s=0); JWT keysets refresh only on route events", JWKSRefreshIntervalEnv)
		return false
	}
	_, started := startJWKSRefresh(interval, stopCh, func() { refreshJWKSRoutes(SharedPolicyStore()) })
	if started {
		utils.AviLog.Infof("JWKS refresh: every %s", interval)
	}
	return started
}

// startJWKSRefresh runs tick every interval until stopCh closes. It returns a
// channel closed once the loop has stopped, and false (and a nil channel) when
// the interval disables it or a loop is already running.
func startJWKSRefresh(interval time.Duration, stopCh <-chan struct{}, tick func()) (<-chan struct{}, bool) {
	if interval <= 0 {
		return nil, false
	}
	jwksRefreshMu.Lock()
	if jwksRefreshRunning {
		jwksRefreshMu.Unlock()
		return nil, false
	}
	jwksRefreshRunning = true
	jwksRefreshMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			jwksRefreshMu.Lock()
			jwksRefreshRunning = false
			jwksRefreshMu.Unlock()
		}()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				tick()
			}
		}
	}()
	return done, true
}
