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

// sampleModelRouteUnstructured mirrors a real AIModelRoutePolicy CR as the
// dynamic informer would deliver it.
func sampleModelRouteUnstructured() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "ai.ako.vmware.com/v1alpha1",
		"kind":       "AIModelRoutePolicy",
		"metadata": map[string]interface{}{
			"name":      "llm-tiers",
			"namespace": "inference",
		},
		"spec": map[string]interface{}{
			"targetRef": map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
				"name":  "llm-route",
			},
			"modelField": "model",
			"tiers": []interface{}{
				map[string]interface{}{
					"name": "premium",
					"backendRef": map[string]interface{}{
						"group": "gateway.inference.x-k8s.io",
						"kind":  "InferencePool",
						"name":  "premium-llm",
					},
				},
				map[string]interface{}{
					"name": "economy",
					"backendRef": map[string]interface{}{
						"group": "gateway.inference.x-k8s.io",
						"kind":  "InferencePool",
						"name":  "economy-llm",
					},
				},
			},
			"modelTiers": map[string]interface{}{
				"llama-3-70b-instruct": "premium",
				"mistral-7b*":          "economy",
			},
			"defaultTier": "economy",
			"entitlements": map[string]interface{}{
				"groupClaim": "group",
				"rules": []interface{}{
					map[string]interface{}{
						"group": "paid",
						"allow": []interface{}{"premium", "economy"},
					},
					map[string]interface{}{
						"group": "free",
						"allow": []interface{}{"economy"},
					},
				},
			},
			"onUnentitled": map[string]interface{}{
				"type":       "Reject",
				"statusCode": int64(451),
			},
		},
	}}
}

func TestUnstructuredToModelRoutePolicy(t *testing.T) {
	p, err := unstructuredToModelRoutePolicy(sampleModelRouteUnstructured())
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if p.Name != "llm-tiers" || p.Namespace != "inference" {
		t.Errorf("metadata not parsed: %s/%s", p.Namespace, p.Name)
	}
	if p.Spec.TargetRef.Name != "llm-route" || p.Spec.TargetRef.Kind != "HTTPRoute" {
		t.Errorf("targetRef not parsed: %+v", p.Spec.TargetRef)
	}
	if p.Spec.EffectiveModelField() != "model" {
		t.Errorf("modelField = %q", p.Spec.EffectiveModelField())
	}
	if len(p.Spec.Tiers) != 2 || p.Spec.Tiers[0].Name != "premium" ||
		p.Spec.Tiers[0].BackendRef.Name != "premium-llm" ||
		p.Spec.Tiers[0].BackendRef.Kind != "InferencePool" {
		t.Errorf("tiers not parsed: %+v", p.Spec.Tiers)
	}
	if p.Spec.ModelTiers["llama-3-70b-instruct"] != "premium" || p.Spec.ModelTiers["mistral-7b*"] != "economy" {
		t.Errorf("modelTiers not parsed: %+v", p.Spec.ModelTiers)
	}
	if p.Spec.DefaultTier != "economy" {
		t.Errorf("defaultTier = %q", p.Spec.DefaultTier)
	}
	if p.Spec.Entitlements == nil || p.Spec.Entitlements.EffectiveGroupClaim() != "group" ||
		len(p.Spec.Entitlements.Rules) != 2 {
		t.Fatalf("entitlements not parsed: %+v", p.Spec.Entitlements)
	}
	if p.Spec.Entitlements.Rules[0].Group != "paid" ||
		len(p.Spec.Entitlements.Rules[0].Allow) != 2 {
		t.Errorf("entitlement rule not parsed: %+v", p.Spec.Entitlements.Rules[0])
	}
	if p.Spec.OnUnentitled == nil || p.Spec.OnUnentitled.EffectiveType() != "Reject" ||
		p.Spec.OnUnentitled.EffectiveStatusCode() != 451 {
		t.Errorf("onUnentitled not parsed: %+v", p.Spec.OnUnentitled)
	}

	// The parsed policy must be valid and resolve/entitle as configured.
	if err := p.Spec.Validate(); err != nil {
		t.Errorf("parsed policy should validate: %v", err)
	}
	if got := p.Spec.ResolveTier("mistral-7b-instruct"); got != "economy" {
		t.Errorf("ResolveTier(mistral-7b-instruct) = %q, want economy", got)
	}
	if p.Spec.IsAllowed("free", "premium") {
		t.Error("free should not reach premium")
	}
}

func TestPolicyStoreModelRouteUpsertDelete(t *testing.T) {
	p, err := unstructuredToModelRoutePolicy(sampleModelRouteUnstructured())
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	s := SharedPolicyStore()
	s.upsertModelRoutePolicy(p)

	got := s.GetModelRoutePoliciesForRoute("inference/llm-route")
	if len(got) != 1 || got[0].Name != "llm-tiers" {
		t.Fatalf("expected the policy indexed by route, got %d", len(got))
	}

	s.deleteModelRoutePolicy("inference", "llm-tiers")
	if got := s.GetModelRoutePoliciesForRoute("inference/llm-route"); len(got) != 0 {
		t.Errorf("policy should be removed after delete, got %d", len(got))
	}
}
