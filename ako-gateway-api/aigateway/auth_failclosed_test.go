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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
)

// fakeJWTBackend stands in for the IdP and the Avi controller so the
// fail-closed / last-known-good decisions can be driven directly.
type fakeJWTBackend struct {
	fetchErr  error            // FetchJWKS fails with this when non-nil
	lkg       jwtLastKnownGood // what Avi already holds for the policy
	lookupErr error            // LastKnownGood fails with this when non-nil
	authErr   error            // EnsureAuthProfile fails with this when non-nil

	upserts         int            // UpsertServerProfile calls
	authProfileFrom string         // serverProfileName EnsureAuthProfile was given
	authProfileProv jwksProvenance // provenance EnsureAuthProfile was given
}

func (f *fakeJWTBackend) FetchJWKS(string) (string, error) {
	if f.fetchErr != nil {
		return "", f.fetchErr
	}
	return fakeJWKS, nil
}

// fakeJWKS is the keyset fakeJWTBackend serves.
const fakeJWKS = `{"keys":[{"kty":"RSA","kid":"k1","n":"AQAB","e":"AQAB"}]}`

func (f *fakeJWTBackend) LastKnownGood(*AIGatewayAuthPolicy) (jwtLastKnownGood, error) {
	return f.lkg, f.lookupErr
}
func (f *fakeJWTBackend) UpsertServerProfile(_ string, p *AIGatewayAuthPolicy, _ string) (string, error) {
	f.upserts++
	return jwtServerProfileName(p), nil
}
func (f *fakeJWTBackend) EnsureAuthProfile(_ string, p *AIGatewayAuthPolicy, srv string, prov jwksProvenance) (string, error) {
	f.authProfileFrom = srv
	f.authProfileProv = prov
	if f.authErr != nil {
		return "", f.authErr
	}
	return jwtAuthProfileName(p), nil
}
func (f *fakeJWTBackend) EnsureSSOPolicy(_ string, p *AIGatewayAuthPolicy, _ string) (string, error) {
	return jwtSSOPolicyName(p), nil
}

// testNow is the fixed clock the JWT tests run on.
var testNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// withJWTBackend swaps the package backend and clock, and captures auth retries,
// for one test.
func withJWTBackend(t *testing.T, b jwtBackend) *[]string {
	t.Helper()
	prevBackend, prevNow := jwtAvi, jwtNow
	jwtAvi = b
	jwtNow = func() time.Time { return testNow }
	var retries []string
	setAuthRetryHook(func(ns, name string, after time.Duration) {
		retries = append(retries, ns+"/"+name)
	})
	t.Cleanup(func() {
		jwtAvi, jwtNow = prevBackend, prevNow
		setAuthRetryHook(nil)
	})
	return &retries
}

// goodLKG is a last-known-good profile built for p's current issuer and
// jwksUri, refreshed age ago.
func goodLKG(p *AIGatewayAuthPolicy, age time.Duration) jwtLastKnownGood {
	return jwtLastKnownGood{
		HasKeys:    true,
		Issuer:     p.Spec.JWT.Issuer,
		Provenance: jwksProvenance{Source: jwksSourceID(p), Refreshed: testNow.Add(-age)},
	}
}

func jwtPolicy(mode string) *AIGatewayAuthPolicy {
	p := &AIGatewayAuthPolicy{}
	p.Namespace = "ai"
	p.Name = "llm-auth"
	p.Spec.AuthMode = mode
	p.Spec.JWT.Issuer = "https://issuer.example"
	p.Spec.JWT.JwksUri = "https://issuer.example/jwks"
	p.Spec.JWT.Audiences = []string{"llm"}
	return p
}

// childVS returns a child VS as the route translator hands it over: route
// metadata set, and an existing (model-route) DataScript already attached.
func childVS() *nodes.AviEvhVsNode {
	vs := &nodes.AviEvhVsNode{Name: "vs-llm", Tenant: "admin"}
	vs.ServiceMetadata = lib.ServiceMetadataObj{HTTPRoute: "ai/llm-route"}
	vs.HTTPDSrefs = []*nodes.AviHTTPDataScriptNode{{
		Name:       "vs-llm-ai-model-reqdata",
		DataScript: &nodes.DataScript{Evt: DSEvtHTTPReqData, Script: "-- model route"},
	}}
	return vs
}

func denyNode(vs *nodes.AviEvhVsNode) (int, *nodes.AviHTTPDataScriptNode) {
	for i, ds := range vs.HTTPDSrefs {
		if ds.Name == DSAuthDenyName(vs.Name) {
			return i, ds
		}
	}
	return -1, nil
}

// JWKS fetch fails and nothing exists in Avi to fall back on: the VS must be
// failed closed (guard first, SSO not attached) and the route retried.
func TestJWTAuthFetchErrorNoProfileFailsClosed(t *testing.T) {
	be := &fakeJWTBackend{fetchErr: errors.New("x509: certificate signed by unknown authority")}
	retries := withJWTBackend(t, be)
	vs := childVS()

	ApplyAuthPolicy("key", jwtPolicy("jwtHeader"), vs, "llm.example", "/v1")

	idx, ds := denyNode(vs)
	if ds == nil {
		t.Fatalf("expected fail-closed guard %s on the VS, got %d DataScripts", DSAuthDenyName(vs.Name), len(vs.HTTPDSrefs))
	}
	if idx != 0 {
		t.Errorf("guard must run first (index 0), got index %d", idx)
	}
	if ds.Evt != DSEvtHTTPReq {
		t.Errorf("guard event = %s, want %s", ds.Evt, DSEvtHTTPReq)
	}
	for _, want := range []string{"avi.http.response(503", `'{"error":"auth_unavailable"}'`} {
		if !strings.Contains(ds.Script, want) {
			t.Errorf("guard script missing %q:\n%s", want, ds.Script)
		}
	}
	if len(vs.HTTPDSrefs) != 2 {
		t.Errorf("existing DataScripts must be kept behind the guard, got %d nodes", len(vs.HTTPDSrefs))
	}
	if be.upserts != 0 || be.authProfileFrom != "" {
		t.Errorf("nothing downstream of a failed server profile may be written (upserts=%d, authProfileFrom=%q)", be.upserts, be.authProfileFrom)
	}
	gf := vs.GetGeneratedFields()
	if gf.SsoPolicyRef != nil || gf.JwtConfig != nil {
		t.Errorf("no SSO/JWT config may be attached on failure: sso=%v jwt=%v", gf.SsoPolicyRef, gf.JwtConfig)
	}
	if len(*retries) != 1 || (*retries)[0] != "ai/llm-route" {
		t.Errorf("expected one retry of ai/llm-route, got %v", *retries)
	}
}

// A failed lookup of the existing profile is also "nothing usable".
func TestJWTAuthFetchErrorLookupErrorFailsClosed(t *testing.T) {
	withJWTBackend(t, &fakeJWTBackend{fetchErr: errors.New("dns"), lookupErr: errors.New("controller unreachable")})
	vs := childVS()
	ApplyAuthPolicy("key", jwtPolicy("jwtQuery"), vs, "", "/v1")
	if _, ds := denyNode(vs); ds == nil {
		t.Fatal("expected fail-closed guard when the existing profile cannot be looked up")
	}
}

// A later Avi write failure (not the JWKS fetch) fails closed too.
func TestJWTAuthAuthProfileErrorFailsClosed(t *testing.T) {
	withJWTBackend(t, &fakeJWTBackend{authErr: errors.New("PUT 400")})
	vs := childVS()
	ApplyAuthPolicy("key", jwtPolicy("jwtHeader"), vs, "", "/v1")
	if _, ds := denyNode(vs); ds == nil {
		t.Fatal("expected fail-closed guard when the AuthProfile cannot be written")
	}
	if vs.GetGeneratedFields().SsoPolicyRef != nil {
		t.Error("SSO policy must not be attached when the chain is incomplete")
	}
}

// JWKS fetch fails but the JWTServerProfile already exists: keep the
// last-known-good keyset, attach auth as normal, no guard; retry for fresh keys.
func TestJWTAuthFetchErrorExistingProfileReused(t *testing.T) {
	p := jwtPolicy("jwtHeader")
	be := &fakeJWTBackend{fetchErr: errors.New("dial tcp: lookup issuer: no such host"), lkg: goodLKG(p, 2*time.Hour)}
	retries := withJWTBackend(t, be)
	vs := childVS()

	ApplyAuthPolicy("key", p, vs, "", "/v1")

	if _, ds := denyNode(vs); ds != nil {
		t.Fatal("a usable last-known-good profile must not fail the route closed")
	}
	if be.upserts != 0 {
		t.Errorf("the existing profile must be kept, not overwritten (upserts=%d)", be.upserts)
	}
	if be.authProfileFrom != jwtServerProfileName(p) {
		t.Errorf("AuthProfile must reference the existing server profile %q, got %q", jwtServerProfileName(p), be.authProfileFrom)
	}
	if be.authProfileProv != be.lkg.Provenance {
		t.Errorf("the keyset's provenance must be carried over, not refreshed: got %+v, want %+v", be.authProfileProv, be.lkg.Provenance)
	}
	gf := vs.GetGeneratedFields()
	if gf.SsoPolicyRef == nil || *gf.SsoPolicyRef != "/api/ssopolicy?name="+jwtSSOPolicyName(p) {
		t.Errorf("SSO policy not attached: %v", gf.SsoPolicyRef)
	}
	if gf.JwtConfig == nil || gf.JwtConfig.JwtLocation == nil || *gf.JwtConfig.JwtLocation != "JWT_LOCATION_AUTHORIZATION_HEADER" {
		t.Errorf("header-mode jwt_config not attached: %+v", gf.JwtConfig)
	}
	if len(*retries) != 1 {
		t.Errorf("stale keys should schedule one refresh retry, got %v", *retries)
	}
}

// Success in both JWT modes: unchanged config, no guard, no retry.
func TestJWTAuthSuccessNoDeny(t *testing.T) {
	cases := map[string]struct {
		location string
		name     *string
	}{
		"jwtHeader": {location: "JWT_LOCATION_AUTHORIZATION_HEADER"},
		"jwtQuery":  {location: "JWT_LOCATION_QUERY_PARAM", name: strPtr(JwtQueryParamName)},
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			be := &fakeJWTBackend{}
			retries := withJWTBackend(t, be)
			vs := childVS()
			p := jwtPolicy(mode)

			ApplyAuthPolicy("key", p, vs, "", "/v1")

			if _, ds := denyNode(vs); ds != nil {
				t.Fatal("successful realization must not attach the fail-closed guard")
			}
			if len(vs.HTTPDSrefs) != 1 {
				t.Errorf("existing DataScripts must be untouched, got %d", len(vs.HTTPDSrefs))
			}
			if be.upserts != 1 {
				t.Errorf("fresh JWKS must be written to the server profile once, got %d", be.upserts)
			}
			if want := (jwksProvenance{Source: jwksSourceID(p), Keys: jwksKeysID(fakeJWKS), Refreshed: testNow}); be.authProfileProv != want {
				t.Errorf("fresh keyset provenance = %+v, want %+v", be.authProfileProv, want)
			}
			gf := vs.GetGeneratedFields()
			if gf.SsoPolicyRef == nil || *gf.SsoPolicyRef != "/api/ssopolicy?name="+jwtSSOPolicyName(p) {
				t.Errorf("SSO policy not attached: %v", gf.SsoPolicyRef)
			}
			jc := gf.JwtConfig
			if jc == nil || jc.Audience == nil || *jc.Audience != "llm" || jc.JwtLocation == nil || *jc.JwtLocation != want.location {
				t.Fatalf("jwt_config = %+v, want audience=llm location=%s", jc, want.location)
			}
			if (want.name == nil) != (jc.JwtName == nil) || (want.name != nil && *jc.JwtName != *want.name) {
				t.Errorf("jwt_name = %v, want %v", jc.JwtName, want.name)
			}
			if len(*retries) != 0 {
				t.Errorf("success must not schedule a retry, got %v", *retries)
			}
		})
	}
}

// Child VS nodes are carried over between reconciles, so the guard left by a
// failed reconcile must be removed when a later one succeeds.
func TestJWTAuthRecoveryRemovesDeny(t *testing.T) {
	be := &fakeJWTBackend{fetchErr: errors.New("dns")}
	withJWTBackend(t, be)
	vs := childVS()
	p := jwtPolicy("jwtHeader")

	ApplyAuthPolicy("key", p, vs, "", "/v1")
	if _, ds := denyNode(vs); ds == nil {
		t.Fatal("precondition: first reconcile should fail closed")
	}

	be.fetchErr = nil
	ApplyAuthPolicy("key", p, vs, "", "/v1")
	if _, ds := denyNode(vs); ds != nil {
		t.Fatal("guard must be removed once the policy realizes")
	}
	if len(vs.HTTPDSrefs) != 1 || vs.HTTPDSrefs[0].Name != "vs-llm-ai-model-reqdata" {
		t.Errorf("only the guard may be removed, got %v", dsNames(vs))
	}
	if vs.GetGeneratedFields().SsoPolicyRef == nil {
		t.Error("SSO policy must be attached after recovery")
	}
}

// A last-known-good keyset stands in only for the issuer + jwksUri it was
// fetched for, and only for JWKSMaxStale. Anything else fails closed.
func TestJWTAuthLastKnownGoodMustMatchPolicy(t *testing.T) {
	cases := map[string]func(p *AIGatewayAuthPolicy, l *jwtLastKnownGood){
		"issuer changed in the policy": func(p *AIGatewayAuthPolicy, l *jwtLastKnownGood) {
			p.Spec.JWT.Issuer = "https://new-idp.example"
		},
		"jwksUri changed in the policy": func(p *AIGatewayAuthPolicy, l *jwtLastKnownGood) {
			p.Spec.JWT.JwksUri = "https://issuer.example/rotated-jwks"
		},
		"no recorded provenance": func(p *AIGatewayAuthPolicy, l *jwtLastKnownGood) {
			l.Provenance = jwksProvenance{}
		},
		"keyset older than JWKSMaxStale": func(p *AIGatewayAuthPolicy, l *jwtLastKnownGood) {
			l.Provenance.Refreshed = testNow.Add(-JWKSMaxStale - time.Minute)
		},
		"refresh time further in the future than the clock skew allowance": func(p *AIGatewayAuthPolicy, l *jwtLastKnownGood) {
			l.Provenance.Refreshed = testNow.Add(jwksMaxClockSkew + time.Minute)
		},
		"refresh time far in the future": func(p *AIGatewayAuthPolicy, l *jwtLastKnownGood) {
			l.Provenance.Refreshed = testNow.Add(365 * 24 * time.Hour)
		},
		"profile has no keyset": func(p *AIGatewayAuthPolicy, l *jwtLastKnownGood) {
			l.HasKeys = false
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := jwtPolicy("jwtHeader")
			lkg := goodLKG(p, time.Hour) // built from the policy as it was
			mutate(p, &lkg)
			be := &fakeJWTBackend{fetchErr: errors.New("dns"), lkg: lkg}
			withJWTBackend(t, be)
			vs := childVS()

			ApplyAuthPolicy("key", p, vs, "", "/v1")

			if _, ds := denyNode(vs); ds == nil {
				t.Fatal("expected fail-closed guard: the last-known-good keyset no longer matches the policy")
			}
			if be.authProfileFrom != "" {
				t.Error("nothing downstream may be written when the last-known-good keyset is rejected")
			}
			if vs.GetGeneratedFields().SsoPolicyRef != nil {
				t.Error("SSO policy must not be attached")
			}
		})
	}
}

// A refresh time slightly ahead of this clock (another replica's clock, NTP
// drift) is within the skew allowance and still usable.
func TestJWTAuthLastKnownGoodToleratesSmallClockSkew(t *testing.T) {
	p := jwtPolicy("jwtHeader")
	lkg := goodLKG(p, 0)
	lkg.Provenance.Refreshed = testNow.Add(jwksMaxClockSkew - time.Second)
	withJWTBackend(t, &fakeJWTBackend{fetchErr: errors.New("dns"), lkg: lkg})
	vs := childVS()
	ApplyAuthPolicy("key", p, vs, "", "/v1")
	if _, ds := denyNode(vs); ds != nil {
		t.Fatal("a refresh time within the clock skew allowance must still allow the last-known-good keyset")
	}
}

// The provenance survives the round trip through the AuthProfile description,
// with and without the keyset id (descriptions written before it was recorded).
func TestJWKSProvenanceRoundTrip(t *testing.T) {
	for _, p := range []jwksProvenance{
		{Source: jwksSourceID(jwtPolicy("jwtHeader")), Keys: jwksKeysID(fakeJWKS), Refreshed: testNow},
		{Source: jwksSourceID(jwtPolicy("jwtHeader")), Refreshed: testNow},
	} {
		got, ok := parseJWKSProvenance(p.description())
		if !ok || got != p {
			t.Fatalf("round trip = %+v, %v; want %+v", got, ok, p)
		}
	}
	legacy := jwksProvenancePrefix + " source=abc refreshed=2026-09-27T11:00:00Z"
	if got, ok := parseJWKSProvenance(legacy); !ok || got.Keys != "" || got.description() != legacy {
		t.Errorf("legacy provenance %q = %+v, %v; must parse and render back unchanged", legacy, got, ok)
	}
	for _, desc := range []string{"", "hand-written description", jwksProvenancePrefix + " source=abc", jwksProvenancePrefix + " refreshed=not-a-time source=abc"} {
		if _, ok := parseJWKSProvenance(desc); ok {
			t.Errorf("parseJWKSProvenance(%q) accepted an incomplete provenance", desc)
		}
	}
}

// A route whose auth requirement went away (policy deleted, dangling authRef
// removed) is rebuilt with neither DenyUnauthenticated nor ApplyAuthPolicy: the
// pass sweep must lift the guard an earlier reconcile left on the carried-over
// node.
func TestAuthGuardPassLiftsGuardWhenAuthNoLongerApplied(t *testing.T) {
	withJWTBackend(t, &fakeJWTBackend{})
	vs := childVS()

	pass := BeginAuthGuardPass("key")
	DenyUnauthenticated("key", vs, "dangling authRef")
	EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})
	if _, ds := denyNode(vs); ds == nil {
		t.Fatal("a VS denied during the pass must keep its guard")
	}

	// Next reconcile: the authRef is gone, nothing touches auth.
	pass = BeginAuthGuardPass("key")
	EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})
	if _, ds := denyNode(vs); ds != nil {
		t.Fatal("guard must be lifted once the rebuild no longer denies the VS")
	}
	if len(vs.HTTPDSrefs) != 1 || vs.HTTPDSrefs[0].Name != "vs-llm-ai-model-reqdata" {
		t.Errorf("only the guard may be removed, got %v", dsNames(vs))
	}
}

// A VS the pass did not rebuild keeps its guard.
func TestAuthGuardPassLeavesUnrebuiltVS(t *testing.T) {
	withJWTBackend(t, &fakeJWTBackend{})
	vs := childVS()
	DenyUnauthenticated("key", vs, "earlier failure")

	EndAuthGuardPass(BeginAuthGuardPass("key"), nil)
	if _, ds := denyNode(vs); ds == nil {
		t.Fatal("a VS outside the rebuilt set must keep its guard")
	}
}

// Within one pass, a policy that realizes must not lift the guard another
// required policy on the same VS added earlier in the pass.
func TestAuthGuardPassSuccessDoesNotLiftSamePassDeny(t *testing.T) {
	withJWTBackend(t, &fakeJWTBackend{})
	vs := childVS()

	pass := BeginAuthGuardPass("key")
	DenyUnauthenticated("key", vs, "MCP authRef not found")
	ApplyAuthPolicy("key", jwtPolicy("jwtHeader"), vs, "", "/v1") // realizes
	EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})

	if _, ds := denyNode(vs); ds == nil {
		t.Fatal("a same-pass deny must survive another policy realizing")
	}

	// A later pass where every requirement realizes lifts it.
	pass = BeginAuthGuardPass("key")
	ApplyAuthPolicy("key", jwtPolicy("jwtHeader"), vs, "", "/v1")
	EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})
	if _, ds := denyNode(vs); ds != nil {
		t.Fatal("guard must be lifted once every auth requirement realizes")
	}
}

// FullSyncK8s rebuilds a key on the full-sync thread while a worker rebuilds the
// same key: two passes on one key overlap. Whichever of them denies the VS, and
// in whichever order they end, neither sweep may lift that guard.
func TestAuthGuardPassConcurrentPassesSameKey(t *testing.T) {
	rebuilt := func(vs *nodes.AviEvhVsNode) []nodes.AviVsEvhSniModel { return []nodes.AviVsEvhSniModel{vs} }
	type step func(p1, p2 **AuthGuardPass, vs *nodes.AviEvhVsNode)
	begin1 := func(p1, _ **AuthGuardPass, _ *nodes.AviEvhVsNode) { *p1 = BeginAuthGuardPass("key") }
	begin2 := func(_, p2 **AuthGuardPass, _ *nodes.AviEvhVsNode) { *p2 = BeginAuthGuardPass("key") }
	deny := func(_, _ **AuthGuardPass, vs *nodes.AviEvhVsNode) { DenyUnauthenticated("key", vs, "unrealized") }
	realize := func(_, _ **AuthGuardPass, vs *nodes.AviEvhVsNode) {
		ApplyAuthPolicy("key", jwtPolicy("jwtHeader"), vs, "", "/v1")
	}
	end1 := func(p1, _ **AuthGuardPass, vs *nodes.AviEvhVsNode) { EndAuthGuardPass(*p1, rebuilt(vs)) }
	end2 := func(_, p2 **AuthGuardPass, vs *nodes.AviEvhVsNode) { EndAuthGuardPass(*p2, rebuilt(vs)) }

	cases := map[string][]step{
		"second pass denies, first ends first":             {begin1, begin2, deny, end1, end2},
		"first pass denies, first ends first":              {begin1, deny, begin2, end1, end2},
		"first pass denies, second ends first":             {begin1, deny, begin2, end2, end1},
		"deny then a concurrent realize must not clear it": {begin1, deny, begin2, realize, end2, end1},
		"deny after the other pass began and realized":     {begin1, begin2, realize, deny, end1, end2},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			withJWTBackend(t, &fakeJWTBackend{})
			vs := childVS()
			var p1, p2 *AuthGuardPass
			for _, s := range steps {
				s(&p1, &p2, vs)
			}
			if _, ds := denyNode(vs); ds == nil {
				t.Fatal("a guard added by either overlapping pass must survive both sweeps")
			}
			// A clean pass with nothing overlapping lifts it.
			EndAuthGuardPass(BeginAuthGuardPass("key"), rebuilt(vs))
			if _, ds := denyNode(vs); ds != nil {
				t.Fatal("a later pass that denies nothing must lift the guard")
			}
		})
	}
}

// The same under real concurrency (run with -race): many passes on one key,
// some denying one shared VS, all open before any ends.
func TestAuthGuardPassConcurrentPassesRace(t *testing.T) {
	withJWTBackend(t, &fakeJWTBackend{})
	vs := childVS()
	setAuthRetryHook(func(string, string, time.Duration) {}) // the default capture is not goroutine-safe
	const n = 16
	var opened, denied, wg sync.WaitGroup
	opened.Add(n)
	denied.Add(n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			pass := BeginAuthGuardPass("key")
			opened.Done()
			opened.Wait()
			if i%2 == 0 {
				DenyUnauthenticated("key", vs, "unrealized")
			} else {
				clearAuthUnavailable("key", vs) // another policy on the VS realizing
			}
			denied.Done()
			denied.Wait()
			EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})
		}(i)
	}
	wg.Wait()
	if _, ds := denyNode(vs); ds == nil {
		t.Fatal("a guard added by any concurrent pass must survive every sweep")
	}
	guardPassMu.Lock()
	open := len(activePasses)
	guardPassMu.Unlock()
	if open != 0 {
		t.Errorf("%d passes left open", open)
	}
}

// A pass that is never ended (its rebuild panicked) must not pin guards
// forever: it is dropped once it is older than authGuardPassMaxAge.
func TestAuthGuardPassLeakedPassExpires(t *testing.T) {
	withJWTBackend(t, &fakeJWTBackend{})
	now := testNow
	prev := guardPassNow
	guardPassNow = func() time.Time { return now }
	t.Cleanup(func() { guardPassNow = prev })
	vs := childVS()

	leaked := BeginAuthGuardPass("key")
	DenyUnauthenticated("key", vs, "unrealized")
	_ = leaked // never ended

	now = now.Add(authGuardPassMaxAge + time.Second)
	EndAuthGuardPass(BeginAuthGuardPass("key"), []nodes.AviVsEvhSniModel{vs})
	if _, ds := denyNode(vs); ds != nil {
		t.Fatal("a leaked pass older than authGuardPassMaxAge must no longer protect the guard")
	}
}

// A guard spared only because another pass (still open, or ended and handed its
// record over) holds a deny must get a retry from the sweep: the deny's own
// retry may already have run, and without one the guard would stay after auth
// recovered. A guard this pass itself saw denied is covered by that deny's own
// retry and gets no extra one.
func TestAuthGuardPassSparedByOtherPassSchedulesRetry(t *testing.T) {
	t.Run("held by a pass still open", func(t *testing.T) {
		retries := withJWTBackend(t, &fakeJWTBackend{})
		vs := childVS()

		stuck := BeginAuthGuardPass("key") // e.g. a slow or leaked rebuild
		DenyUnauthenticated("key", vs, "unrealized")
		if len(*retries) != 1 {
			t.Fatalf("precondition: the deny schedules one retry, got %v", *retries)
		}

		// The deny's retry: auth has recovered, but the open pass still holds
		// the record, so the guard is (conservatively) kept.
		pass := BeginAuthGuardPass("key")
		ApplyAuthPolicy("key", jwtPolicy("jwtHeader"), vs, "", "/v1")
		EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})
		if _, ds := denyNode(vs); ds == nil {
			t.Fatal("a guard recorded by an open pass must survive the sweep")
		}
		if len(*retries) != 2 || (*retries)[1] != "ai/llm-route" {
			t.Fatalf("the sparing sweep must schedule a retry of ai/llm-route, got %v", *retries)
		}

		EndAuthGuardPass(stuck, nil)
		// The scheduled retry now runs clean and lifts the guard.
		pass = BeginAuthGuardPass("key")
		ApplyAuthPolicy("key", jwtPolicy("jwtHeader"), vs, "", "/v1")
		EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})
		if _, ds := denyNode(vs); ds != nil {
			t.Fatal("the retry's clean pass must lift the guard")
		}
		if len(*retries) != 2 {
			t.Errorf("a clean pass must not schedule a retry, got %v", *retries)
		}
	})

	t.Run("record handed over by a pass that ended", func(t *testing.T) {
		retries := withJWTBackend(t, &fakeJWTBackend{})
		vs := childVS()

		first := BeginAuthGuardPass("key")
		DenyUnauthenticated("key", vs, "unrealized") // retry #1
		second := BeginAuthGuardPass("key")          // opened after the deny
		EndAuthGuardPass(first, nil)                 // hands its record to second
		EndAuthGuardPass(second, []nodes.AviVsEvhSniModel{vs})
		if _, ds := denyNode(vs); ds == nil {
			t.Fatal("a handed-over record must keep the guard")
		}
		if len(*retries) != 2 {
			t.Fatalf("sparing a guard for a handed-over record must schedule a retry, got %v", *retries)
		}
	})

	t.Run("denied during this pass: no extra retry", func(t *testing.T) {
		retries := withJWTBackend(t, &fakeJWTBackend{})
		vs := childVS()

		pass := BeginAuthGuardPass("key")
		DenyUnauthenticated("key", vs, "unrealized")
		EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})
		if len(*retries) != 1 {
			t.Errorf("only the deny's own retry is expected, got %v", *retries)
		}
	})

	t.Run("protected but unguarded: no retry", func(t *testing.T) {
		retries := withJWTBackend(t, &fakeJWTBackend{})
		vs := childVS()

		stuck := BeginAuthGuardPass("key")
		DenyUnauthenticated("key", vs, "unrealized")
		// A fresh node of the same name (the carried-over one was replaced) has
		// no guard to keep.
		fresh := childVS()
		EndAuthGuardPass(BeginAuthGuardPass("key"), []nodes.AviVsEvhSniModel{fresh})
		EndAuthGuardPass(stuck, nil)
		if len(*retries) != 1 {
			t.Errorf("a VS without the guard needs no retry, got %v", *retries)
		}
	})
}

// An unchanged keyset keeps the AuthProfile description byte-identical, so the
// PUT body does not change on every route event; it is rewritten only when the
// source or keys change, or the recorded time needs advancing or is bogus.
func TestAuthProfileDescriptionChurn(t *testing.T) {
	p := jwtPolicy("jwtHeader")
	src, keys := jwksSourceID(p), jwksKeysID(fakeJWKS)
	fresh := jwksProvenance{Source: src, Keys: keys, Refreshed: testNow}
	storedAt := func(age time.Duration) string {
		return jwksProvenance{Source: src, Keys: keys, Refreshed: testNow.Add(-age)}.description()
	}

	t.Run("fresh fetch, unchanged keyset: stored kept byte-for-byte", func(t *testing.T) {
		stored := storedAt(time.Hour)
		if got := authProfileDescription(stored, fresh, testNow); got != stored {
			t.Fatalf("description changed although nothing did:\n got %q\nwant %q", got, stored)
		}
	})
	t.Run("last-known-good path: stored kept", func(t *testing.T) {
		stored := storedAt(3 * time.Hour)
		lkg, ok := parseJWKSProvenance(stored)
		if !ok {
			t.Fatal("stored provenance does not parse")
		}
		if got := authProfileDescription(stored, lkg, testNow); got != stored {
			t.Fatalf("got %q, want stored %q", got, stored)
		}
	})
	rewrite := map[string]struct {
		stored string
		prov   jwksProvenance
	}{
		"keys rotated":            {storedAt(time.Hour), jwksProvenance{Source: src, Keys: jwksKeysID(`{"keys":[]}`), Refreshed: testNow}},
		"source changed":          {storedAt(time.Hour), jwksProvenance{Source: "other", Keys: keys, Refreshed: testNow}},
		"recorded time too old":   {storedAt(jwksProvenanceRewriteAfter), fresh},
		"recorded time in future": {jwksProvenance{Source: src, Keys: keys, Refreshed: testNow.Add(time.Hour)}.description(), fresh},
		"legacy, no keyset id":    {jwksProvenance{Source: src, Refreshed: testNow.Add(-time.Hour)}.description(), fresh},
		"not a provenance":        {"hand-written", fresh},
		"empty":                   {"", fresh},
	}
	for name, c := range rewrite {
		t.Run(name, func(t *testing.T) {
			if got, want := authProfileDescription(c.stored, c.prov, testNow), c.prov.description(); got != want {
				t.Fatalf("got %q, want the new provenance %q", got, want)
			}
		})
	}
}

// Repeated failures keep exactly one guard, still first.
func TestDenyUnauthenticatedIdempotent(t *testing.T) {
	withJWTBackend(t, &fakeJWTBackend{})
	vs := childVS()
	DenyUnauthenticated("key", vs, "test")
	DenyUnauthenticated("key", vs, "test")
	if len(vs.HTTPDSrefs) != 2 || vs.HTTPDSrefs[0].Name != DSAuthDenyName(vs.Name) {
		t.Errorf("want [guard, model-route], got %v", dsNames(vs))
	}
}

func dsNames(vs *nodes.AviEvhVsNode) []string {
	var out []string
	for _, ds := range vs.HTTPDSrefs {
		out = append(out, ds.Name)
	}
	return out
}

func strPtr(s string) *string { return &s }
