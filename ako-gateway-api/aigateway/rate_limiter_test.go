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

func rpsPolicy(rps, burst int, key string) *AITokenRateLimitPolicy {
	return &AITokenRateLimitPolicy{
		Spec: AITokenRateLimitPolicySpec{
			TargetRef:        PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route"},
			RequestRateLimit: &RequestRateLimit{RequestsPerSecond: rps, Burst: burst, Key: key},
		},
	}
}

// The request-rate script must use the native rate limiter, not the old per-SE
// soft token bucket.
func TestRequestRateLimitScriptUsesNativeLimiter(t *testing.T) {
	rlName := RPSRateLimiterName("vs-1")
	script := buildRequestRateLimitScript(rpsPolicy(10, 20, "consumer").Spec, rlName)

	if !strings.Contains(script, "avi.vs.ratelimit.exceed(\""+rlName+"\"") {
		t.Errorf("script should call ratelimit.exceed against %q:\n%s", rlName, script)
	}
	// Consumer keying: prefer the identity header, fall back to client IP.
	if !strings.Contains(script, `avi.http.get_header("x-ai-consumer", avi.HTTP_REQUEST) or avi.vs.client_ip()`) {
		t.Errorf("consumer key should resolve identity header then client IP:\n%s", script)
	}
	if !strings.Contains(script, "avi.http.response(429") {
		t.Errorf("script should reject with 429 on exceed:\n%s", script)
	}
	// The fragile soft-bucket primitives must be gone.
	for _, gone := range []string{"table_insert", "table_remove", "table_lookup", "os.time()"} {
		if strings.Contains(script, gone) {
			t.Errorf("native script must not use soft-bucket primitive %q:\n%s", gone, script)
		}
	}
}

func TestRequestRateLimitScriptClientIPKey(t *testing.T) {
	script := buildRequestRateLimitScript(rpsPolicy(5, 0, "clientIP").Spec, RPSRateLimiterName("vs-1"))
	if !strings.Contains(script, "local rk = avi.vs.client_ip()") {
		t.Errorf("clientIP key should resolve to client_ip():\n%s", script)
	}
	if strings.Contains(script, "get_header") {
		t.Errorf("clientIP key must not read a header:\n%s", script)
	}
}

func TestBuildRequestRateLimiterObject(t *testing.T) {
	rl := buildRequestRateLimiter(rpsPolicy(10, 20, "consumer").Spec, "vs-1-ai-rps")
	if rl.Name == nil || *rl.Name != "vs-1-ai-rps" {
		t.Errorf("rate limiter name = %v, want vs-1-ai-rps", rl.Name)
	}
	if rl.Count == nil || *rl.Count != 10 {
		t.Errorf("count = %v, want 10", rl.Count)
	}
	if rl.Period == nil || *rl.Period != 1 {
		t.Errorf("period = %v, want 1", rl.Period)
	}
	if rl.BurstSz == nil || *rl.BurstSz != 20 {
		t.Errorf("burst = %v, want 20", rl.BurstSz)
	}
}

// Burst defaults to the sustained count when unset/zero.
func TestBuildRequestRateLimiterBurstDefault(t *testing.T) {
	rl := buildRequestRateLimiter(rpsPolicy(7, 0, "consumer").Spec, "n")
	if rl.BurstSz == nil || *rl.BurstSz != 7 {
		t.Errorf("burst should default to count (7), got %v", rl.BurstSz)
	}
}

// The rate limiter object is attached to the request-phase DataScript node, and
// its name matches the name the script references.
func TestApplyAttachesRateLimiterToReqNode(t *testing.T) {
	vsNode := &nodes.AviEvhVsNode{Name: "vs-1", Tenant: "admin"}
	ApplyTokenRateLimitPolicy("key", rpsPolicy(10, 20, "consumer"), vsNode, ClaimModeOAuth)

	var reqNode *nodes.AviHTTPDataScriptNode
	for _, ds := range vsNode.GetHTTPDSrefs() {
		if ds.Name == DSReqName("vs-1") {
			reqNode = ds
		}
	}
	if reqNode == nil {
		t.Fatal("expected a request-phase DataScript node")
	}
	if len(reqNode.RateLimiters) != 1 {
		t.Fatalf("expected exactly one rate limiter on the req node, got %d", len(reqNode.RateLimiters))
	}
	rlName := RPSRateLimiterName("vs-1")
	if reqNode.RateLimiters[0].Name == nil || *reqNode.RateLimiters[0].Name != rlName {
		t.Errorf("attached limiter name = %v, want %q", reqNode.RateLimiters[0].Name, rlName)
	}
	if !strings.Contains(reqNode.Script, "avi.vs.ratelimit.exceed(\""+rlName+"\"") {
		t.Errorf("req script should reference the attached limiter %q:\n%s", rlName, reqNode.Script)
	}
}

// A token-budget-only policy (no requestRateLimit) attaches no rate limiter.
func TestApplyTokenOnlyHasNoRateLimiter(t *testing.T) {
	vsNode := &nodes.AviEvhVsNode{Name: "vs-2", Tenant: "admin"}
	p := &AITokenRateLimitPolicy{
		Spec: AITokenRateLimitPolicySpec{
			TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route"},
			Limits:    []TokenLimit{{Name: "hourly", Key: "consumer", Tokens: "total", Window: "1h", Budget: 1000}},
		},
	}
	ApplyTokenRateLimitPolicy("key", p, vsNode, ClaimModeOAuth)

	for _, ds := range vsNode.GetHTTPDSrefs() {
		if len(ds.RateLimiters) != 0 {
			t.Errorf("token-only policy should attach no rate limiter, node %s has %d", ds.Name, len(ds.RateLimiters))
		}
	}
}
