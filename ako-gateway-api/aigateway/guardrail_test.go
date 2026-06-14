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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResolveBlockProfile(t *testing.T) {
	s := AIGuardrailPolicySpec{Profile: ProfileBlockLLMAndMCP}
	r := s.Resolve()
	if len(r.Secrets) == 0 || !contains(r.Secrets, "aws-access-key") || !contains(r.Secrets, "openai-api-key") {
		t.Errorf("BlockLLMAndMCP should include the secret library: %v", r.Secrets)
	}
	if !contains(r.PII, "ssn") || !contains(r.PII, "credit-card") {
		t.Errorf("BlockLLMAndMCP should include core PII: %v", r.PII)
	}
	if !r.PromptInjection {
		t.Error("BlockLLMAndMCP should enable prompt injection")
	}
}

func TestLLMandMCPProfilesAreDistinct(t *testing.T) {
	llm := (&AIGuardrailPolicySpec{Profile: ProfileBlockLLM}).Resolve()
	mcp := (&AIGuardrailPolicySpec{Profile: ProfileBlockMCP}).Resolve()

	// Both share the DLP core.
	if !contains(llm.Secrets, "aws-access-key") || !contains(mcp.Secrets, "aws-access-key") {
		t.Error("both profiles should carry the secret DLP core")
	}
	// LLM = prompt-injection, not tool-abuse.
	if !llm.PromptInjection || llm.ToolAbuse {
		t.Errorf("BlockLLM should have promptInjection only: pi=%v tool=%v", llm.PromptInjection, llm.ToolAbuse)
	}
	// MCP = tool-abuse, not prompt-injection.
	if !mcp.ToolAbuse || mcp.PromptInjection {
		t.Errorf("BlockMCP should have toolAbuse only: pi=%v tool=%v", mcp.PromptInjection, mcp.ToolAbuse)
	}
	// The generated rule sets must be unique (matched on the stable rule name in msg).
	llmRules := rulesString(GenerateGuardrailRules(llm, true, false, true, 403))
	mcpRules := rulesString(GenerateGuardrailRules(mcp, true, false, true, 403))
	if !strings.Contains(llmRules, "pi-ignore-instructions") || strings.Contains(llmRules, "tool-command-injection") {
		t.Errorf("BlockLLM rules should contain prompt-injection, not tool-abuse:\n%s", llmRules)
	}
	if !strings.Contains(mcpRules, "tool-command-injection") || strings.Contains(mcpRules, "pi-ignore-instructions") {
		t.Errorf("BlockMCP rules should contain tool-abuse, not prompt-injection:\n%s", mcpRules)
	}
}

func TestResolveMergesProfileAndExplicit(t *testing.T) {
	s := AIGuardrailPolicySpec{
		Profile:   ProfileBlockLLMAndMCP,
		Detectors: &GuardrailDetectors{PII: []string{"email", "ssn"}, Custom: []GuardrailCustomRule{{Name: "emp", Regex: "EMP-[0-9]{6}"}}},
	}
	r := s.Resolve()
	if !contains(r.PII, "email") {
		t.Error("explicit email detector should be merged in")
	}
	// ssn appears in both profile and explicit — must be de-duped
	if count(r.PII, "ssn") != 1 {
		t.Errorf("ssn should be de-duplicated, got %v", r.PII)
	}
	if len(r.Custom) != 1 || r.Custom[0].Name != "emp" {
		t.Errorf("custom detector should be present: %v", r.Custom)
	}
}

func TestGuardrailValidate(t *testing.T) {
	good := AIGuardrailPolicySpec{Profile: ProfileBlockLLMAndMCP}
	if err := good.Validate(); err != nil {
		t.Fatalf("block profile should validate: %v", err)
	}
	bad := AIGuardrailPolicySpec{Profile: "Nope"}
	if err := bad.Validate(); err == nil {
		t.Error("unknown profile should fail")
	}
	bad = AIGuardrailPolicySpec{Detectors: &GuardrailDetectors{Secrets: []string{"not-a-secret"}}}
	if err := bad.Validate(); err == nil {
		t.Error("unknown secret detector should fail")
	}
	empty := AIGuardrailPolicySpec{}
	if err := empty.Validate(); err == nil {
		t.Error("empty policy (no profile/detectors) should fail")
	}
}

func TestGenerateGuardrailRulesRequest(t *testing.T) {
	s := AIGuardrailPolicySpec{Profile: ProfileBlockLLMAndMCP}
	rules := GenerateGuardrailRules(s.Resolve(), true, false, true, 403)
	if len(rules) == 0 {
		t.Fatal("expected rules")
	}
	blob := rulesString(rules)
	for _, sub := range []string{
		`@rx AKIA[0-9A-Z]{16}`,               // aws secret
		`@rx [0-9]{3}-[0-9]{2}-[0-9]{4}`,     // ssn
		`(ignore|disregard|forget|override)`, // prompt injection
		`ARGS|REQUEST_BODY`,                  // request target
		`!ARGS:jwt`,                          // jwtQuery auth token excluded from WAF inspection
		`phase:2`,                            // request phase
		`deny`,                               // block action
		`WAF_MODE_ENFORCEMENT`,               // per-rule enforce mode
		`t:lowercase`,                        // PI case-insensitive
		`t:removeWhitespace`,                 // hardening: defeats "i g n o r e" spacing
		`t:base64Decode`,                     // hardening: defeats base64-encoded injection
		`t:urlDecodeUni`,                     // hardening: defeats %-encoding / unicode
		`role-injection`,                     // role/delimiter family ([system], <|im_start|>)
	} {
		if !strings.Contains(blob, sub) {
			t.Errorf("request rules missing %q", sub)
		}
	}
	// No response targets when inspectResp=false.
	if strings.Contains(blob, "RESPONSE_BODY") {
		t.Error("should not emit response rules when response inspection off")
	}
}

func TestGenerateGuardrailRulesResponseAndLog(t *testing.T) {
	s := AIGuardrailPolicySpec{
		Detectors: &GuardrailDetectors{PII: []string{"ssn"}},
		Action:    &GuardrailAction{Type: "Log"},
	}
	rules := GenerateGuardrailRules(s.Resolve(), true, true, false, 403)
	blob := rulesString(rules)
	if !strings.Contains(blob, "RESPONSE_BODY") || !strings.Contains(blob, "phase:4") {
		t.Error("expected a response-phase rule")
	}
	if !strings.Contains(blob, "WAF_MODE_DETECTION_ONLY") || strings.Contains(blob, ",deny,") {
		t.Errorf("Log mode should detect (pass), not deny:\n%s", blob)
	}
}

func TestBuildWafPolicyBody(t *testing.T) {
	rules := GenerateGuardrailRules((&AIGuardrailPolicySpec{Profile: ProfileBlockLLMAndMCP}).Resolve(), true, false, true, 403)
	body := BuildGuardrailWafPolicyBody("ns-name-ai-guardrail", "/api/tenant/?name=admin",
		"/api/wafprofile/wafprofile-x", "/api/wafcrs/wafcrs-y", rules)

	if body["mode"] != "WAF_MODE_DETECTION_ONLY" || body["allow_mode_delegation"] != true {
		t.Errorf("policy must be detection+delegation (so CRS won't false-positive, custom rules enforce): %v", body["mode"])
	}
	if body["waf_profile_ref"] != "/api/wafprofile/wafprofile-x" || body["waf_crs_ref"] != "/api/wafcrs/wafcrs-y" {
		t.Error("refs not set")
	}
	groups, ok := body["pre_crs_groups"].([]interface{})
	if !ok || len(groups) != 1 {
		t.Fatalf("expected one pre_crs_group")
	}
	g := groups[0].(map[string]interface{})
	if g["name"] != guardrailRuleGroupName {
		t.Errorf("group name = %v", g["name"])
	}
}

func TestUnstructuredToGuardrailPolicy(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "llm-guardrails", "namespace": "inference"},
		"spec": map[string]interface{}{
			"targetRef": map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "llm-route"},
			"profile":   "BlockLLMAndMCP",
			"inspect":   map[string]interface{}{"request": true, "response": true},
			"detectors": map[string]interface{}{
				"pii":             []interface{}{"email"},
				"promptInjection": true,
				"keywords":        map[string]interface{}{"match": []interface{}{"CONFIDENTIAL"}},
				"custom":          []interface{}{map[string]interface{}{"name": "emp", "regex": "EMP-[0-9]{6}"}},
			},
			"action": map[string]interface{}{"type": "Block", "statusCode": int64(403)},
		},
	}}
	p, err := unstructuredToGuardrailPolicy(u)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if p.Name != "llm-guardrails" || p.Spec.TargetRef.Name != "llm-route" || p.Spec.Profile != "BlockLLMAndMCP" {
		t.Errorf("metadata/targetRef/profile not parsed: %+v", p.Spec)
	}
	if !p.Spec.EffectiveInspectResponse() {
		t.Error("response inspection not parsed")
	}
	if p.Spec.Detectors == nil || !p.Spec.Detectors.PromptInjection ||
		!contains(p.Spec.Detectors.PII, "email") ||
		p.Spec.Detectors.Keywords == nil || len(p.Spec.Detectors.Custom) != 1 {
		t.Errorf("detectors not parsed: %+v", p.Spec.Detectors)
	}
	if err := p.Spec.Validate(); err != nil {
		t.Errorf("parsed policy should validate: %v", err)
	}
}

// helpers
func rulesString(rules []map[string]interface{}) string {
	var b strings.Builder
	for _, r := range rules {
		b.WriteString(r["rule"].(string))
		b.WriteString(" | mode=")
		b.WriteString(r["mode"].(string))
		b.WriteString("\n")
	}
	return b.String()
}
func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
func count(ss []string, s string) int {
	n := 0
	for _, x := range ss {
		if x == s {
			n++
		}
	}
	return n
}
