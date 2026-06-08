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
	"fmt"
	"sort"
	"strings"
)

// The WAF is the SE-native, regex-capable engine for DLP/guardrails (the SE Lua
// sandbox has no regex). AKO authors an Avi WafPolicy whose pre_crs_groups carry
// the generated SecRules and attaches it to the route VS (waf_policy_ref). Verified
// on Avi 31.2.2 + 32.1.1 (see docs/gateway-api/ai-gateway-guardrails.md).

// guardrailRuleGroupName is the pre-CRS group AKO owns inside the WafPolicy.
const guardrailRuleGroupName = "ai-guardrail"

// guardrailRuleIDBase is the start of the custom WAF rule-id range (kept clear of
// the OWASP CRS rule ids).
const guardrailRuleIDBase = 4000000

// ─── Built-in signature library (AKO-maintained) ─────────────────────────────

// builtinSecretSignatures maps a secret-detector name to its @rx regex.
var builtinSecretSignatures = map[string]string{
	"aws-access-key": `AKIA[0-9A-Z]{16}`,
	"gcp-api-key":    `AIza[0-9A-Za-z_-]{35}`,
	"openai-api-key": `sk-[A-Za-z0-9]{20,}`,
	"github-token":   `gh[pousr]_[A-Za-z0-9]{36}`,
	"slack-token":    `xox[baprs]-[0-9A-Za-z-]{10,}`,
	"private-key":    `-----BEGIN [A-Z ]+PRIVATE KEY-----`,
	"jwt":            `eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+`,
}

// builtinPIISignatures maps a PII-detector name to its @rx regex.
var builtinPIISignatures = map[string]string{
	"ssn":         `[0-9]{3}-[0-9]{2}-[0-9]{4}`,
	"credit-card": `[0-9]{13,16}`,
	"email":       `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
}

// promptInjectionRules is the built-in prompt-injection signature set. Patterns
// are lowercase and matched with t:lowercase (case-insensitive). They catch
// *known phrases*; semantic/paraphrased injection needs a model (out of scope).
var promptInjectionRules = []struct{ name, regex string }{
	{"ignore-instructions", `(ignore|disregard|forget) (all |the |any )?(previous|prior|above|earlier) (instruction|instructions|prompt|prompts)`},
	{"jailbreak", `(dan mode|developer mode|jailbreak|jailbroken|do anything now)`},
	{"reveal-system-prompt", `(reveal|show|print|repeat|expose) (your |the |me your )?(system prompt|system message|hidden instruction|instructions above)`},
	{"override-safety", `(override|bypass|turn off|disable) (your |all |the )?(safety|guardrail|guardrails|content policy|restrictions|filters)`},
}

// toolAbuseRules is the built-in MCP tool-abuse signature set: command injection,
// path traversal, and SSRF patterns in JSON-RPC tool-call arguments. These are the
// MCP analog of prompt-injection (which is LLM-specific). Patterns are lowercase
// and matched with t:lowercase.
var toolAbuseRules = []struct{ name, regex string }{
	{"command-injection", `(;\s*(rm|cat|curl|wget|sh|bash|nc|chmod|kill)|\|\s*(sh|bash|nc)|&&\s*(rm|curl|wget)|\$\([^)]+\)|/bin/(sh|bash)|nc -e)`},
	{"path-traversal", `(\.\./\.\./|/etc/passwd|/etc/shadow|/root/\.ssh|c:\\windows\\)`},
	{"ssrf", `(169\.254\.169\.254|metadata\.google\.internal|file://|gopher://|dict://)`},
}

// BuiltinSecretNames returns the secret-detector names, sorted (used by the
// block profiles and for deterministic output).
func BuiltinSecretNames() []string {
	out := make([]string, 0, len(builtinSecretSignatures))
	for k := range builtinSecretSignatures {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ─── SecRule generation ──────────────────────────────────────────────────────

// generatedDetector is one detector reduced to a name + regex + case-sensitivity.
type generatedDetector struct {
	name          string
	regex         string
	caseSensitive bool
}

// flatten turns the resolved detectors into an ordered, deterministic list of
// (name, regex) detectors.
func (r ResolvedDetectors) flatten() []generatedDetector {
	var out []generatedDetector
	for _, n := range r.Secrets {
		out = append(out, generatedDetector{"secret-" + n, builtinSecretSignatures[n], true})
	}
	for _, n := range r.PII {
		out = append(out, generatedDetector{"pii-" + n, builtinPIISignatures[n], true})
	}
	if r.PromptInjection {
		for _, p := range promptInjectionRules {
			out = append(out, generatedDetector{"pi-" + p.name, p.regex, false})
		}
	}
	if r.ToolAbuse {
		for _, p := range toolAbuseRules {
			out = append(out, generatedDetector{"tool-" + p.name, p.regex, false})
		}
	}
	if r.Keywords != nil && len(r.Keywords.Match) > 0 {
		// One alternation rule over all keywords (regex-escaped).
		escaped := make([]string, 0, len(r.Keywords.Match))
		for _, k := range r.Keywords.Match {
			escaped = append(escaped, regexEscape(k))
		}
		out = append(out, generatedDetector{"keyword-denylist", "(" + strings.Join(escaped, "|") + ")", r.Keywords.CaseSensitive})
	}
	for _, c := range r.Custom {
		out = append(out, generatedDetector{"custom-" + c.Name, c.Regex, true})
	}
	return out
}

// GenerateGuardrailRules builds the WafPolicy rule maps for the resolved
// detectors. Each detector becomes a request-phase rule (phase 2, ARGS|REQUEST_BODY)
// and/or a response-phase rule (phase 4, RESPONSE_BODY) per `inspectReq`/`inspectResp`.
// `block` selects the per-rule mode (ENFORCEMENT blocks, DETECTION_ONLY logs).
func GenerateGuardrailRules(r ResolvedDetectors, inspectReq, inspectResp bool, block bool, statusCode int) []map[string]interface{} {
	mode := "WAF_MODE_DETECTION_ONLY"
	if block {
		mode = "WAF_MODE_ENFORCEMENT"
	}
	var rules []map[string]interface{}
	id := guardrailRuleIDBase
	idx := 0
	add := func(name, target, secRule string) {
		rules = append(rules, map[string]interface{}{
			"index":  idx,
			"name":   name,
			"enable": true,
			"mode":   mode,
			"rule":   secRule,
		})
		idx++
	}
	for _, d := range r.flatten() {
		transforms := "t:none"
		if !d.caseSensitive {
			transforms = "t:none,t:lowercase"
		}
		if inspectReq {
			id++
			add(d.name+"-req", "ARGS|REQUEST_BODY",
				secRule(id, "ARGS|REQUEST_BODY", 2, d.regex, "guardrail "+d.name+" (request)", block, statusCode, transforms))
		}
		if inspectResp {
			id++
			add(d.name+"-resp", "RESPONSE_BODY",
				secRule(id, "RESPONSE_BODY", 4, d.regex, "guardrail "+d.name+" (response)", block, statusCode, transforms))
		}
	}
	return rules
}

// secRule renders one ModSecurity SecRule string. The action is `deny` (block) or
// `pass` (log only) — the per-rule mode set on the WafRule also gates enforcement,
// so this is belt-and-suspenders for the Log case.
func secRule(id int, target string, phase int, regex, msg string, block bool, statusCode int, transforms string) string {
	action := "pass"
	statusPart := ""
	if block {
		action = "deny"
		statusPart = fmt.Sprintf(",status:%d", statusCode)
	}
	return fmt.Sprintf(`SecRule %s "@rx %s" "id:%d,phase:%d,%s%s,msg:'%s',log,%s"`,
		target, regex, id, phase, action, statusPart, msg, transforms)
}

// BuildGuardrailWafPolicyBody assembles the Avi WafPolicy REST body. The policy
// mode is DETECTION_ONLY with allow_mode_delegation so the per-rule ENFORCEMENT
// rules block while any referenced CRS (waf_crs_ref) stays non-blocking — the
// isolation pattern verified in the guardrail spikes.
func BuildGuardrailWafPolicyBody(name, tenantRef, profileRef, crsRef string, rules []map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{
		"name":                  name,
		"mode":                  "WAF_MODE_DETECTION_ONLY",
		"allow_mode_delegation": true,
		"pre_crs_groups": []interface{}{
			map[string]interface{}{
				"name":   guardrailRuleGroupName,
				"enable": true,
				"index":  0,
				"rules":  rules,
			},
		},
	}
	if tenantRef != "" {
		body["tenant_ref"] = tenantRef
	}
	if profileRef != "" {
		body["waf_profile_ref"] = profileRef
	}
	if crsRef != "" {
		body["waf_crs_ref"] = crsRef
	}
	return body
}

// regexEscape escapes PCRE metacharacters in a literal keyword.
func regexEscape(s string) string {
	const meta = `\.+*?()|[]{}^$`
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(meta, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
