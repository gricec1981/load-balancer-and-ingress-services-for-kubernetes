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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// streamingPolicy is the counters fixture plus a native limit, so a reservation
// has both kinds of key to land in, with the streaming block set as given.
func streamingPolicy(st *StreamingPolicy) *AITokenRateLimitPolicy {
	p := countersPolicy()
	p.Spec.Limits = append(p.Spec.Limits, TokenLimit{
		Name: "native-hourly", Backend: "native", Key: "consumer", Tokens: "total", Budget: 1000, Window: "1h",
	})
	p.Spec.Streaming = st
	return p
}

// TestStreamingScriptsAgainstSEStub executes the generated phases in the SE
// sandbox stub. See testdata/streaming_spec.lua for what each scenario proves.
func TestStreamingScriptsAgainstSEStub(t *testing.T) {
	lua, err := exec.LookPath("lua5.3")
	if err != nil {
		if lua, err = exec.LookPath("lua"); err != nil {
			t.Skip("no lua interpreter on PATH; skipping SE-sandbox execution test")
		}
	}

	dir := os.Getenv("AIGW_STREAM_DUMP_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s := GenerateTokenAccountingScripts(streamingPolicy(nil), ClaimModeJWTQuery, "vs-test")
	write("req.lua", s.ReqScript)
	write("native_req.lua", s.NativeReqScript)
	write("reqdata.lua", s.ReqDataEnforceScript)
	write("resp.lua", s.RespScript)
	write("respdata.lua", s.RespDataScript)
	write("native_respdata.lua", s.NativeRespDataScript)

	d := GenerateTokenAccountingScripts(streamingPolicy(&StreamingPolicy{DefaultMaxTokens: 256}), ClaimModeJWTQuery, "vs-test")
	write("reqdata_default.lua", d.ReqDataEnforceScript)
	n := GenerateTokenAccountingScripts(streamingPolicy(&StreamingPolicy{Mode: StreamingModeDeny}), ClaimModeJWTQuery, "vs-test")
	write("reqdata_deny.lua", n.ReqDataEnforceScript)

	for _, f := range []string{"se_stub.lua", "streaming_spec.lua"} {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		write(f, string(b))
	}

	out, err := exec.Command(lua, filepath.Join(dir, "streaming_spec.lua"), dir).CombinedOutput()
	if err != nil {
		t.Fatalf("SE-sandbox streaming spec failed: %v\n%s", err, out)
	}
	t.Log(string(out))
}

// The source-level contract: what each mode emits, and where.
func TestStreamingGeneration(t *testing.T) {
	for _, mode := range []AuthClaimMode{ClaimModeOAuth, ClaimModeJWTQuery, ClaimModeJWTHeader} {
		// Reserve (the default when the block is absent).
		s := GenerateTokenAccountingScripts(streamingPolicy(nil), mode, "vs-test")
		if !strings.Contains(s.ReqScript, "set_request_body_buffer_size(32768)") {
			t.Errorf("mode %v: Reserve must buffer the request head in HTTP_REQ", mode)
		}
		for _, sub := range []string{`'"stream"'`, `'"max_tokens"'`, `'"max_completion_tokens"'`, "max_tokens_required",
			`set_reqvar("ai_stream", "1")`, `set_reqvar("ai_skip_meter", "1")`, "tkcarry:native-hourly"} {
			if !strings.Contains(s.ReqDataEnforceScript, sub) {
				t.Errorf("mode %v: HTTP_REQ_DATA script missing %q", mode, sub)
			}
		}
		// The reservation must come AFTER the tier gates, so a rejected request
		// reserves nothing — and the tier gate here is none, so just the order of
		// the shared header matters: identity before the block that keys on it.
		if strings.Index(s.ReqDataEnforceScript, "local identity") > strings.Index(s.ReqDataEnforceScript, "streamed request") {
			t.Errorf("mode %v: identity must be resolved before the reservation", mode)
		}
		// The response block must precede the skip guard, because a reserved
		// stream carries ai_skip_meter.
		at, guard := strings.Index(s.RespScript, "streamed response"), strings.Index(s.RespScript, `get_reqvar("ai_skip_meter") == "1" then return end`)
		if at < 0 || guard < 0 || at > guard {
			t.Errorf("mode %v: the stream block (%d) must precede the skip guard (%d)", mode, at, guard)
		}
		for _, sub := range []string{`"estimated"`, `"penalty"`, "release: native-hourly", "release: hourly-group-budget", "text/event-stream"} {
			if !strings.Contains(s.RespScript, sub) {
				t.Errorf("mode %v: HTTP_RESP script missing %q", mode, sub)
			}
		}
		// An unknown avi.* field raises on access, so every optional call is a closure.
		if strings.Contains(s.ReqDataEnforceScript, "pcall(avi.vs.log") || strings.Contains(s.RespScript, "pcall(avi.vs.log") {
			t.Errorf("mode %v: avi.vs.log must be called inside a closure", mode)
		}
	}

	// Allow emits none of it and leaves the request head unbuffered.
	a := GenerateTokenAccountingScripts(streamingPolicy(&StreamingPolicy{Mode: "allow"}), ClaimModeJWTQuery, "vs-test")
	if strings.Contains(a.ReqScript, "set_request_body_buffer_size") {
		t.Error("Allow must not buffer the request head")
	}
	if a.ReqDataEnforceScript != "" {
		t.Errorf("Allow must emit no HTTP_REQ_DATA script for a policy without tier limits, got:\n%s", a.ReqDataEnforceScript)
	}
	if strings.Contains(a.RespScript, "streamed response") {
		t.Error("Allow must not emit the response-side stream block")
	}

	// Deny answers 400 and never reserves.
	d := GenerateTokenAccountingScripts(streamingPolicy(&StreamingPolicy{Mode: "Deny"}), ClaimModeJWTQuery, "vs-test")
	if !strings.Contains(d.ReqDataEnforceScript, "streaming_not_allowed") {
		t.Error("Deny must answer streaming_not_allowed")
	}
	if strings.Contains(d.ReqDataEnforceScript, "tkcarry") {
		t.Error("Deny must not charge a reservation")
	}

	// A default ceiling is baked in verbatim.
	f := GenerateTokenAccountingScripts(streamingPolicy(&StreamingPolicy{DefaultMaxTokens: 512, PromptCharsPerToken: 3}), ClaimModeJWTQuery, "vs-test")
	if !strings.Contains(f.ReqDataEnforceScript, "_max = 512") || strings.Contains(f.ReqDataEnforceScript, "max_tokens_required") {
		t.Error("defaultMaxTokens must replace the max_tokens_required refusal")
	}
	if !strings.Contains(f.ReqDataEnforceScript, "math.floor(#_rb / 3)") {
		t.Error("promptCharsPerToken must be the estimate divisor")
	}
}

// The CR shape the operator writes must round-trip, and an unknown mode must
// land on the side that charges.
func TestStreamingParse(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "ai.ako.vmware.com/v1alpha1", "kind": "AITokenRateLimitPolicy",
		"metadata": map[string]interface{}{"name": "llm-limits", "namespace": "inference"},
		"spec": map[string]interface{}{
			"targetRef": map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "llm-route"},
			"limits": []interface{}{map[string]interface{}{
				"name": "l", "key": "consumer", "budget": int64(10), "window": "1h"}},
			"streaming": map[string]interface{}{"mode": "Deny", "defaultMaxTokens": int64(300), "promptCharsPerToken": int64(3)},
		},
	}}
	p, err := unstructuredToTokenRateLimitPolicy(obj)
	if err != nil {
		t.Fatal(err)
	}
	st := p.Spec.EffectiveStreaming()
	if st.Mode != StreamingModeDeny || st.DefaultMaxTokens != 300 || st.PromptCharsPerToken != 3 {
		t.Errorf("parsed streaming = %+v", st)
	}

	obj.Object["spec"].(map[string]interface{})["streaming"] = map[string]interface{}{"mode": "sometimes"}
	p, _ = unstructuredToTokenRateLimitPolicy(obj)
	if p.Spec.EffectiveStreaming().Mode != StreamingModeReserve {
		t.Errorf("an unknown mode must resolve to Reserve, got %q", p.Spec.EffectiveStreaming().Mode)
	}

	delete(obj.Object["spec"].(map[string]interface{}), "streaming")
	p, _ = unstructuredToTokenRateLimitPolicy(obj)
	if got := p.Spec.EffectiveStreaming(); got.Mode != StreamingModeReserve || got.DefaultMaxTokens != 0 || got.PromptCharsPerToken != 4 {
		t.Errorf("absent block must default to Reserve / require max_tokens / 4 chars per token, got %+v", got)
	}
}
