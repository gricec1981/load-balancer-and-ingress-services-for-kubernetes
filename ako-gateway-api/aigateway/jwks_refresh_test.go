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
	"reflect"
	"sync"
	"testing"
	"time"
)

func newTestPolicyStore() *PolicyStore {
	return &PolicyStore{
		authPolicyByNsName:        map[string]*AIGatewayAuthPolicy{},
		tokenPolicyByNsName:       map[string]*AITokenRateLimitPolicy{},
		routeToAuthPolicies:       map[string][]string{},
		routeToTokenPolicies:      map[string][]string{},
		modelRoutePolicyByNsName:  map[string]*AIModelRoutePolicy{},
		routeToModelRoutePolicies: map[string][]string{},
		mcpRoutePolicyByNsName:    map[string]*AIMCPRoutePolicy{},
		routeToMCPRoutePolicies:   map[string][]string{},
		guardrailPolicyByNsName:   map[string]*AIGuardrailPolicy{},
		routeToGuardrailPolicies:  map[string][]string{},
		a2aRoutePolicyByNsName:    map[string]*AIA2ARoutePolicy{},
		routeToA2ARoutePolicies:   map[string][]string{},
	}
}

func authPolicyFor(ns, name, mode, route, kind string) *AIGatewayAuthPolicy {
	p := jwtPolicy(mode)
	p.Namespace, p.Name = ns, name
	p.Spec.TargetRef = PolicyTargetRef{Kind: kind, Name: route}
	return p
}

func mcpPolicyFor(ns, name, route, authRef string) *AIMCPRoutePolicy {
	p := &AIMCPRoutePolicy{}
	p.Namespace, p.Name = ns, name
	p.Spec.TargetRef = PolicyTargetRef{Kind: "HTTPRoute", Name: route}
	if authRef != "" {
		p.Spec.AuthRef = &AuthPolicyRef{Name: authRef}
	}
	return p
}

func a2aPolicyFor(ns, name, route, authRef string) *AIA2ARoutePolicy {
	p := &AIA2ARoutePolicy{}
	p.Namespace, p.Name = ns, name
	p.Spec.TargetRef = PolicyTargetRef{Kind: "HTTPRoute", Name: route}
	if authRef != "" {
		p.Spec.AuthRef = &AuthPolicyRef{Name: authRef}
	}
	return p
}

// jwksRefreshStore holds one route of every shape the refresh must include or
// skip.
func jwksRefreshStore() *PolicyStore {
	s := newTestPolicyStore()
	// Included: JWT-mode policies targeting a route directly (both modes).
	s.upsertAuthPolicy(authPolicyFor("ai", "llm-hdr", "jwtHeader", "llm-route", "HTTPRoute"))
	s.upsertAuthPolicy(authPolicyFor("ai", "llm-qry", "jwtQuery", "query-route", ""))
	// A second JWT policy on the same route: enqueued once.
	s.upsertAuthPolicy(authPolicyFor("ai", "llm-hdr-2", "jwtHeader", "llm-route", "HTTPRoute"))
	// Skipped: OAuth mode has no JWKS; a Gateway target is not rebuilt as a route.
	s.upsertAuthPolicy(authPolicyFor("ai", "browser", "", "oauth-route", "HTTPRoute"))
	s.upsertAuthPolicy(authPolicyFor("ai", "gw-wide", "jwtHeader", "gateway-1", "Gateway"))
	// A JWT policy used only through authRef (no route of its own).
	s.upsertAuthPolicy(authPolicyFor("tools", "shared", "jwtHeader", "", ""))
	s.upsertAuthPolicy(authPolicyFor("tools", "shared-oauth", "", "", ""))

	// Included: MCP / A2A routes whose authRef names a JWT-mode policy.
	s.upsertMCPRoutePolicy(mcpPolicyFor("tools", "mcp", "mcp-route", "shared"))
	s.upsertA2ARoutePolicy(a2aPolicyFor("tools", "a2a", "a2a-route", "shared"))
	// Skipped: authRef to an OAuth policy, to a missing policy, no authRef, and
	// an authRef resolved in the wrong namespace.
	s.upsertMCPRoutePolicy(mcpPolicyFor("tools", "mcp-oauth", "mcp-oauth-route", "shared-oauth"))
	s.upsertA2ARoutePolicy(a2aPolicyFor("tools", "a2a-dangling", "a2a-dangling-route", "missing"))
	s.upsertMCPRoutePolicy(mcpPolicyFor("tools", "mcp-open", "mcp-open-route", ""))
	s.upsertA2ARoutePolicy(a2aPolicyFor("other", "a2a-other-ns", "a2a-other-route", "shared"))
	return s
}

var wantJWKSRefreshRoutes = []string{"ai/llm-route", "ai/query-route", "tools/a2a-route", "tools/mcp-route"}

func TestJWKSRefreshRoutes(t *testing.T) {
	if got := jwksRefreshStore().jwksRefreshRoutes(); !reflect.DeepEqual(got, wantJWKSRefreshRoutes) {
		t.Fatalf("routes = %v, want %v", got, wantJWKSRefreshRoutes)
	}
}

// enqueueCapture installs a goroutine-safe re-enqueue hook for one test.
type enqueueCapture struct {
	mu     sync.Mutex
	routes []string
	afters []time.Duration
	ch     chan struct{}
}

func captureEnqueues(t *testing.T) *enqueueCapture {
	t.Helper()
	c := &enqueueCapture{ch: make(chan struct{}, 1024)}
	setAuthRetryHook(func(ns, name string, after time.Duration) {
		c.mu.Lock()
		c.routes = append(c.routes, ns+"/"+name)
		c.afters = append(c.afters, after)
		c.mu.Unlock()
		select {
		case c.ch <- struct{}{}:
		default:
		}
	})
	t.Cleanup(func() { setAuthRetryHook(nil) })
	return c
}

func (c *enqueueCapture) snapshot() ([]string, []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.routes...), append([]time.Duration(nil), c.afters...)
}

// One round enqueues exactly the JWT routes, immediately.
func TestRefreshJWKSRoutesEnqueues(t *testing.T) {
	c := captureEnqueues(t)
	if n := refreshJWKSRoutes(jwksRefreshStore()); n != len(wantJWKSRefreshRoutes) {
		t.Errorf("refreshJWKSRoutes = %d, want %d", n, len(wantJWKSRefreshRoutes))
	}
	routes, afters := c.snapshot()
	if !reflect.DeepEqual(routes, wantJWKSRefreshRoutes) {
		t.Errorf("enqueued %v, want %v", routes, wantJWKSRefreshRoutes)
	}
	for i, a := range afters {
		if a != 0 {
			t.Errorf("route %s enqueued after %s, want immediately", routes[i], a)
		}
	}
}

// Without the hook (event handlers not wired) a round is a no-op, not a panic.
func TestRefreshJWKSRoutesNoHook(t *testing.T) {
	setAuthRetryHook(nil)
	if n := refreshJWKSRoutes(jwksRefreshStore()); n != 0 {
		t.Errorf("refreshJWKSRoutes without a hook = %d, want 0", n)
	}
}

// The ticker drives rounds on its interval, only one runs at a time, and it
// stops when the stop channel closes (after which a new one may start).
func TestJWKSRefreshTicker(t *testing.T) {
	c := captureEnqueues(t)
	store := jwksRefreshStore()
	stop := make(chan struct{})
	done, started := startJWKSRefresh(5*time.Millisecond, stop, func() { refreshJWKSRoutes(store) })
	if !started {
		t.Fatal("first start must start the refresh loop")
	}
	if _, again := startJWKSRefresh(5*time.Millisecond, stop, func() { t.Error("second loop ticked") }); again {
		t.Fatal("a second loop must not start while one runs")
	}

	// Two full rounds.
	for i := 0; i < 2*len(wantJWKSRefreshRoutes); i++ {
		select {
		case <-c.ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("ticker enqueued only %d routes in 5s", i)
		}
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh loop did not stop after the stop channel closed")
	}
	routes, _ := c.snapshot()
	if got := routes[:len(wantJWKSRefreshRoutes)]; !reflect.DeepEqual(got, wantJWKSRefreshRoutes) {
		t.Errorf("first round enqueued %v, want %v", got, wantJWKSRefreshRoutes)
	}

	// Stopped: a new loop may start (e.g. after an AKO restart in-process).
	stop2 := make(chan struct{})
	done2, started := startJWKSRefresh(time.Hour, stop2, func() {})
	if !started {
		t.Fatal("a loop must be able to start once the previous one stopped")
	}
	close(stop2)
	<-done2
}

// "0" disables the refresh; unset or invalid falls back to the default; tiny
// values are raised to the minimum.
func TestJWKSRefreshInterval(t *testing.T) {
	cases := map[string]time.Duration{
		"":       DefaultJWKSRefreshInterval,
		"0":      0,
		"0s":     0,
		"30m":    30 * time.Minute,
		" 2h ":   2 * time.Hour,
		"1s":     minJWKSRefreshInterval,
		"-5m":    DefaultJWKSRefreshInterval,
		"hourly": DefaultJWKSRefreshInterval,
	}
	for in, want := range cases {
		if got := parseJWKSRefreshInterval(in); got != want {
			t.Errorf("parseJWKSRefreshInterval(%q) = %s, want %s", in, got, want)
		}
	}
}

// Disabled via the environment: nothing starts, nothing ticks.
func TestStartJWKSRefreshDisabled(t *testing.T) {
	t.Setenv(JWKSRefreshIntervalEnv, "0")
	stop := make(chan struct{})
	defer close(stop)
	if StartJWKSRefresh(stop) {
		t.Fatal(`StartJWKSRefresh must not start a loop when the interval is "0"`)
	}
	if _, started := startJWKSRefresh(0, stop, func() { t.Error("disabled loop ticked") }); started {
		t.Fatal("a zero interval must not start a loop")
	}
}
