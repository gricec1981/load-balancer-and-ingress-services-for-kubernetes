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
	// Generic `sk-`+20-alphanumeric token (covers OpenAI legacy sk-<48> and newer
	// sk-proj-/sk-svcacct- formats, but also any other system's hyphenated sk-
	// prefixed token). Named generically on purpose: a match is NOT necessarily an
	// OpenAI key, so the detector name (and the generated rule msg) must not claim it is.
	"generic-sk-token": `sk-(?:proj-|svcacct-)?[A-Za-z0-9]{20,}`,
	// Classic PATs: ghp_/gho_/ghu_/ghs_/ghr_ + 36 alphanum chars.
	"github-token": `gh[pousr]_[A-Za-z0-9]{36}`,
	// Fine-grained PATs (2022+): github_pat_ + ~82 alphanum/underscore chars.
	"github-fine-grained-pat": `github_pat_[A-Za-z0-9_]{82,}`,
	"slack-token":             `xox[baprs]-[0-9A-Za-z-]{10,}`,
	// Require a matching END marker within a bounded window so a bare header (a
	// partial paste, or pure meta-discussion of PEM formats — "what does a private
	// key header look like") does NOT match; only a real BEGIN...body...END block
	// does. The `(?:(?!-----END).){0,4096}` guard is a standard PCRE negative
	// lookahead in a bounded repetition (Avi's WAF engine is PCRE-compatible, so
	// this is fine); it's less obviously readable than the other flat signatures,
	// hence this note. Go's stdlib regexp (RE2) can't compile lookaheads, so the
	// unit test asserts on the generated SecRule text / an RE2-equivalent stand-in.
	"private-key": `-----BEGIN [A-Z ]+PRIVATE KEY-----(?:(?!-----END).){0,4096}-----END [A-Z ]+PRIVATE KEY-----`,
	"jwt":         `eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+`,
}

// builtinPIISignatures maps a PII-detector name to its @rx regex.
var builtinPIISignatures = map[string]string{
	// Boundary-anchored + SSN validity ranges (exclude area 000/666/900-999, group 00,
	// serial 0000). A bare 3-2-4 digit pattern matches phone/order/ID numbers and any
	// digit run; anchoring + range exclusions cut most of those false positives.
	"ssn": `\b(?!000|666|9[0-9]{2})[0-9]{3}-(?!00)[0-9]{2}-(?!0000)[0-9]{4}\b`,
	// IIN-prefix anchored pattern covering Visa, Mastercard, Amex, Diners, Discover.
	// A bare \d{13,16} produces excessive false positives on any numeric sequence;
	// encoding the known IIN prefixes eliminates most noise without a Luhn check
	// (which requires runtime logic unavailable in a WAF regex).
	"credit-card": `\b(?:4[0-9]{12}(?:[0-9]{3})?|5[1-5][0-9]{14}|3[47][0-9]{13}|3(?:0[0-5]|[68][0-9])[0-9]{11}|6(?:011|5[0-9]{2})[0-9]{12})\b`,
	"email":       `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
}

// promptInjectionRules is the built-in prompt-injection signature set. These are
// "hardened" detectors (§GenerateGuardrailRules): they emit two SecRules each — a
// normalised pass (lowercase + unicode/url-decode + whitespace removal, so
// "I g n o r e", zero-width tricks and %-encoding are caught) and a base64-decode
// pass (so base64-encoded injections are caught). Patterns are therefore written
// WITHOUT spaces (whitespace is stripped before matching) and use bounded `.{0,N}`
// gaps for filler words. They still catch only *known* phrasings; semantic /
// paraphrased injection needs a model (an ICAP classifier — out of scope here).
//
// False-positive tiering: several of these terms are common in ordinary tech /
// support language and should never hard-block by default. Each such rule is split
// into a SPECIFIC tier (AI-specific tokens — follows the caller's action, i.e.
// Block by default) and a GENERIC tier (terms too common to block — forceLog:true,
// ALWAYS emitted at WAF_MODE_DETECTION_ONLY / Log regardless of the policy action).
// Both tiers keep the hardened evasion-resistant transform passes.
var promptInjectionRules = []struct {
	name     string
	regex    string
	forceLog bool // true → always detection-only (Log), overriding the caller's Block action.
}{
	// "disregard the above instruction, use Celsius" is a normal utterance — this
	// whole detector is inherently conversational/ambiguous, so the WHOLE thing is
	// always-Log (not split further).
	{"ignore-instructions", `(ignore|disregard|forget|override).{0,12}(previous|prior|above|earlier|preceding|all).{0,12}(instruction|prompt|direction|rule|guideline)`, true},

	// jailbreak — specific tier: unambiguous AI-jailbreak tokens (Block).
	{"jailbreak", `(doanythingnow|danmode|unfilteredmode|withoutanyfilter|withoutrestriction)`, false},
	// jailbreak — generic tier: common in non-AI tech contexts (phone jailbreaking,
	// dev-mode toggles), so Log only.
	{"jailbreak-generic", `(developermode|jailbreak|jailbroken)`, true},

	// reveal-system-prompt — not flagged as a FP source; left intact (Block).
	{"reveal-system-prompt", `(reveal|show|print|repeat|expose|leak).{0,12}(system.{0,4}prompt|system.{0,4}message|initial.{0,4}prompt|hidden.{0,4}instruction)`, false},

	// override-safety — specific tier: the verb group + AI-safety nouns (Block).
	{"override-safety", `(override|bypass|turnoff|disable|switchoff).{0,12}(safety|guardrail|contentpolic)`, false},
	// override-safety — generic tier: the same verb group + generic IT nouns that
	// are common outside any AI/safety context, so Log only.
	{"override-safety-generic", `(override|bypass|turnoff|disable|switchoff).{0,12}(restriction|filter|moderation)`, true},

	// role-injection — specific tier: chat-template delimiters / role tokens (Block).
	{"role-injection", `(\[system\]|<\|im_start\|>|<\|im_end\|>|<system>|</system>|beginsystemprompt|endofprompt)`, false},
	// role-injection — generic tier: ordinary Markdown headers ("### system", "###
	// instruction") are extremely common — the single biggest FP driver here — so Log only.
	{"role-injection-generic", `(###(system|instruction))`, true},
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
// hardened detectors (prompt-injection) emit evasion-resistant rule variants.
type generatedDetector struct {
	name          string
	regex         string
	caseSensitive bool
	hardened      bool
	// forceLog pins this detector to WAF_MODE_DETECTION_ONLY (Log) regardless of the
	// caller's Block action — used for generic prompt-injection terms that are too
	// common in ordinary language to hard-block by default.
	forceLog bool
}

// flatten turns the resolved detectors into an ordered, deterministic list of
// (name, regex) detectors.
func (r ResolvedDetectors) flatten() []generatedDetector {
	var out []generatedDetector
	for _, n := range r.Secrets {
		out = append(out, generatedDetector{name: "secret-" + n, regex: builtinSecretSignatures[n], caseSensitive: true})
	}
	for _, n := range r.PII {
		out = append(out, generatedDetector{name: "pii-" + n, regex: builtinPIISignatures[n], caseSensitive: true})
	}
	if r.PromptInjection {
		// Prompt-injection patterns are hardened (evasion-resistant variants, §GenerateGuardrailRules).
		for _, p := range promptInjectionRules {
			out = append(out, generatedDetector{name: "pi-" + p.name, regex: p.regex, hardened: true, forceLog: p.forceLog})
		}
	}
	if r.ToolAbuse {
		for _, p := range toolAbuseRules {
			out = append(out, generatedDetector{name: "tool-" + p.name, regex: p.regex})
		}
	}
	if r.Keywords != nil && len(r.Keywords.Match) > 0 {
		// One alternation rule over all keywords (regex-escaped). \b(...)\b anchors
		// on word boundaries so "internal" doesn't match inside "internally". When
		// case-insensitive the input is matched under t:lowercase (see
		// transformChains), so lowercase each keyword BEFORE escaping so the pattern
		// lines up with the transformed input; otherwise the pattern never fires.
		escaped := make([]string, 0, len(r.Keywords.Match))
		for _, k := range r.Keywords.Match {
			if !r.Keywords.CaseSensitive {
				k = strings.ToLower(k)
			}
			escaped = append(escaped, regexEscape(k))
		}
		out = append(out, generatedDetector{name: "keyword-denylist", regex: `\b(` + strings.Join(escaped, "|") + `)\b`, caseSensitive: r.Keywords.CaseSensitive})
	}
	for _, c := range r.Custom {
		out = append(out, generatedDetector{name: "custom-" + c.Name, regex: c.Regex, caseSensitive: true})
	}
	return out
}

// transformChain is one ModSecurity transformation pipeline + a rule-name suffix.
type transformChain struct {
	suffix     string
	transforms string
}

// transformChains returns the SecRule variants a detector emits. Hardened
// (prompt-injection) detectors get an evasion-resistant normalised pass
// (lowercase + url/unicode-decode + whitespace removal, multiMatch so each step is
// tested) plus a base64-decode pass — defeating casing, spacing ("i g n o r e"),
// %-encoding and base64-encoded injections. Everything else gets a single pass.
func transformChains(d generatedDetector) []transformChain {
	if d.hardened {
		return []transformChain{
			{"", "multiMatch,t:none,t:urlDecodeUni,t:lowercase,t:removeWhitespace"},
			{"-b64", "t:base64Decode,t:lowercase,t:removeWhitespace"},
		}
	}
	if !d.caseSensitive {
		return []transformChain{{"", "t:none,t:lowercase"}}
	}
	return []transformChain{{"", "t:none"}}
}

// GenerateGuardrailRules builds the WafPolicy rule maps for the resolved
// detectors. Each detector becomes a request-phase rule (phase 2, ARGS|REQUEST_BODY)
// and/or a response-phase rule (phase 4, RESPONSE_BODY) per `inspectReq`/`inspectResp`.
// `block` selects the per-rule mode (ENFORCEMENT blocks, DETECTION_ONLY logs).
func GenerateGuardrailRules(r ResolvedDetectors, inspectReq, inspectResp bool, block bool, statusCode int) []map[string]interface{} {
	var rules []map[string]interface{}
	id := guardrailRuleIDBase
	idx := 0
	add := func(name, mode, secRule string) {
		rules = append(rules, map[string]interface{}{
			"index":  idx,
			"name":   name,
			"enable": true,
			"mode":   mode,
			"rule":   secRule,
		})
		idx++
	}
	// Request-phase rules scan ARGS, which includes query-string args. The jwtQuery
	// auth mode carries the bearer token in the ?jwt= query param; excluding that arg
	// (!ARGS:<param>) keeps the WAF from inspecting — and blocking — the opaque token,
	// so guardrails and jwtQuery auth coexist on the same route. The token is
	// SE-validated separately; its bytes are never user prompt content.
	reqTarget := "ARGS|REQUEST_BODY|!ARGS:" + JwtQueryParamName
	for _, d := range r.flatten() {
		// A detector may pin itself to Log (detection-only) regardless of the policy's
		// Block action — generic prompt-injection terms too common to hard-block. This
		// reuses the per-rule mode + pass/deny mechanism (the same isolation trick that
		// lets custom rules enforce while CRS stays detection-only): forceLog flips this
		// rule's mode to DETECTION_ONLY and its action to `pass` even when block=true.
		ruleBlock := block && !d.forceLog
		mode := "WAF_MODE_DETECTION_ONLY"
		if ruleBlock {
			mode = "WAF_MODE_ENFORCEMENT"
		}
		// Hardened detectors emit several variants (different transform pipelines)
		// so casing/spacing/encoding evasions are all caught; others emit one.
		for _, ch := range transformChains(d) {
			if inspectReq {
				id++
				add(d.name+ch.suffix+"-req", mode,
					secRule(id, reqTarget, 2, d.regex, "guardrail "+d.name+" (request)", ruleBlock, statusCode, ch.transforms))
			}
			if inspectResp {
				id++
				add(d.name+ch.suffix+"-resp", mode,
					secRule(id, "RESPONSE_BODY", 4, d.regex, "guardrail "+d.name+" (response)", ruleBlock, statusCode, ch.transforms))
			}
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

// generateCreditCardVerifyCCRule renders an UNVERIFIED credit-card detector that
// Luhn-validates matches, cutting the false positives a plain @rx leaves (any
// IIN-shaped 13–16 digit run — order numbers, IDs — that isn't a real card number).
//
// ⚠️ UNVERIFIED — needs a live-controller spike before it's trusted or made default.
// It uses ModSecurity's native @verifyCC (Luhn) operator via the standard OWASP-CRS
// capture-and-chain idiom: rule 1 anchors on the IIN regex WITH `capture` and `chain`;
// the chained rule 2 runs `@verifyCC \d{13,16}` on the captured value (TX:0) and only
// then does the chain's disruptive action fire. `WafRule.Rule` is "Rule as per Modsec
// language" (vendored alb-sdk WafRule model), so ModSecurity operators *should* be
// usable — BUT this depends on whether Avi's underlying WAF engine build actually
// implements @verifyCC. If it doesn't, the controller will likely reject the rule at
// compile/apply time (non-201). This is intentionally NOT wired into flatten() or any
// default profile (BlockLLM/BlockMCP/BlockLLMAndMCP): the plain @rx credit-card rule
// stays the known-good default; this variant only exists (and is unit-tested for
// SecRule shape) so the generation logic is ready once the operator is confirmed.
func generateCreditCardVerifyCCRule(id, phase int, target string, block bool, statusCode int) string {
	iin := builtinPIISignatures["credit-card"]
	action := "pass"
	statusPart := ""
	if block {
		action = "deny"
		statusPart = fmt.Sprintf(",status:%d", statusCode)
	}
	// Rule 1 (@rx + capture + chain) → Rule 2 (@verifyCC on TX:0). Both directives
	// form one logical (chained) ModSecurity rule carried in a single WafRule.Rule.
	return fmt.Sprintf(
		`SecRule %s "@rx %s" "id:%d,phase:%d,%s%s,msg:'guardrail credit-card-luhn',log,capture,t:none,chain"`+"\n"+
			`    SecRule TX:0 "@verifyCC \d{13,16}" "t:none"`,
		target, iin, id, phase, action, statusPart)
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
