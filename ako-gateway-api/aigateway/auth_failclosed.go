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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// Fail-closed authentication
// ──────────────────────────
// An AIGatewayAuthPolicy is realized as Avi objects (JWTServerProfile /
// AuthProfile / SSOPolicy, or the OAuth pool + profile + policy) that the child
// VS then references. If any of them cannot be realized — the JWKS fetch fails,
// the controller rejects a write — the VS would otherwise be published with no
// SSO policy at all, i.e. the route silently serves UNAUTHENTICATED traffic to
// every policy layer behind it. That happened live: a JWKS fetch failed on DNS
// and then on an untrusted issuer CA, and anonymous requests reached the
// backend-side DataScripts.
//
// Instead, the route fails closed: a request-phase VSDataScript is placed FIRST
// on the child VS (index 0, so it runs before the model-route, MCP-session and
// token-budget scripts) and answers every request 503 {"error":"auth_unavailable"}.
// It is removed again the next time the policy realizes, and a delayed re-enqueue
// of the route guarantees that "next time" comes: AKO's informers have no
// periodic resync, so without it a transient failure would stick until something
// unrelated touched the route.
//
// Child VS nodes are carried over between reconciles, so the guard is also
// swept per reconcile (BeginAuthGuardPass / EndAuthGuardPass): a rebuilt child
// VS that nothing denied during the pass loses the guard. That is how a route
// whose auth requirement went away (policy deleted, dangling authRef removed)
// recovers without a restart.
//
// The deny is a last resort. A JWKS fetch failure keeps the last-known-good
// keyset instead, but only while Avi still holds one fetched from the SAME
// issuer and jwksUri the policy names, refreshed within JWKSMaxStale (see
// ensureJWTServerProfile). A transient IdP outage never takes down traffic that
// still has valid keys, and a policy change is never papered over.

// DSNameSuffixAuthDeny names the fail-closed guard DataScript on a VS.
const DSNameSuffixAuthDeny = "-ai-auth-deny"

// DSAuthDenyName returns the fail-closed guard DataScript name for a VS.
func DSAuthDenyName(vsName string) string { return vsName + DSNameSuffixAuthDeny }

// AuthUnavailableStatus is the HTTP status the guard answers with. 503 rather
// than 401/403: the client did nothing wrong, the gateway cannot authenticate
// anyone right now, and a retry may succeed.
const AuthUnavailableStatus = 503

// AuthRetryAfter is how long after a failed (or stale, last-known-good) auth
// realization the route is re-enqueued, and the Retry-After the guard sends.
const AuthRetryAfter = 30 * time.Second

// authUnavailableScript is the guard's HTTP_REQ Lua. It is unconditional on
// purpose: while the policy is unrealized nothing on this VS can tell a valid
// token from a forged one, and that includes the admin paths the SSO policy
// would normally exempt (their DataScripts assume the auth layer ran).
func authUnavailableScript() string {
	return `-- AKO AI Gateway: fail-closed auth guard.
-- The AIGatewayAuthPolicy for this route could not be realized in Avi, so this
-- VS has no token validation. Reject every request instead of serving it
-- unauthenticated; AKO removes this script once the policy realizes.
avi.http.response(503,
  {["Content-Type"] = "application/json", ["Retry-After"] = "` + strconv.Itoa(int(AuthRetryAfter/time.Second)) + `"},
  '{"error":"auth_unavailable"}')
return`
}

// DenyUnauthenticated makes the VS reject every request with 503
// auth_unavailable, and schedules a retry of the route. Use it wherever an auth
// policy is required for a VS but cannot be applied (a failed realization here,
// or a dangling authRef on an MCP/A2A route policy).
func DenyUnauthenticated(key string, vsNode nodes.AviVsEvhSniModel, reason string) {
	vsName := vsNode.GetName()
	name := DSAuthDenyName(vsName)
	guard := &nodes.AviHTTPDataScriptNode{
		Name:       name,
		Tenant:     vsNode.GetTenant(),
		DataScript: &nodes.DataScript{Evt: DSEvtHTTPReq, Script: authUnavailableScript()},
	}
	// The guard and its pass record change together under guardPassMu, so a
	// concurrent pass's sweep sees either neither or both (see EndAuthGuardPass).
	guardPassMu.Lock()
	// Prepend (not upsert-in-place): the guard must hold the lowest index so no
	// other script on the VS runs for a request it is about to reject.
	existing := vsNode.GetHTTPDSrefs()
	out := make([]*nodes.AviHTTPDataScriptNode, 0, len(existing)+1)
	out = append(out, guard)
	for _, ds := range existing {
		if ds != nil && ds.Name != name {
			out = append(out, ds)
		}
	}
	vsNode.SetHTTPDSrefs(out)
	markDeniedLocked(vsName)
	guardPassMu.Unlock()
	utils.AviLog.Errorf("key: %s, msg: FAIL-CLOSED: VS %s rejects all requests with %d auth_unavailable: %s",
		key, vsName, AuthUnavailableStatus, reason)
	requestAuthRetry(key, vsNode)
}

// clearAuthUnavailable removes the fail-closed guard from the VS. Called when
// the auth policy realizes; child VS nodes are carried over between
// reconciles, so a guard added by an earlier failure would otherwise persist.
//
// It leaves alone a guard added during any guard pass still open: one auth
// policy realizing must not lift the deny that another required policy on the
// route (e.g. a dangling MCP authRef) put there moments earlier, in the same
// rebuild or in a concurrent one.
func clearAuthUnavailable(key string, vsNode nodes.AviVsEvhSniModel) {
	guardPassMu.Lock()
	removed := !deniedInActivePassLocked(vsNode.GetName()) && removeAuthGuard(vsNode)
	guardPassMu.Unlock()
	if removed {
		utils.AviLog.Infof("key: %s, msg: auth realized; removed fail-closed guard %s from VS %s",
			key, DSAuthDenyName(vsNode.GetName()), vsNode.GetName())
	}
}

// removeAuthGuard drops the guard DataScript from the VS and reports whether it
// was there. guardPassMu must be held.
func removeAuthGuard(vsNode nodes.AviVsEvhSniModel) bool {
	name := DSAuthDenyName(vsNode.GetName())
	existing := vsNode.GetHTTPDSrefs()
	for i, ds := range existing {
		if ds != nil && ds.Name == name {
			out := make([]*nodes.AviHTTPDataScriptNode, 0, len(existing)-1)
			out = append(out, existing[:i]...)
			out = append(out, existing[i+1:]...)
			vsNode.SetHTTPDSrefs(out)
			return true
		}
	}
	return false
}

// ─── Per-reconcile guard pass ────────────────────────────────────────────────

// Without a pass, the guard is removed only by a later successful realization.
// A route whose auth requirement disappears (its AIGatewayAuthPolicy deleted, a
// dangling authRef removed from its MCP/A2A policy) would then answer 503
// forever: nothing calls ApplyAuthPolicy or DenyUnauthenticated for it again and
// no retry is scheduled. The pass makes the guard a function of the current
// reconcile instead. The graph layer brackets each route rebuild with
// BeginAuthGuardPass / EndAuthGuardPass, DenyUnauthenticated records the VSes it
// guards, and EndAuthGuardPass strips the guard from every rebuilt child VS that
// was not denied. A VS that still needs the guard never loses it, because the
// sweep runs after the rebuild has re-denied it and before the model is saved
// and published.
//
// Passes can overlap, including for the SAME ingestion key:
// GatewayController.FullSyncK8s calls DequeueIngestion(key, true) directly
// (quick sync on the full-sync thread) while the ingestion workers keep
// dequeuing, and different keys (a Gateway and one of its HTTPRoutes) rebuild
// the same child VS. DenyUnauthenticated cannot tell which invocation it runs
// in, so the records are kept per pass instance, not per key, and err toward
// keeping the guard:
//
//   - a deny is recorded in EVERY pass open at the time (VS names are unique,
//     so a record only ever protects the VS that was denied);
//   - a pass that ends hands its records to every pass still open;
//   - a sweep spares every VS recorded in its own pass or in any pass still
//     open.
//
// So no pass lifts a guard that a pass overlapping it in time added. The guard
// and the records change together under guardPassMu, so a sweep never races a
// concurrent deny of the same VS. At worst an overlap keeps a guard one
// reconcile longer than needed, and something must bring the clean pass that
// lifts it:
//
//   - a deny made while the pass was open scheduled its own retry
//     (AuthRetryAfter), which rebuilds the route after the deny;
//   - a guard spared for any other reason (a record handed over by a pass that
//     already ended, or held by a pass still open, e.g. one that has not
//     finished yet or leaked until authGuardPassMaxAge) has no retry of its
//     own that is guaranteed to come after this sweep, so the sweep schedules
//     one for the VS's route. That retry's pass either finds the requirement
//     gone and lifts the guard, or is itself spared and schedules the next.

// AuthGuardPass is one route rebuild's guard pass, from BeginAuthGuardPass.
type AuthGuardPass struct {
	key     string
	started time.Time
	// denied holds the VS names denied while this pass was open, by any pass.
	// Each such deny scheduled its own retry.
	denied map[string]struct{}
	// inherited holds the VS names handed over by overlapping passes that
	// ended first. Their denies may predate this pass (and their retries may
	// already have run), so sparing a guard for them needs a fresh retry.
	inherited map[string]struct{}
}

// authGuardPassMaxAge bounds how long a pass may stay open. A pass that was
// never ended (its rebuild panicked) would otherwise protect every VS it saw
// denied forever, so BeginAuthGuardPass drops passes older than this. A route
// rebuild takes seconds.
const authGuardPassMaxAge = 10 * time.Minute

// guardPassNow is the clock for pass ages. Tests swap it.
var guardPassNow = time.Now

var (
	// guardPassMu guards activePasses, every pass's denied set, and the guard
	// DataScript on VS nodes (added, cleared and swept only while holding it).
	guardPassMu  sync.Mutex
	activePasses = map[*AuthGuardPass]struct{}{}
)

// BeginAuthGuardPass opens a guard pass for one route rebuild under key. The
// caller must hand the pass to EndAuthGuardPass when the rebuild is done.
func BeginAuthGuardPass(key string) *AuthGuardPass {
	pass := &AuthGuardPass{key: key, started: guardPassNow(), denied: map[string]struct{}{}, inherited: map[string]struct{}{}}
	guardPassMu.Lock()
	defer guardPassMu.Unlock()
	for q := range activePasses {
		if pass.started.Sub(q.started) > authGuardPassMaxAge {
			delete(activePasses, q)
			utils.AviLog.Warnf("key: %s, msg: dropped auth guard pass of key %s, opened at %s and never ended",
				key, q.key, q.started.UTC().Format(time.RFC3339))
		}
	}
	activePasses[pass] = struct{}{}
	return pass
}

// EndAuthGuardPass closes pass and removes the fail-closed guard from each VS
// in rebuilt that neither pass nor any pass still open recorded as denied.
// rebuilt must hold only child VSes whose AI policies the pass re-applied; a VS
// the pass did not rebuild keeps whatever guard it has. Ending a pass twice is
// harmless.
//
// A guarded VS that is spared although no deny was made while pass was open
// (it is protected only by a handed-over record or by a pass still open) gets
// an auth retry of its route, so a later clean pass can lift the guard once
// auth has recovered.
func EndAuthGuardPass(pass *AuthGuardPass, rebuilt []nodes.AviVsEvhSniModel) {
	if pass == nil {
		return
	}
	retry := endAuthGuardPassLocked(pass, rebuilt)
	// The hook takes its own lock and touches the workqueues: call it after
	// guardPassMu is released.
	for _, vs := range retry {
		utils.AviLog.Infof("key: %s, msg: kept fail-closed guard %s on VS %s for a deny outside this pass; retrying its route",
			pass.key, DSAuthDenyName(vs.GetName()), vs.GetName())
		requestAuthRetry(pass.key, vs)
	}
}

// endAuthGuardPassLocked does EndAuthGuardPass's bookkeeping and sweep under
// guardPassMu and returns the guarded VSes it spared that need a retry.
func endAuthGuardPassLocked(pass *AuthGuardPass, rebuilt []nodes.AviVsEvhSniModel) []nodes.AviVsEvhSniModel {
	guardPassMu.Lock()
	defer guardPassMu.Unlock()
	delete(activePasses, pass)
	protected := make(map[string]struct{}, len(pass.denied)+len(pass.inherited))
	for vs := range pass.denied {
		protected[vs] = struct{}{}
	}
	for vs := range pass.inherited {
		protected[vs] = struct{}{}
	}
	for q := range activePasses {
		for vs := range q.denied {
			protected[vs] = struct{}{}
		}
		for vs := range q.inherited {
			protected[vs] = struct{}{}
		}
		for vs := range pass.denied {
			q.inherited[vs] = struct{}{}
		}
		for vs := range pass.inherited {
			q.inherited[vs] = struct{}{}
		}
	}
	var retry []nodes.AviVsEvhSniModel
	for _, vs := range rebuilt {
		if vs == nil {
			continue
		}
		if _, ok := protected[vs.GetName()]; ok {
			if _, own := pass.denied[vs.GetName()]; !own && hasAuthGuard(vs) {
				retry = append(retry, vs)
			}
			continue
		}
		if removeAuthGuard(vs) {
			utils.AviLog.Infof("key: %s, msg: VS %s no longer requires unrealized auth; removed fail-closed guard %s",
				pass.key, vs.GetName(), DSAuthDenyName(vs.GetName()))
		}
	}
	return retry
}

// hasAuthGuard reports whether the VS carries the fail-closed guard.
// guardPassMu must be held.
func hasAuthGuard(vsNode nodes.AviVsEvhSniModel) bool {
	name := DSAuthDenyName(vsNode.GetName())
	for _, ds := range vsNode.GetHTTPDSrefs() {
		if ds != nil && ds.Name == name {
			return true
		}
	}
	return false
}

// markDeniedLocked records vsName as denied in every open pass. Outside any
// pass it does nothing. guardPassMu must be held.
func markDeniedLocked(vsName string) {
	for q := range activePasses {
		q.denied[vsName] = struct{}{}
	}
}

// deniedInActivePassLocked reports whether any open pass recorded vsName as
// denied, itself or by hand-over. guardPassMu must be held.
func deniedInActivePassLocked(vsName string) bool {
	for q := range activePasses {
		if _, ok := q.denied[vsName]; ok {
			return true
		}
		if _, ok := q.inherited[vsName]; ok {
			return true
		}
	}
	return false
}

// ─── Retry ───────────────────────────────────────────────────────────────────

// authRetryHook re-enqueues an HTTPRoute ("ns", "name") after a delay. It is
// installed by SetupAuthPolicyEventHandlers, which owns the workqueues, and
// shared by the fail-closed retry and the periodic JWKS refresh.
var (
	authRetryMu   sync.RWMutex
	authRetryHook func(ns, name string, after time.Duration)
)

func setAuthRetryHook(fn func(ns, name string, after time.Duration)) {
	authRetryMu.Lock()
	defer authRetryMu.Unlock()
	authRetryHook = fn
}

// requestAuthRetry schedules a rebuild of the route that owns vsNode, so a
// failed or stale auth realization is retried without waiting for an unrelated
// event. The route comes from the node's ServiceMetadata ("ns/name"), which is
// set for every Gateway API child VS before AI policies are applied.
func requestAuthRetry(key string, vsNode nodes.AviVsEvhSniModel) {
	authRetryMu.RLock()
	hook := authRetryHook
	authRetryMu.RUnlock()
	if hook == nil {
		utils.AviLog.Warnf("key: %s, msg: no auth retry hook installed; VS %s will retry on the next route event", key, vsNode.GetName())
		return
	}
	evh, ok := vsNode.(*nodes.AviEvhVsNode)
	if !ok {
		return
	}
	ns, name, found := strings.Cut(evh.ServiceMetadata.HTTPRoute, "/")
	if !found || ns == "" || name == "" {
		utils.AviLog.Warnf("key: %s, msg: VS %s has no HTTPRoute metadata; cannot schedule an auth retry", key, vsNode.GetName())
		return
	}
	hook(ns, name, AuthRetryAfter)
	utils.AviLog.Infof("key: %s, msg: scheduled auth retry for HTTPRoute %s/%s in %s", key, ns, name, AuthRetryAfter)
}
