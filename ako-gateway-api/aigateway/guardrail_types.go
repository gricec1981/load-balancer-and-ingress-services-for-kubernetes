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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─── AIGuardrailPolicy ───────────────────────────────────────────────────────

// AIGuardrailPolicy enforces DLP and content guardrails on AI traffic (inference
// and MCP) by having AKO author an Avi WAF policy from a high-level spec and
// attach it to the targeted route's Virtual Service. Detection runs entirely on
// the Service Engine WAF — no proxy, no sidecar. The signature core blocks
// secrets/PII and known prompt-injection phrases in request (and optionally
// response) bodies; semantic detection is out of scope (see
// docs/gateway-api/ai-gateway-guardrails.md).
type AIGuardrailPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIGuardrailPolicySpec   `json:"spec"`
	Status AIGuardrailPolicyStatus `json:"status,omitempty"`
}

// AIGuardrailPolicySpec is the desired guardrail configuration.
type AIGuardrailPolicySpec struct {
	// TargetRef identifies the HTTPRoute (or Gateway) this policy applies to.
	TargetRef PolicyTargetRef `json:"targetRef"`

	// Profile selects a pre-canned guardrail bundle. "BlockLLMAndMCP" enables the
	// full secrets/PII DLP + prompt-injection signature set for LLM and MCP
	// traffic. When set, its detectors are merged with any explicit Detectors.
	// +optional
	Profile string `json:"profile,omitempty"`

	// Inspect selects which bodies to scan. Request defaults true, Response false.
	// +optional
	Inspect *GuardrailInspect `json:"inspect,omitempty"`

	// Detectors adds explicit detectors on top of (or instead of) the Profile.
	// +optional
	Detectors *GuardrailDetectors `json:"detectors,omitempty"`

	// Action controls what happens on a match.
	// +optional
	Action *GuardrailAction `json:"action,omitempty"`

	// Semantic enables the ICAP classifier layer: the SE calls an out-of-band
	// prompt-injection classifier over ICAP REQMOD and enforces its verdict. This
	// catches novel / paraphrased injection the signature layer provably misses
	// (see docs/gateway-api/ai-gateway-guardrails-semantic.md). AKO authors both
	// the icapprofile AND the HTTP_SECURITY_ACTION_REQUEST_CHECK_ICAP HTTPPolicySet
	// that makes ICAP fire.
	// +optional
	Semantic *GuardrailSemantic `json:"semantic,omitempty"`
}

// GuardrailSemantic configures the ICAP classifier layer.
type GuardrailSemantic struct {
	// Enabled turns the ICAP classifier layer on for this route.
	Enabled bool `json:"enabled,omitempty"`
	// Classifier locates the ICAP server (the classifier + shim Service).
	Classifier *GuardrailClassifier `json:"classifier,omitempty"`
	// Threshold is the injection-score cutoff the shim enforces (default 0.6).
	// Passed to the shim on the icapprofile service_uri query string.
	// +optional
	Threshold *float64 `json:"threshold,omitempty"`
	// FailOpen controls ICAP error/timeout behaviour. true (default) favours
	// availability (allow on ICAP failure); false favours security (deny). Maps to
	// icapprofile.fail_action ICAP_FAIL_OPEN / ICAP_FAIL_CLOSED.
	// +optional
	FailOpen *bool `json:"failOpen,omitempty"`
	// Action is "Block" (default, reject on match) or "Log" (shadow mode: classify
	// and log, don't reject). Passed to the shim as mode=block|log.
	// +optional
	Action string `json:"action,omitempty"`
}

// GuardrailClassifier is a reference to the ICAP server Service. AKO builds the
// Avi Pool from this Service's ready endpoints and authors the icapprofile that
// points at it.
type GuardrailClassifier struct {
	// BackendRef is the ICAP server Service (the classifier shim, port 1344).
	BackendRef GuardrailBackendRef `json:"backendRef"`
}

// GuardrailBackendRef identifies a Kubernetes Service and port.
type GuardrailBackendRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Port      int32  `json:"port,omitempty"`
}

// GuardrailInspect selects request/response body inspection.
type GuardrailInspect struct {
	Request  *bool `json:"request,omitempty"`
	Response *bool `json:"response,omitempty"`
}

// GuardrailDetectors enumerates what to detect.
type GuardrailDetectors struct {
	// Secrets lists built-in secret signatures (keys of the secret library).
	Secrets []string `json:"secrets,omitempty"`
	// PII lists built-in PII signatures.
	PII []string `json:"pii,omitempty"`
	// PromptInjection enables the built-in prompt-injection signature set
	// (LLM-specific: "ignore previous instructions", jailbreak, etc.).
	PromptInjection bool `json:"promptInjection,omitempty"`
	// ToolAbuse enables the built-in tool-abuse signature set (MCP-specific:
	// command injection, path traversal, and SSRF in tool-call arguments).
	ToolAbuse bool `json:"toolAbuse,omitempty"`
	// Keywords is an operator denylist of literal terms (markers / banned topics).
	Keywords *GuardrailKeywords `json:"keywords,omitempty"`
	// Custom is a raw-regex escape hatch.
	Custom []GuardrailCustomRule `json:"custom,omitempty"`
}

// GuardrailKeywords is a literal denylist.
type GuardrailKeywords struct {
	Match         []string `json:"match,omitempty"`
	CaseSensitive bool     `json:"caseSensitive,omitempty"`
}

// GuardrailCustomRule is a raw-regex detector.
type GuardrailCustomRule struct {
	Name  string `json:"name"`
	Regex string `json:"regex"`
}

// GuardrailAction controls the response on a match.
type GuardrailAction struct {
	// Type is "Block" (default — reject) or "Log" (detect-only, shadow mode).
	Type string `json:"type,omitempty"`
	// StatusCode is the HTTP status on Block. Default 403.
	StatusCode int `json:"statusCode,omitempty"`
}

// AIGuardrailPolicyStatus is the observed state.
type AIGuardrailPolicyStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ─── Defaults ────────────────────────────────────────────────────────────────

// Pre-canned profiles. LLM and MCP are intentionally distinct: prompt-injection
// is an LLM-prompt threat, tool-abuse (command injection / path traversal / SSRF
// in tool arguments) is an MCP-tool threat. Both carry the DLP (secrets/PII) core.
const (
	// ProfileBlockLLM — DLP + prompt-injection. For LLM inference routes.
	ProfileBlockLLM = "BlockLLM"
	// ProfileBlockMCP — DLP + tool-abuse. For MCP tool routes.
	ProfileBlockMCP = "BlockMCP"
	// ProfileBlockLLMAndMCP — DLP + prompt-injection + tool-abuse (both surfaces).
	ProfileBlockLLMAndMCP = "BlockLLMAndMCP"
)

// knownProfiles is the set of valid spec.Profile values.
var knownProfiles = map[string]bool{
	ProfileBlockLLM:       true,
	ProfileBlockMCP:       true,
	ProfileBlockLLMAndMCP: true,
}

// EffectiveInspectRequest reports whether to scan the request body (default true).
func (s *AIGuardrailPolicySpec) EffectiveInspectRequest() bool {
	if s.Inspect != nil && s.Inspect.Request != nil {
		return *s.Inspect.Request
	}
	return true
}

// EffectiveInspectResponse reports whether to scan the response body (default false).
func (s *AIGuardrailPolicySpec) EffectiveInspectResponse() bool {
	if s.Inspect != nil && s.Inspect.Response != nil {
		return *s.Inspect.Response
	}
	return false
}

// EffectiveActionType returns "Block" (default) or "Log".
func (s *AIGuardrailPolicySpec) EffectiveActionType() string {
	if s.Action != nil && s.Action.Type != "" {
		return s.Action.Type
	}
	return "Block"
}

// EffectiveStatusCode returns the block status, defaulting to 403.
func (s *AIGuardrailPolicySpec) EffectiveStatusCode() int {
	if s.Action != nil && s.Action.StatusCode >= 400 {
		return s.Action.StatusCode
	}
	return 403
}

// ─── Semantic (ICAP) layer accessors ─────────────────────────────────────────

// SemanticEnabled reports whether the ICAP classifier layer is on.
func (s *AIGuardrailPolicySpec) SemanticEnabled() bool {
	return s.Semantic != nil && s.Semantic.Enabled
}

// SemanticThreshold returns the injection-score cutoff (default 0.6).
func (g *GuardrailSemantic) SemanticThreshold() float64 {
	if g != nil && g.Threshold != nil {
		return *g.Threshold
	}
	return 0.6
}

// SemanticFailOpen returns the ICAP failure behaviour (default true = fail-open).
func (g *GuardrailSemantic) SemanticFailOpen() bool {
	if g != nil && g.FailOpen != nil {
		return *g.FailOpen
	}
	return true
}

// SemanticMode returns "block" (default) or "log" (shadow mode), lower-cased for
// the shim's service_uri query.
func (g *GuardrailSemantic) SemanticMode() string {
	if g != nil && strings.EqualFold(g.Action, "Log") {
		return "log"
	}
	return "block"
}

// ─── Detector resolution (profile + explicit) ────────────────────────────────

// ResolvedDetectors is the concrete detector set after expanding the Profile and
// merging explicit detectors — the input the SecRule generator works from.
type ResolvedDetectors struct {
	Secrets         []string
	PII             []string
	PromptInjection bool
	ToolAbuse       bool
	Keywords        *GuardrailKeywords
	Custom          []GuardrailCustomRule
}

// Resolve expands spec.Profile into concrete detectors and unions them with the
// explicit spec.Detectors (de-duplicated). A profile is additive, not exclusive.
func (s *AIGuardrailPolicySpec) Resolve() ResolvedDetectors {
	var r ResolvedDetectors

	// All profiles share the DLP core (secrets + core PII); they differ in the
	// surface-specific threat set.
	switch s.Profile {
	case ProfileBlockLLM:
		r.Secrets = append(r.Secrets, BuiltinSecretNames()...)
		r.PII = append(r.PII, "ssn", "credit-card")
		r.PromptInjection = true
	case ProfileBlockMCP:
		r.Secrets = append(r.Secrets, BuiltinSecretNames()...)
		r.PII = append(r.PII, "ssn", "credit-card")
		r.ToolAbuse = true
	case ProfileBlockLLMAndMCP:
		r.Secrets = append(r.Secrets, BuiltinSecretNames()...)
		r.PII = append(r.PII, "ssn", "credit-card")
		r.PromptInjection = true
		r.ToolAbuse = true
	}

	if d := s.Detectors; d != nil {
		r.Secrets = append(r.Secrets, d.Secrets...)
		r.PII = append(r.PII, d.PII...)
		r.PromptInjection = r.PromptInjection || d.PromptInjection
		r.ToolAbuse = r.ToolAbuse || d.ToolAbuse
		r.Keywords = d.Keywords
		r.Custom = append(r.Custom, d.Custom...)
	}

	r.Secrets = dedupeStrings(r.Secrets)
	r.PII = dedupeStrings(r.PII)
	return r
}

// HasAny reports whether the resolved set produces at least one rule.
func (r ResolvedDetectors) HasAny() bool {
	return len(r.Secrets) > 0 || len(r.PII) > 0 || r.PromptInjection || r.ToolAbuse ||
		(r.Keywords != nil && len(r.Keywords.Match) > 0) || len(r.Custom) > 0
}

// ─── Validation ──────────────────────────────────────────────────────────────

// Validate checks that referenced built-in detectors exist, custom rules are
// well-formed, and the policy resolves to at least one rule.
func (s *AIGuardrailPolicySpec) Validate() error {
	if s.Profile != "" && !knownProfiles[s.Profile] {
		return fmt.Errorf("unknown profile %q", s.Profile)
	}
	if s.SemanticEnabled() {
		if s.Semantic.Classifier == nil || s.Semantic.Classifier.BackendRef.Name == "" {
			return fmt.Errorf("semantic.enabled requires semantic.classifier.backendRef.name")
		}
		if a := s.Semantic.Action; a != "" && !strings.EqualFold(a, "Block") && !strings.EqualFold(a, "Log") {
			return fmt.Errorf("semantic.action must be Block or Log, got %q", a)
		}
	}
	r := s.Resolve()
	for _, name := range r.Secrets {
		if _, ok := builtinSecretSignatures[name]; !ok {
			return fmt.Errorf("unknown secret detector %q", name)
		}
	}
	for _, name := range r.PII {
		if _, ok := builtinPIISignatures[name]; !ok {
			return fmt.Errorf("unknown pii detector %q", name)
		}
	}
	for _, c := range r.Custom {
		if c.Name == "" || c.Regex == "" {
			return fmt.Errorf("custom detector requires name and regex")
		}
	}
	// A semantic-only policy (ICAP classifier, no signature detectors) is valid;
	// the classifier is the enforcement in that case.
	if !r.HasAny() && !s.SemanticEnabled() {
		return fmt.Errorf("policy resolves to no detectors (set a profile, detectors, or semantic)")
	}
	return nil
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
