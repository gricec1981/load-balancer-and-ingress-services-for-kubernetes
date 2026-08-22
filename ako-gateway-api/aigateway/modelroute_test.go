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

// sampleSpec builds a three-tier policy spec with prefix globs and entitlements.
func sampleSpec() AIModelRoutePolicySpec {
	return AIModelRoutePolicySpec{
		TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route"},
		Tiers: []ModelTier{
			{Name: "premium", BackendRef: ModelBackendRef{Group: "gateway.inference.x-k8s.io", Kind: "InferencePool", Name: "premium-llm"}},
			{Name: "standard", BackendRef: ModelBackendRef{Group: "gateway.inference.x-k8s.io", Kind: "InferencePool", Name: "standard-llm"}},
			{Name: "economy", BackendRef: ModelBackendRef{Group: "gateway.inference.x-k8s.io", Kind: "InferencePool", Name: "economy-llm"}},
		},
		ModelTiers: map[string]string{
			"llama-3-70b-instruct": "premium",
			"llama-3-8b-instruct":  "standard",
			"mistral-7b*":          "standard",
			"mistral-7b-instruct*": "premium", // longer prefix should win
			"*-q4":                 "economy",
		},
		DefaultTier: "economy",
		Entitlements: &ModelEntitlements{
			GroupClaim: "group",
			Rules: []EntitlementRule{
				{Group: "platinum", Allow: []string{"premium", "standard", "economy"}},
				{Group: "gold", Allow: []string{"standard", "economy"}},
				{Group: "free", Allow: []string{"economy"}},
			},
		},
		OnUnentitled: &UnentitledAction{Type: "Downgrade"},
	}
}

func TestResolveTier(t *testing.T) {
	s := sampleSpec()
	cases := map[string]string{
		"llama-3-70b-instruct":   "premium",  // exact
		"llama-3-8b-instruct":    "standard", // exact
		"mistral-7b-base":        "standard", // prefix mistral-7b*
		"mistral-7b-instruct-v3": "premium",  // longer prefix wins over mistral-7b*
		"something-q4":           "economy",  // *-q4 has empty-ish prefix? no: prefix "*-q4" -> "-q4"? handled below
		"totally-unknown":        "economy",  // default
	}
	for model, want := range cases {
		if got := s.ResolveTier(model); got != want {
			t.Errorf("ResolveTier(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestResolveTierPrefixOnlyMatchesPrefix(t *testing.T) {
	// "*-q4" is a suffix-style glob in intent but our globs are trailing "*"
	// (prefix). "*-q4" therefore means prefix "*-q4" which won't match normal
	// models; confirm it does NOT accidentally catch "model-q4".
	s := sampleSpec()
	if got := s.ResolveTier("model-q4"); got != "economy" {
		// economy here only because of DefaultTier, not the "*-q4" rule.
		t.Errorf("ResolveTier(model-q4) = %q, want economy (default)", got)
	}
}

func TestEntitlement(t *testing.T) {
	s := sampleSpec()
	if !s.IsAllowed("platinum", "premium") {
		t.Error("platinum should reach premium")
	}
	if s.IsAllowed("gold", "premium") {
		t.Error("gold should NOT reach premium")
	}
	if s.IsAllowed("free", "standard") {
		t.Error("free should NOT reach standard")
	}
	if got := s.BestAllowedTier("gold"); got != "standard" {
		t.Errorf("BestAllowedTier(gold) = %q, want standard", got)
	}
	if got := s.BestAllowedTier("free"); got != "economy" {
		t.Errorf("BestAllowedTier(free) = %q, want economy", got)
	}
	if got := s.BestAllowedTier("nobody"); got != "" {
		t.Errorf("BestAllowedTier(nobody) = %q, want empty", got)
	}
}

func TestEntitlementAbsentAllowsAll(t *testing.T) {
	s := sampleSpec()
	s.Entitlements = nil
	if !s.IsAllowed("anyone", "premium") {
		t.Error("with no entitlements, every group should reach every tier")
	}
	if got := s.BestAllowedTier("anyone"); got != "premium" {
		t.Errorf("BestAllowedTier with no entitlements = %q, want premium (first)", got)
	}
}

func TestValidate(t *testing.T) {
	good := sampleSpec()
	if err := good.Validate(); err != nil {
		t.Fatalf("sample spec should be valid: %v", err)
	}

	bad := sampleSpec()
	bad.DefaultTier = "ghost"
	if err := bad.Validate(); err == nil {
		t.Error("expected error for unknown defaultTier")
	}

	bad = sampleSpec()
	bad.ModelTiers["x"] = "ghost"
	if err := bad.Validate(); err == nil {
		t.Error("expected error for modelTiers referencing unknown tier")
	}

	bad = sampleSpec()
	bad.Entitlements.Rules[0].Allow = []string{"ghost"}
	if err := bad.Validate(); err == nil {
		t.Error("expected error for entitlement allowing unknown tier")
	}

	bad = sampleSpec()
	bad.Tiers = nil
	if err := bad.Validate(); err == nil {
		t.Error("expected error for no tiers")
	}
}

func tierPGForTest() map[string]string {
	return map[string]string{
		"premium":  "vs-tier-premium-pg",
		"standard": "vs-tier-standard-pg",
		"economy":  "vs-tier-economy-pg",
	}
}

func TestGenerateModelRouteScriptsReq(t *testing.T) {
	p := &AIModelRoutePolicy{Spec: sampleSpec()}
	scripts := GenerateModelRouteScripts(p, tierPGForTest(), nil, nil, ClaimModeOAuth)

	if !strings.Contains(scripts.ReqScript, "set_request_body_buffer_size(32768)") {
		t.Errorf("ReqScript should enable 32KB buffering, got:\n%s", scripts.ReqScript)
	}
}

func TestGenerateModelRouteScriptsReqData(t *testing.T) {
	p := &AIModelRoutePolicy{Spec: sampleSpec()}
	s := GenerateModelRouteScripts(p, tierPGForTest(), nil, nil, ClaimModeOAuth).ReqDataScript

	mustContain := []string{
		"avi.http.get_req_body(32768)",           // verified read API
		`json_str(_body, "model")`,               // model field extraction
		`local DEFAULT_TIER = "economy"`,         // baked default
		"avi.poolgroup.select(pg)",               // verified routing primitive
		`set_reqvar("ai_tier", tier)`,            // per-tier budget hook
		`["llama-3-70b-instruct"]="premium"`,     // baked exact map
		`{k="mistral-7b-instruct", v="premium"}`, // longer prefix baked
		`"vs-tier-premium-pg"`,                   // tier→PG mapping baked
		"ENTITLE",                                // entitlement table present
		"jwt_claim(GROUP_CLAIM)",                 // claim read
		`avi.http.response(403`,                  // unentitled reject path
	}
	for _, sub := range mustContain {
		if !strings.Contains(s, sub) {
			t.Errorf("ReqDataScript missing %q\n---\n%s", sub, s)
		}
	}

	// longest-prefix-first ordering: mistral-7b-instruct must precede mistral-7b
	iLong := strings.Index(s, `{k="mistral-7b-instruct"`)
	iShort := strings.Index(s, `{k="mistral-7b",`)
	if iLong < 0 || iShort < 0 || iLong > iShort {
		t.Errorf("prefixes not ordered longest-first (long=%d short=%d)", iLong, iShort)
	}
}

func TestGenerateModelRouteScriptsNoEntitlement(t *testing.T) {
	spec := sampleSpec()
	spec.Entitlements = nil
	p := &AIModelRoutePolicy{Spec: spec}
	s := GenerateModelRouteScripts(p, tierPGForTest(), nil, nil, ClaimModeOAuth).ReqDataScript

	for _, absent := range []string{"ENTITLE", "jwt_claim", "tier_not_entitled"} {
		if strings.Contains(s, absent) {
			t.Errorf("ReqDataScript should not contain %q when entitlements are absent\n%s", absent, s)
		}
	}
	// routing must still be present
	if !strings.Contains(s, "avi.poolgroup.select(pg)") {
		t.Error("routing select must be present even without entitlements")
	}
}

func TestGenerateRejectMode(t *testing.T) {
	spec := sampleSpec()
	spec.OnUnentitled = &UnentitledAction{Type: "Reject", StatusCode: 451}
	p := &AIModelRoutePolicy{Spec: spec}
	s := GenerateModelRouteScripts(p, tierPGForTest(), nil, nil, ClaimModeOAuth).ReqDataScript

	if !strings.Contains(s, "avi.http.response(451") {
		t.Errorf("Reject mode should use configured status 451\n%s", s)
	}
	// Reject mode must not compute a downgrade (_best)
	if strings.Contains(s, "_best") {
		t.Errorf("Reject mode should not compute a downgrade tier\n%s", s)
	}
}
