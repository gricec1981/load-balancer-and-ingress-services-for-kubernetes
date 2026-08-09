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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func semanticSpec() AIGuardrailPolicySpec {
	thr := 0.75
	fo := false
	return AIGuardrailPolicySpec{
		TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "llm-route"},
		Semantic: &GuardrailSemantic{
			Enabled:    true,
			Classifier: &GuardrailClassifier{BackendRef: GuardrailBackendRef{Name: "prompt-injection-icap", Port: 1344}},
			Threshold:  &thr,
			FailOpen:   &fo,
			Action:     "Log",
		},
	}
}

func TestSemanticAccessorsDefaults(t *testing.T) {
	// A minimal enabled semantic block should return the documented defaults.
	sem := &GuardrailSemantic{Enabled: true}
	if got := sem.SemanticThreshold(); got != 0.6 {
		t.Errorf("default threshold = %v, want 0.6", got)
	}
	if !sem.SemanticFailOpen() {
		t.Error("default failOpen should be true")
	}
	if got := sem.SemanticMode(); got != "block" {
		t.Errorf("default mode = %q, want block", got)
	}
}

func TestSemanticAccessorsExplicit(t *testing.T) {
	s := semanticSpec()
	if !s.SemanticEnabled() {
		t.Fatal("SemanticEnabled should be true")
	}
	if got := s.Semantic.SemanticThreshold(); got != 0.75 {
		t.Errorf("threshold = %v, want 0.75", got)
	}
	if s.Semantic.SemanticFailOpen() {
		t.Error("failOpen should be false (explicit)")
	}
	if got := s.Semantic.SemanticMode(); got != "log" {
		t.Errorf("mode = %q, want log (from action Log)", got)
	}
}

func TestSemanticModeIsCaseInsensitive(t *testing.T) {
	for _, in := range []string{"Log", "log", "LOG"} {
		if got := (&GuardrailSemantic{Action: in}).SemanticMode(); got != "log" {
			t.Errorf("action %q -> mode %q, want log", in, got)
		}
	}
	if got := (&GuardrailSemantic{Action: "Block"}).SemanticMode(); got != "block" {
		t.Errorf("action Block -> mode %q, want block", got)
	}
}

func TestSemanticOnlyPolicyValidates(t *testing.T) {
	// A semantic-only policy (no profile, no detectors) must be valid — the
	// classifier is the enforcement.
	s := semanticSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("semantic-only policy should validate: %v", err)
	}
}

func TestSemanticRequiresBackendRef(t *testing.T) {
	s := AIGuardrailPolicySpec{
		TargetRef: PolicyTargetRef{Name: "r"},
		Semantic:  &GuardrailSemantic{Enabled: true},
	}
	if err := s.Validate(); err == nil {
		t.Error("semantic.enabled without classifier.backendRef should fail validation")
	}
}

func TestSemanticBadActionRejected(t *testing.T) {
	s := semanticSpec()
	s.Semantic.Action = "Quarantine"
	if err := s.Validate(); err == nil {
		t.Error("semantic.action other than Block/Log should fail validation")
	}
}

func TestSemanticComposesWithSignature(t *testing.T) {
	// Signature + semantic on one policy: both layers resolve independently.
	s := semanticSpec()
	s.Profile = ProfileBlockLLM
	if err := s.Validate(); err != nil {
		t.Fatalf("combined policy should validate: %v", err)
	}
	if !s.Resolve().HasAny() {
		t.Error("signature layer should still resolve detectors when combined with semantic")
	}
	if !s.SemanticEnabled() {
		t.Error("semantic layer should still be enabled when combined with a profile")
	}
}

func TestUnstructuredParsesSemantic(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "llm-guardrails", "namespace": "inference"},
		"spec": map[string]interface{}{
			"targetRef": map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "llm-route"},
			"semantic": map[string]interface{}{
				"enabled":   true,
				"threshold": 0.8,
				"failOpen":  false,
				"action":    "Log",
				"classifier": map[string]interface{}{
					"backendRef": map[string]interface{}{"name": "prompt-injection-icap", "namespace": "inference", "port": int64(1344)},
				},
			},
		},
	}}
	p, err := unstructuredToGuardrailPolicy(u)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !p.Spec.SemanticEnabled() {
		t.Fatal("semantic.enabled not parsed")
	}
	sem := p.Spec.Semantic
	if sem.Classifier == nil || sem.Classifier.BackendRef.Name != "prompt-injection-icap" ||
		sem.Classifier.BackendRef.Namespace != "inference" || sem.Classifier.BackendRef.Port != 1344 {
		t.Errorf("classifier backendRef not parsed: %+v", sem.Classifier)
	}
	if sem.SemanticThreshold() != 0.8 {
		t.Errorf("threshold not parsed: %v", sem.SemanticThreshold())
	}
	if sem.SemanticFailOpen() {
		t.Error("failOpen=false not parsed")
	}
	if sem.SemanticMode() != "log" {
		t.Errorf("mode not parsed: %q", sem.SemanticMode())
	}
	if err := p.Spec.Validate(); err != nil {
		t.Errorf("parsed semantic policy should validate: %v", err)
	}
}

func TestSemanticNameDerivation(t *testing.T) {
	p := &AIGuardrailPolicy{}
	p.Namespace = "inference"
	p.Name = "llm-guardrails"
	cases := map[string]string{
		icapPoolName(p):                "inference-llm-guardrails-icap-pool",
		icapPoolGroupName(p):           "inference-llm-guardrails-icap-pg",
		icapProfileName(p):             "inference-llm-guardrails-icap",
		icapSecurityPolicySetName(p):   "inference-llm-guardrails-icap-security",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("name = %q, want %q", got, want)
		}
	}
}
