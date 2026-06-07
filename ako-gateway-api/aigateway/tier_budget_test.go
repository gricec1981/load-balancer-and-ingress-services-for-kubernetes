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

// tierBudgetPolicy is a token policy whose budget ceiling depends on the routed
// tier, selected via the ai_tier reqvar that an AIModelRoutePolicy sets.
func tierBudgetPolicy() *AITokenRateLimitPolicy {
	return &AITokenRateLimitPolicy{
		Spec: AITokenRateLimitPolicySpec{
			TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route"},
			Limits: []TokenLimit{
				{
					Name:        "per-tier-budget",
					Key:         "consumer",
					Tokens:      "total",
					Window:      "1h",
					GroupHeader: "reqvar:ai_tier",
					GroupBudgets: map[string]int64{
						"premium": 500,
						"economy": 100000,
					},
					Budget: 0, // unknown tier -> 403
				},
			},
		},
	}
}

func TestTierBudgetEnforcedInReqData(t *testing.T) {
	s := GenerateTokenAccountingScripts(tierBudgetPolicy())

	if s.ReqDataEnforceScript == "" {
		t.Fatal("expected a non-empty ReqDataEnforceScript for a reqvar-keyed limit")
	}
	// The tier is read from the reqvar (set by model routing), not a JWT claim.
	if !strings.Contains(s.ReqDataEnforceScript, `avi.http.get_reqvar("ai_tier")`) {
		t.Errorf("ReqDataEnforceScript should read the ai_tier reqvar:\n%s", s.ReqDataEnforceScript)
	}
	// Per-tier budget table and the budget compare must be in the req-data script.
	for _, sub := range []string{`["premium"]=500`, `["economy"]=100000`, "if cur >= budget"} {
		if !strings.Contains(s.ReqDataEnforceScript, sub) {
			t.Errorf("ReqDataEnforceScript missing %q:\n%s", sub, s.ReqDataEnforceScript)
		}
	}
	// The reqvar limit must NOT be enforced in the HTTP_REQ script (ai_tier isn't
	// set there yet).
	if strings.Contains(s.ReqScript, "get_reqvar(\"ai_tier\")") {
		t.Errorf("HTTP_REQ script must not enforce the tier limit (ai_tier not set yet):\n%s", s.ReqScript)
	}
	// Identity + helper must be present in the req-data script so it runs standalone.
	for _, sub := range []string{"local function jwt_claim", "local identity"} {
		if !strings.Contains(s.ReqDataEnforceScript, sub) {
			t.Errorf("ReqDataEnforceScript missing standalone prerequisite %q", sub)
		}
	}
}

func TestNonReqvarLimitUnchanged(t *testing.T) {
	// A classic per-consumer budget (no reqvar) must keep enforcing in HTTP_REQ and
	// produce no req-data enforcement script — i.e. existing behaviour is unchanged.
	p := &AITokenRateLimitPolicy{
		Spec: AITokenRateLimitPolicySpec{
			TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route"},
			Limits: []TokenLimit{
				{Name: "hourly", Key: "consumer", Tokens: "total", Window: "1h", Budget: 1000},
			},
		},
	}
	s := GenerateTokenAccountingScripts(p)
	if s.ReqDataEnforceScript != "" {
		t.Errorf("non-reqvar policy should not produce a ReqDataEnforceScript:\n%s", s.ReqDataEnforceScript)
	}
	if !strings.Contains(s.ReqScript, "if cur >= 1000") {
		t.Errorf("classic limit should still enforce in HTTP_REQ:\n%s", s.ReqScript)
	}
}

func TestMixedLimitsSplitAcrossEvents(t *testing.T) {
	// One classic limit (HTTP_REQ) + one tier limit (HTTP_REQ_DATA) on the same policy.
	p := tierBudgetPolicy()
	p.Spec.Limits = append(p.Spec.Limits, TokenLimit{
		Name: "global", Key: "consumer", Tokens: "total", Window: "1h", Budget: 200000,
	})
	s := GenerateTokenAccountingScripts(p)

	if !strings.Contains(s.ReqScript, "if cur >= 200000") {
		t.Errorf("classic limit should enforce in HTTP_REQ:\n%s", s.ReqScript)
	}
	if !strings.Contains(s.ReqDataEnforceScript, `avi.http.get_reqvar("ai_tier")`) {
		t.Errorf("tier limit should enforce in HTTP_REQ_DATA:\n%s", s.ReqDataEnforceScript)
	}
	// Accounting (response phase) must still cover BOTH limits.
	for _, name := range []string{"per-tier-budget", "global"} {
		if !strings.Contains(s.RespDataScript, "account: "+name) {
			t.Errorf("RespDataScript should account for limit %q", name)
		}
	}
}

func TestGroupReadExpr(t *testing.T) {
	if got := groupReadExpr("reqvar:ai_tier"); !strings.Contains(got, `avi.http.get_reqvar("ai_tier")`) {
		t.Errorf("reqvar source should read get_reqvar, got: %s", got)
	}
	if got := groupReadExpr("group"); !strings.Contains(got, `pcall(jwt_claim, "group")`) {
		t.Errorf("claim source should read jwt_claim, got: %s", got)
	}
}
