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

	"github.com/vmware/alb-sdk/go/clients"
	avicache "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/cache"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// Guardrail WAF authoring.
// ─────────────────────────
// There is no AKO WafPolicy CRD and the AKO CRD operator does not manage WAF, so
// AKO authors the Avi WafPolicy directly over REST (the same pattern oauth_rest.go
// uses to build the OAuth Pool/AuthProfile/SSOPolicy). The translator then sets
// waf_policy_ref on the route VS — attachment can also be done via the L7Rule CRD,
// but authoring is what AKO adds. See docs/gateway-api/ai-gateway-guardrails.md.

// guardrailWafPolicyName derives the Avi WafPolicy name from the policy.
func guardrailWafPolicyName(policy *AIGuardrailPolicy) string {
	return fmt.Sprintf("%s-%s-ai-guardrail", policy.Namespace, policy.Name)
}

// resolveWafBaseRefs looks up the System WAF profile and the newest CRS so the
// generated policy references real objects. CRS is referenced but kept
// non-blocking (the custom rules carry enforcement); the ref is still required.
func resolveWafBaseRefs(client *clients.AviClient) (profileRef, crsRef string) {
	var prof struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/wafprofile?name=System-WAF-Profile", &prof)
	if prof.Count > 0 {
		profileRef = "/api/wafprofile/" + prof.Results[0].UUID
	}

	var crs struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/wafcrs?page_size=50&fields=name", &crs)
	best, bestName := "", ""
	for _, r := range crs.Results {
		// Prefer the lexically-greatest concrete "CRS-YYYY-N" version.
		if strings.HasPrefix(r.Name, "CRS-") && strings.Contains(r.Name, "-2") {
			if r.Name > bestName {
				bestName, best = r.Name, r.UUID
			}
		}
	}
	if best == "" && crs.Count > 0 {
		// Fallback: any CRS, deterministically chosen.
		names := make([]string, 0, len(crs.Results))
		byName := map[string]string{}
		for _, r := range crs.Results {
			names = append(names, r.Name)
			byName[r.Name] = r.UUID
		}
		sort.Strings(names)
		best = byName[names[len(names)-1]]
	}
	if best != "" {
		crsRef = "/api/wafcrs/" + best
	}
	return profileRef, crsRef
}

// EnsureGuardrailWafPolicy creates or updates the Avi WafPolicy that implements
// the guardrail spec and returns its name (for waf_policy_ref).
func EnsureGuardrailWafPolicy(key string, policy *AIGuardrailPolicy) (string, error) {
	if err := policy.Spec.Validate(); err != nil {
		return "", fmt.Errorf("invalid AIGuardrailPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
	}
	name := guardrailWafPolicyName(policy)
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	profileRef, crsRef := resolveWafBaseRefs(client)
	if profileRef == "" {
		return "", fmt.Errorf("System-WAF-Profile not found (is the WAF feature available on this controller?)")
	}

	resolved := policy.Spec.Resolve()
	rules := GenerateGuardrailRules(
		resolved,
		policy.Spec.EffectiveInspectRequest(),
		policy.Spec.EffectiveInspectResponse(),
		policy.Spec.EffectiveActionType() == "Block",
		policy.Spec.EffectiveStatusCode(),
	)
	body := BuildGuardrailWafPolicyBody(name, "/api/tenant/?name="+lib.GetEscapedValue(tenant), profileRef, crsRef, rules)

	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/wafpolicy?name="+name, &check)
	var resp interface{}
	if check.Count > 0 {
		if err := lib.AviPut(client, "/api/wafpolicy/"+check.Results[0].UUID, body, &resp); err != nil {
			return "", fmt.Errorf("guardrail WafPolicy PUT %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: guardrail WafPolicy %s updated (%d rules)", key, name, len(rules))
	} else {
		if err := lib.AviPost(client, "/api/wafpolicy", body, &resp); err != nil {
			return "", fmt.Errorf("guardrail WafPolicy POST %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: guardrail WafPolicy %s created (%d rules)", key, name, len(rules))
	}
	return name, nil
}

// DeleteGuardrailWafPolicy removes the AKO-authored WafPolicy when the policy is
// deleted. The VS no longer references it (the route was re-enqueued first), so
// the delete is safe.
func DeleteGuardrailWafPolicy(key string, policy *AIGuardrailPolicy) {
	name := guardrailWafPolicyName(policy)
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/wafpolicy?name="+name, &check)
	if check.Count > 0 {
		if err := lib.AviDelete(client, "/api/wafpolicy/"+check.Results[0].UUID); err != nil {
			utils.AviLog.Warnf("key: %s, msg: guardrail WafPolicy %s delete failed: %v", key, name, err)
			return
		}
		utils.AviLog.Infof("key: %s, msg: guardrail WafPolicy %s deleted", key, name)
	}
}
