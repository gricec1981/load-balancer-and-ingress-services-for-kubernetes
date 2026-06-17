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

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
)

// A native-backend per-group token limit, mirroring the demo llm-limits policy.
func nativeGroupPolicy() *AITokenRateLimitPolicy {
	return &AITokenRateLimitPolicy{
		Spec: AITokenRateLimitPolicySpec{
			TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route"},
			Limits: []TokenLimit{{
				Name:         "hourly",
				Backend:      "native",
				Key:          "consumer",
				Tokens:       "total",
				Window:       "1h",
				GroupHeader:  "group",
				GroupBudgets: map[string]int64{"group1": 2347, "group2": 4500},
				Budget:       0, // unknown group -> reject
				Action:       &LimitAction{Type: "Reject", StatusCode: 429, RetryAfter: true},
			}},
		},
	}
}

func TestNativeTokenLimiterObjects(t *testing.T) {
	rls := buildNativeTokenLimiters(nativeGroupPolicy().Spec.Limits[0], "vs-1")
	// group1 + group2 (Budget 0 => no fallback limiter)
	if len(rls) != 2 {
		t.Fatalf("expected 2 limiters (group1, group2), got %d", len(rls))
	}
	byName := map[string]uint32{}
	for _, rl := range rls {
		if rl.Name == nil || rl.Count == nil || rl.Period == nil || rl.BurstSz == nil {
			t.Fatalf("limiter has nil fields: %+v", rl)
		}
		if *rl.Period != 3600 {
			t.Errorf("period = %d, want 3600 (1h)", *rl.Period)
		}
		if *rl.BurstSz != *rl.Count {
			t.Errorf("burst_sz %d != count %d", *rl.BurstSz, *rl.Count)
		}
		byName[*rl.Name] = *rl.Count
	}
	if c := byName[nativeTokenLimiterName("vs-1", "hourly", "group1")]; c != 2347 {
		t.Errorf("group1 count = %d, want 2347", c)
	}
	if c := byName[nativeTokenLimiterName("vs-1", "hourly", "group2")]; c != 4500 {
		t.Errorf("group2 count = %d, want 4500", c)
	}
}

// Budget > 0 with groups adds an unknown-group fallback limiter.
func TestNativeTokenLimiterFallback(t *testing.T) {
	p := nativeGroupPolicy()
	p.Spec.Limits[0].Budget = 1000
	rls := buildNativeTokenLimiters(p.Spec.Limits[0], "vs-1")
	if len(rls) != 3 {
		t.Fatalf("expected 3 limiters (group1, group2, fallback), got %d", len(rls))
	}
	found := false
	for _, rl := range rls {
		if *rl.Name == nativeTokenLimiterName("vs-1", "hourly", nativeTokFallbackGroup) {
			found = true
			if *rl.Count != 1000 {
				t.Errorf("fallback count = %d, want 1000", *rl.Count)
			}
		}
	}
	if !found {
		t.Error("expected an unknown-group fallback limiter")
	}
}

// The generated scripts must gate (request) and consume (response) against the
// native limiter, and keep the DataScript display counter.
func TestNativeBackendScripts(t *testing.T) {
	s := GenerateTokenAccountingScripts(nativeGroupPolicy(), ClaimModeOAuth, "vs-1")

	// Request phase: native gate, per-group limiter map, reject 429, stash reqvar.
	for _, sub := range []string{
		"avi.vs.ratelimit.exceed(rlname, rk, 1)",
		nativeTokenLimiterName("vs-1", "hourly", "group1"),
		nativeTokenLimiterName("vs-1", "hourly", "group2"),
		`avi.http.set_reqvar("ai_tnk_hourly"`,
		"token_budget_exceeded",
	} {
		if !strings.Contains(s.ReqScript, sub) {
			t.Errorf("ReqScript missing %q:\n%s", sub, s.ReqScript)
		}
	}
	// Unknown group with Budget 0 -> reject.
	if !strings.Contains(s.ReqScript, "unknown_group") {
		t.Errorf("ReqScript should reject unknown group (Budget 0):\n%s", s.ReqScript)
	}
	// Must NOT use the per-SE table counter to *enforce* in the request phase.
	if strings.Contains(s.ReqScript, "table_lookup") {
		t.Errorf("native gate must not enforce via table_lookup:\n%s", s.ReqScript)
	}

	// Response phase: native consume of total_tokens + display table still present.
	for _, sub := range []string{
		"avi.vs.ratelimit.exceed(rlname, rk, total_tokens)",
		`avi.http.get_reqvar("ai_tnk_hourly")`,
		"table_insert", // hybrid display counter retained
	} {
		if !strings.Contains(s.RespDataScript, sub) {
			t.Errorf("RespDataScript missing %q:\n%s", sub, s.RespDataScript)
		}
	}
}

// A datascript-backend limit is unchanged (no native calls).
func TestDataScriptBackendUnchanged(t *testing.T) {
	p := nativeGroupPolicy()
	p.Spec.Limits[0].Backend = "" // default
	s := GenerateTokenAccountingScripts(p, ClaimModeOAuth, "vs-1")
	if strings.Contains(s.ReqScript, "ratelimit.exceed") {
		t.Errorf("datascript backend must not emit ratelimit.exceed:\n%s", s.ReqScript)
	}
	if !strings.Contains(s.ReqScript, "table_lookup") {
		t.Errorf("datascript backend should still enforce via table_lookup:\n%s", s.ReqScript)
	}
}

// Apply attaches the native token limiters to the request AND response-data nodes
// (so the gate and consume reference the same bucket).
func TestNativeBackendAttachesLimitersToReqAndRespData(t *testing.T) {
	vsNode := &nodes.AviEvhVsNode{Name: "vs-1", Tenant: "admin"}
	ApplyTokenRateLimitPolicy("key", nativeGroupPolicy(), vsNode, ClaimModeOAuth)

	want := map[string]bool{
		DSReqName("vs-1"):      false,
		DSRespDataName("vs-1"): false,
	}
	for _, ds := range vsNode.GetHTTPDSrefs() {
		if _, ok := want[ds.Name]; ok {
			if len(ds.RateLimiters) != 2 {
				t.Errorf("node %s: expected 2 native limiters, got %d", ds.Name, len(ds.RateLimiters))
			}
			want[ds.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("expected DataScript node %s to be created with limiters", name)
		}
	}
}
