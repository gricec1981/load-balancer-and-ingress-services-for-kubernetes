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

// Native limits must gate (HTTP_REQ) and consume (HTTP_RESP_DATA) in the dedicated
// NativeReqScript/NativeRespDataScript (one combined set), NOT in the per-event
// DataScript-backend scripts — so the gate and consume share a bucket.
func TestNativeBackendScripts(t *testing.T) {
	s := GenerateTokenAccountingScripts(nativeGroupPolicy(), ClaimModeOAuth, "vs-1")

	// Native gate (HTTP_REQ entry of the combined set).
	for _, sub := range []string{
		"avi.vs.ratelimit.exceed(rlname, rk, 1)",
		nativeTokenLimiterName("vs-1", "hourly", "group1"),
		nativeTokenLimiterName("vs-1", "hourly", "group2"),
		"token_budget_exceeded",
		"unknown_group",
	} {
		if !strings.Contains(s.NativeReqScript, sub) {
			t.Errorf("NativeReqScript missing %q:\n%s", sub, s.NativeReqScript)
		}
	}

	// Native consume re-resolves the limiter (no cross-phase reqvar) + consumes
	// total_tokens, and keeps the display counter.
	for _, sub := range []string{
		"avi.vs.ratelimit.exceed(rlname, rk, total_tokens)",
		nativeTokenLimiterName("vs-1", "hourly", "group1"), // re-resolved name map present
		"table_insert", // hybrid display counter retained
	} {
		if !strings.Contains(s.NativeRespDataScript, sub) {
			t.Errorf("NativeRespDataScript missing %q:\n%s", sub, s.NativeRespDataScript)
		}
	}
	// Must NOT depend on a reqvar surviving HTTP_REQ -> HTTP_RESP_DATA.
	if strings.Contains(s.NativeRespDataScript, "get_reqvar(\"ai_tnk") {
		t.Errorf("native consume must re-resolve, not read a cross-phase reqvar:\n%s", s.NativeRespDataScript)
	}

	// The native limit must NOT leak into the DataScript-backend scripts.
	if strings.Contains(s.ReqScript, "ratelimit.exceed") || strings.Contains(s.ReqScript, "table_lookup") {
		t.Errorf("native limit must not appear in the DataScript ReqScript:\n%s", s.ReqScript)
	}
	if strings.Contains(s.RespDataScript, "ratelimit.exceed") {
		t.Errorf("native consume must not appear in the DataScript RespDataScript:\n%s", s.RespDataScript)
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

// Apply puts the native limiters + gate + consume on ONE combined DataScriptSet
// (so the gate and consume share the bucket), and NOT on the per-event sets.
func TestNativeBackendAttachesCombinedSet(t *testing.T) {
	vsNode := &nodes.AviEvhVsNode{Name: "vs-1", Tenant: "admin"}
	ApplyTokenRateLimitPolicy("key", nativeGroupPolicy(), vsNode, ClaimModeOAuth)

	var nativeSet *nodes.AviHTTPDataScriptNode
	byName := map[string]*nodes.AviHTTPDataScriptNode{}
	for _, ds := range vsNode.GetHTTPDSrefs() {
		byName[ds.Name] = ds
		if ds.Name == DSTokNativeName("vs-1") {
			nativeSet = ds
		}
	}

	if nativeSet == nil {
		t.Fatal("expected the combined native DataScript set to be created")
	}
	// Both limiters defined once, on this set.
	if len(nativeSet.RateLimiters) != 2 {
		t.Errorf("combined set: expected 2 limiters, got %d", len(nativeSet.RateLimiters))
	}
	// Primary entry = HTTP_REQ gate; one extra entry = HTTP_RESP_DATA consume.
	if nativeSet.DataScript == nil || nativeSet.DataScript.Evt != DSEvtHTTPReq {
		t.Errorf("combined set primary entry should be HTTP_REQ, got %+v", nativeSet.DataScript)
	}
	if len(nativeSet.ExtraDataScripts) != 1 || nativeSet.ExtraDataScripts[0].Evt != DSEvtHTTPRespData {
		t.Errorf("combined set should carry one HTTP_RESP_DATA extra entry, got %+v", nativeSet.ExtraDataScripts)
	}
	// The per-event sets must NOT carry the native limiters.
	for _, n := range []string{DSReqName("vs-1"), DSRespDataName("vs-1")} {
		if ds := byName[n]; ds != nil && len(ds.RateLimiters) != 0 {
			t.Errorf("per-event set %s should have no native limiters, got %d", n, len(ds.RateLimiters))
		}
	}
}
