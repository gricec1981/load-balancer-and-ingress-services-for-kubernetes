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
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResolveBlockProfile(t *testing.T) {
	s := AIGuardrailPolicySpec{Profile: ProfileBlockLLMAndMCP}
	r := s.Resolve()
	if len(r.Secrets) == 0 || !contains(r.Secrets, "aws-access-key") || !contains(r.Secrets, "generic-sk-token") {
		t.Errorf("BlockLLMAndMCP should include the secret library: %v", r.Secrets)
	}
	// The generic sk- token detector must NOT be labelled as an OpenAI key anymore.
	if contains(r.Secrets, "openai-api-key") {
		t.Errorf("openai-api-key should have been renamed to generic-sk-token: %v", r.Secrets)
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
		`@rx AKIA[0-9A-Z]{16}`, // aws secret
		`@rx \b(?!000|666|9[0-9]{2})[0-9]{3}-(?!00)[0-9]{2}-(?!0000)[0-9]{4}\b`, // ssn (boundaries + validity ranges)
		`(ignore|disregard|forget|override)`,                                    // prompt injection
		`ARGS|REQUEST_BODY`,                                                     // request target
		`!ARGS:jwt`,                                                             // jwtQuery auth token excluded from WAF inspection
		`phase:2`,                                                               // request phase
		`deny`,                                                                  // block action
		`WAF_MODE_ENFORCEMENT`,                                                  // per-rule enforce mode
		`t:lowercase`,                                                           // PI case-insensitive
		`t:removeWhitespace`,                                                    // hardening: defeats "i g n o r e" spacing
		`t:base64Decode`,                                                        // hardening: defeats base64-encoded injection
		`t:urlDecodeUni`,                                                        // hardening: defeats %-encoding / unicode
		`role-injection`,                                                        // role/delimiter family ([system], <|im_start|>)
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

// keywordDenylistRegex returns the @rx pattern the keyword-denylist detector
// generates for the given keywords, or "" if none was emitted.
func keywordDenylistRegex(t *testing.T, kw *GuardrailKeywords) string {
	t.Helper()
	for _, d := range (ResolvedDetectors{Keywords: kw}).flatten() {
		if d.name == "keyword-denylist" {
			return d.regex
		}
	}
	t.Fatal("no keyword-denylist detector generated")
	return ""
}

func TestKeywordDenylistWordBoundary(t *testing.T) {
	// Bug 1: the alternation must be \b-anchored so a keyword only matches whole
	// words — "internal" must not match inside "internally".
	pat := keywordDenylistRegex(t, &GuardrailKeywords{Match: []string{"internal"}, CaseSensitive: true})
	if pat != `\b(internal)\b` {
		t.Fatalf("expected word-boundary-anchored pattern, got %q", pat)
	}
	// Compile and exercise it the way the WAF would (PCRE \b == Go regexp \b here).
	re := regexp.MustCompile(pat)
	if re.MatchString("internally") {
		t.Error(`pattern should NOT match the substring inside "internally"`)
	}
	if !re.MatchString("internal use only") {
		t.Error(`pattern should match the standalone word in "internal use only"`)
	}
}

func TestKeywordDenylistCaseInsensitiveLowercasesPattern(t *testing.T) {
	// Bug 2: with caseSensitive:false the input is matched under t:lowercase, so the
	// pattern must be lowercased too or it can never fire.
	pat := keywordDenylistRegex(t, &GuardrailKeywords{Match: []string{"CONFIDENTIAL"}, CaseSensitive: false})
	if pat != `\b(confidential)\b` {
		t.Fatalf("case-insensitive pattern should be lowercased to match t:lowercase input, got %q", pat)
	}
	// The lowercased pattern lines up with the lowercased input the transform produces.
	if !regexp.MustCompile(pat).MatchString(strings.ToLower("This is CONFIDENTIAL data")) {
		t.Error("lowercased pattern should match the lowercased input")
	}
	// And the case-insensitive detector still carries the t:lowercase transform.
	blob := rulesString(GenerateGuardrailRules(
		ResolvedDetectors{Keywords: &GuardrailKeywords{Match: []string{"CONFIDENTIAL"}, CaseSensitive: false}},
		true, false, true, 403))
	if !strings.Contains(blob, `@rx \b(confidential)\b`) || !strings.Contains(blob, "t:lowercase") {
		t.Errorf("expected lowercased @rx pattern with t:lowercase transform:\n%s", blob)
	}
}

func TestKeywordDenylistCaseSensitivePreservesCase(t *testing.T) {
	// caseSensitive:true keeps the keyword as typed (no lowercasing), still \b-anchored.
	pat := keywordDenylistRegex(t, &GuardrailKeywords{Match: []string{"CONFIDENTIAL"}, CaseSensitive: true})
	if pat != `\b(CONFIDENTIAL)\b` {
		t.Fatalf("case-sensitive pattern should preserve original case, got %q", pat)
	}
}

func TestPrivateKeyRequiresEndMarker(t *testing.T) {
	// Change 1: the private-key signature must require a matching END marker so a bare
	// header (partial paste / meta-discussion of PEM formats) does NOT match — only a
	// full BEGIN...body...END block does.
	blob := rulesString(GenerateGuardrailRules(
		ResolvedDetectors{Secrets: []string{"private-key"}}, true, false, true, 403))
	// String-content assertion: Go's stdlib regexp (RE2) can't compile the PCRE
	// negative lookahead the shipped pattern uses, so assert on the generated SecRule.
	if !strings.Contains(blob, `-----END [A-Z ]+PRIVATE KEY-----`) {
		t.Errorf("private-key rule must require an END marker:\n%s", blob)
	}
	if !strings.Contains(blob, `(?:(?!-----END).)`) {
		t.Errorf("private-key rule should use the bounded negative-lookahead guard:\n%s", blob)
	}

	// Behavioural check via an RE2-equivalent stand-in: Go can't run the PCRE lookahead
	// form, so translate the bounded "not-END" repetition into a non-greedy dot-all
	// match — identical accept/reject semantics for these two inputs.
	re := regexp.MustCompile(`-----BEGIN [A-Z ]+PRIVATE KEY-----(?s:.*?)-----END [A-Z ]+PRIVATE KEY-----`)
	if re.MatchString("-----BEGIN RSA PRIVATE KEY-----") {
		t.Error("a bare BEGIN header with no END marker should NOT match")
	}
	full := "-----BEGIN RSA PRIVATE KEY-----\nMIIBVAIBADANBgkqhkiG9w0BAQEF...base64...\n-----END RSA PRIVATE KEY-----"
	if !re.MatchString(full) {
		t.Error("a full BEGIN...body...END block should match")
	}
}

func TestPromptInjectionTierSplit(t *testing.T) {
	// Change 3: call with block=true (the caller wants enforcement). Specific-tier PI
	// rules must enforce (Block); generic-tier rules must stay detection-only (Log)
	// regardless of the requested action — they're too common to hard-block by default.
	rules := GenerateGuardrailRules(
		ResolvedDetectors{PromptInjection: true}, true, false, true /* block */, 403)

	specific := []string{
		"pi-jailbreak-req",
		"pi-role-injection-req",
		"pi-override-safety-req",
		"pi-reveal-system-prompt-req", // untouched — stays Block
	}
	generic := []string{
		"pi-ignore-instructions-req", // whole rule moved to always-Log
		"pi-jailbreak-generic-req",
		"pi-role-injection-generic-req",
		"pi-override-safety-generic-req",
	}
	for _, name := range specific {
		if m := modeOf(rules, name); m != "WAF_MODE_ENFORCEMENT" {
			t.Errorf("specific-tier %s should enforce (Block), got mode=%q", name, m)
		}
		if !strings.Contains(secRuleOf(rules, name), ",deny,") {
			t.Errorf("specific-tier %s SecRule should use deny:\n%s", name, secRuleOf(rules, name))
		}
	}
	for _, name := range generic {
		if m := modeOf(rules, name); m != "WAF_MODE_DETECTION_ONLY" {
			t.Errorf("generic-tier %s should be Log/detection-only even under Block, got mode=%q", name, m)
		}
		rule := secRuleOf(rules, name)
		if strings.Contains(rule, ",deny,") {
			t.Errorf("generic-tier %s SecRule must NOT deny:\n%s", name, rule)
		}
		// Generic-tier rules keep the hardened evasion-resistant transform passes.
		if !strings.Contains(rule, "t:removeWhitespace") {
			t.Errorf("generic-tier %s should keep hardened transforms:\n%s", name, rule)
		}
	}
}

func TestPromptInjectionTierRegexBoundaries(t *testing.T) {
	// PI patterns have no lookahead, so Go's regexp can exercise them directly. Inputs
	// are pre-normalised (lowercase, whitespace removed) to mirror the t:lowercase +
	// t:removeWhitespace transform the hardened rules apply before matching.
	pat := func(name string) string {
		for _, d := range (ResolvedDetectors{PromptInjection: true}).flatten() {
			if d.name == name {
				return d.regex
			}
		}
		t.Fatalf("no detector %q", name)
		return ""
	}

	jbSpecific := regexp.MustCompile(pat("pi-jailbreak"))
	jbGeneric := regexp.MustCompile(pat("pi-jailbreak-generic"))
	// "developermode" is a generic term: generic tier matches, specific tier does not.
	if jbSpecific.MatchString("developermode") {
		t.Error("specific jailbreak tier should NOT match the generic term 'developermode'")
	}
	if !jbGeneric.MatchString("developermode") {
		t.Error("generic jailbreak tier should match 'developermode'")
	}
	// "doanythingnow" is AI-specific: specific tier matches.
	if !jbSpecific.MatchString("doanythingnow") {
		t.Error("specific jailbreak tier should match 'doanythingnow'")
	}
	// Benign tech-support text matches neither tier.
	if jbSpecific.MatchString("pleasehelpmedebugmycode") || jbGeneric.MatchString("pleasehelpmedebugmycode") {
		t.Error("benign text should not match any jailbreak tier")
	}

	riSpecific := regexp.MustCompile(pat("pi-role-injection"))
	riGeneric := regexp.MustCompile(pat("pi-role-injection-generic"))
	// A Markdown header "### system" (normalised → "###system") is the big FP driver:
	// the generic tier logs it, the specific tier ignores it.
	if riSpecific.MatchString("###system") {
		t.Error("specific role-injection tier should NOT match a markdown '###system' header")
	}
	if !riGeneric.MatchString("###system") {
		t.Error("generic role-injection tier should match '###system'")
	}
	// A real chat-template delimiter still hits the specific (Block) tier.
	if !riSpecific.MatchString("<|im_start|>") {
		t.Error("specific role-injection tier should match '<|im_start|>'")
	}
}

func TestCreditCardVerifyCCChainGeneration(t *testing.T) {
	// ⚠️ Change 4 — shape-only test. Asserts the generated chain's SecRule text has the
	// right `chain` action and `@verifyCC` operator syntax. Actual Luhn behaviour can't
	// be tested without a live WAF (Go's regexp has no @verifyCC), and Avi's WAF-engine
	// support for @verifyCC is UNVERIFIED — this rule is intentionally NOT wired into any
	// default profile and needs a live-controller spike before use.
	rule := generateCreditCardVerifyCCRule(guardrailRuleIDBase+900, 2, "ARGS|REQUEST_BODY", true, 403)
	for _, sub := range []string{
		"@rx ",         // rule 1: IIN-anchored candidate match
		"capture",      // captures the candidate into TX:0
		"chain",        // links to the validation rule
		"SecRule TX:0", // rule 2 operates on the captured value
		"@verifyCC ",   // ModSecurity Luhn operator
		`\d{13,16}`,    // verifyCC candidate shape
		"deny",         // block action when block=true
		"status:403",
	} {
		if !strings.Contains(rule, sub) {
			t.Errorf("verifyCC chain missing %q:\n%s", sub, rule)
		}
	}
	// Two chained directives: chain on rule 1, a second SecRule for the validation.
	if strings.Count(rule, "SecRule ") != 2 {
		t.Errorf("expected a 2-directive chained rule:\n%s", rule)
	}
	// Not wired into defaults: BlockLLMAndMCP must NOT emit the @verifyCC variant.
	blob := rulesString(GenerateGuardrailRules(
		(&AIGuardrailPolicySpec{Profile: ProfileBlockLLMAndMCP}).Resolve(), true, false, true, 403))
	if strings.Contains(blob, "verifyCC") || strings.Contains(blob, "credit-card-luhn") {
		t.Errorf("the @verifyCC variant must NOT be wired into default profiles:\n%s", blob)
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

// modeOf returns the WAF mode of the generated rule with the exact given name.
func modeOf(rules []map[string]interface{}, name string) string {
	for _, r := range rules {
		if r["name"].(string) == name {
			return r["mode"].(string)
		}
	}
	return ""
}

// secRuleOf returns the SecRule text of the generated rule with the exact given name.
func secRuleOf(rules []map[string]interface{}, name string) string {
	for _, r := range rules {
		if r["name"].(string) == name {
			return r["rule"].(string)
		}
	}
	return ""
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
