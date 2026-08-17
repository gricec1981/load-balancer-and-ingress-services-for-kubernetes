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
	"strings"
	"testing"
)

// countersPolicy is a minimal token policy with the read-only counters endpoint
// enabled (i.e. an admin-token Secret resolved onto the policy).
func countersPolicy() *AITokenRateLimitPolicy {
	return &AITokenRateLimitPolicy{
		AdminToken:   "s3cr3t",
		CounterEpoch: "7",
		Spec: AITokenRateLimitPolicySpec{
			TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route"},
			Limits: []TokenLimit{{
				Name: "hourly-group-budget", Key: "consumer", Tokens: "total", Window: "1h",
				GroupHeader:  "group",
				GroupBudgets: map[string]int64{"group1": 500},
				Budget:       0,
			}},
		},
	}
}

// The counters endpoint calls jwt_claim when claim-gating is configured. Lua
// resolves a `local function` only from its declaration onward, so the helper
// must be emitted BEFORE the counters block or the call hits a nil global at
// request time — a failure that only appears on the admin path, at runtime.
func TestCountersEndpointFollowsJWTClaimHelper(t *testing.T) {
	for _, mode := range []AuthClaimMode{ClaimModeOAuth, ClaimModeJWTQuery} {
		p := countersPolicy()
		p.AdminClaimName, p.AdminClaimValue = "scope", "counters:read"
		req := GenerateTokenAccountingScripts(p, mode).ReqScript

		helperAt := strings.Index(req, "local function jwt_claim")
		countersAt := strings.Index(req, "/v1/admin/counters")
		if helperAt < 0 {
			t.Fatalf("mode %v: jwt_claim helper not emitted", mode)
		}
		if countersAt < 0 {
			t.Fatalf("mode %v: counters endpoint not emitted", mode)
		}
		if helperAt > countersAt {
			t.Errorf("mode %v: jwt_claim defined at %d, after the counters block at %d — "+
				"the claim gate would call a nil global", mode, helperAt, countersAt)
		}
		if n := strings.Count(req, "local function jwt_claim"); n != 1 {
			t.Errorf("mode %v: jwt_claim emitted %d times, want exactly 1", mode, n)
		}
	}
}

// Without the annotation the gate must stay exactly as it was: shared header
// only, no claim check. This is the rollback state, so it is worth pinning.
func TestCountersGateDefaultsToHeaderOnly(t *testing.T) {
	req := GenerateTokenAccountingScripts(countersPolicy(), ClaimModeJWTQuery).ReqScript

	if !strings.Contains(req, `avi.http.get_header("X-Admin-Token", avi.HTTP_REQUEST) == "s3cr3t"`) {
		t.Error("admin-token header gate missing")
	}
	// jwt_claim is legitimately used later for the group budget, so scope the
	// assertion to the counters block itself.
	block := req[strings.Index(req, "/v1/admin/counters"):]
	if end := strings.Index(block, "-- AKO AI Gateway: token-budget enforcement"); end > 0 {
		block = block[:end]
	}
	if strings.Contains(block, "pcall(jwt_claim") {
		t.Error("counters block claim-gates without the annotation set")
	}
	if strings.Contains(block, "if not _authed then\n    end") {
		t.Error("dead empty if-block emitted when no claim is configured")
	}
}

// With the annotation set BOTH credentials are accepted, which is what makes the
// migration reversible: callers move one at a time.
func TestCountersGateAcceptsHeaderOrClaim(t *testing.T) {
	p := countersPolicy()
	p.AdminClaimName, p.AdminClaimValue = "scope", "counters:read"
	req := GenerateTokenAccountingScripts(p, ClaimModeJWTQuery).ReqScript

	block := req[strings.Index(req, "/v1/admin/counters"):]
	for _, want := range []string{
		`avi.http.get_header("X-Admin-Token", avi.HTTP_REQUEST) == "s3cr3t"`,
		`pcall(jwt_claim, "scope")`,
		`_cv == "counters:read"`,
		`'{"error":"forbidden"}'`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("counters block missing %q", want)
		}
	}
	// The claim read must be inside pcall: on an unauthenticated request there is
	// no validated token, and a raw error would abort the whole HTTP_REQ script.
	if strings.Contains(block, "= jwt_claim(") {
		t.Error("claim read is not wrapped in pcall")
	}
}

// The end state of the migration: no admin-token Secret at all, claim only. The
// endpoint must still be generated, and no credential may appear in the config.
func TestClaimOnlyKeepsEndpointAndBakesNoSecret(t *testing.T) {
	p := countersPolicy()
	p.AdminToken = "" // Secret annotation removed
	p.AdminClaimName, p.AdminClaimValue = "scope", "counters:read"
	req := GenerateTokenAccountingScripts(p, ClaimModeJWTQuery).ReqScript

	if !strings.Contains(req, "/v1/admin/counters") {
		t.Fatal("endpoint disappeared when the admin token was removed — " +
			"the secret is still acting as the feature's on-switch")
	}
	if strings.Contains(req, "X-Admin-Token") {
		t.Error("header gate emitted with no secret configured")
	}
	// The specific hazard: `get_header(...) == ""` authenticates any caller that
	// sends an empty header, which is worse than the secret it replaces.
	if strings.Contains(req, `avi.http.get_header("X-Admin-Token", avi.HTTP_REQUEST) == ""`) {
		t.Error("empty-string comparison emitted: the gate would accept an empty header")
	}
	if !strings.Contains(req, `pcall(jwt_claim, "scope")`) {
		t.Error("claim gate missing, so the endpoint would be unauthenticated")
	}
	if !strings.Contains(req, `'{"error":"forbidden"}'`) {
		t.Error("reject branch missing")
	}
}

// With neither credential the endpoint must not exist at all — an unauthenticated
// counters endpoint would expose every consumer's usage.
func TestNoCredentialEmitsNoEndpoint(t *testing.T) {
	p := countersPolicy()
	p.AdminToken, p.AdminClaimName, p.AdminClaimValue = "", "", ""
	if req := GenerateTokenAccountingScripts(p, ClaimModeJWTQuery).ReqScript; strings.Contains(req, "/v1/admin/counters") {
		t.Error("counters endpoint emitted with no credential configured")
	}
}

func TestEffectiveAdminSkipPath(t *testing.T) {
	cases := []struct {
		annotation string
		wantPath   string
		wantEmit   bool
	}{
		{"", DefaultAdminSkipPath, true},                   // untouched: historical behaviour
		{"/v1/admin/counters", "/v1/admin/counters", true}, // narrowed
		{"none", "", false},                                // rule dropped: SE validates first
	}
	for _, c := range cases {
		p := &AIGatewayAuthPolicy{AdminSkipPath: c.annotation}
		got, emit := p.EffectiveAdminSkipPath()
		if got != c.wantPath || emit != c.wantEmit {
			t.Errorf("AdminSkipPath=%q: got (%q,%v), want (%q,%v)",
				c.annotation, got, emit, c.wantPath, c.wantEmit)
		}
	}
}
