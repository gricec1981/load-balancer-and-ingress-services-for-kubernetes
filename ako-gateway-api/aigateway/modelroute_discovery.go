/*
 * Copyright © 2026 Broadcom Inc. and/or its subsidiaries. All Rights Reserved.
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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/workqueue"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// ─── Model alias discovery ─────────────────────────────────────────────────────
//
// Discovery extends an AIModelRoutePolicy's static modelTiers table with aliases
// declared on running pods (annotation = alias, label = tier). The informer
// cache is the only state: aliases are recomputed from live pods at every
// translation, so there is nothing to drift and nothing to garbage-collect.
// KServe is not consulted — its predictor pods participate purely because
// InferenceService labels/annotations propagate onto them, and a bare
// Deployment with the same keys participates identically.

// WithDiscoveredModels returns the policy to translate: the original when
// discovery is off, otherwise a copy whose Spec.ModelTiers is the union of the
// static table and the aliases discovered from pods (static entries win). The
// stored policy object is never mutated. Skipped candidates are logged.
func WithDiscoveredModels(key string, policy *AIModelRoutePolicy) *AIModelRoutePolicy {
	if !policy.Spec.Discovery.IsEnabled() {
		return policy
	}

	pods := listDiscoveryPods(key, &policy.Spec, policy.Namespace)

	known := make(map[string]bool, len(policy.Spec.Tiers))
	for _, t := range policy.Spec.Tiers {
		known[t.Name] = true
	}

	discovered, warnings := mergeDiscoveredModels(
		known,
		policy.Spec.ModelTiers,
		pods,
		policy.Spec.Discovery.EffectiveAliasAnnotation(),
		policy.Spec.Discovery.EffectiveTierLabel(),
	)
	for _, w := range warnings {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s discovery: %s", key, policy.Namespace, policy.Name, w)
	}
	if len(discovered) == 0 {
		return policy
	}

	// Shallow-copy the policy, deep-copy only the merged map.
	effective := *policy
	merged := make(map[string]string, len(policy.Spec.ModelTiers)+len(discovered))
	for k, v := range policy.Spec.ModelTiers {
		merged[k] = v
	}
	for k, v := range discovered {
		merged[k] = v
	}
	effective.Spec.ModelTiers = merged

	// Deterministic, greppable summary of what discovery published.
	aliases := make([]string, 0, len(discovered))
	for a, t := range discovered {
		aliases = append(aliases, a+"→"+t)
	}
	sort.Strings(aliases)
	utils.AviLog.Infof("key: %s, msg: AIModelRoutePolicy %s/%s discovery: %d alias(es) merged: %s",
		key, policy.Namespace, policy.Name, len(discovered), strings.Join(aliases, ", "))

	return &effective
}

// listDiscoveryPods lists candidate pods from the shared pod informer across
// the policy's allowed namespaces. A nil informer (unit tests, degraded boot)
// discovers nothing.
func listDiscoveryPods(key string, spec *AIModelRoutePolicySpec, policyNamespace string) []*corev1.Pod {
	informers := utils.GetInformers()
	if informers == nil || informers.PodInformer == nil {
		utils.AviLog.Warnf("key: %s, msg: model discovery: pod informer unavailable, discovering nothing", key)
		return nil
	}
	var pods []*corev1.Pod
	for _, ns := range spec.Discovery.EffectiveNamespaces(policyNamespace) {
		nsPods, err := informers.PodInformer.Lister().Pods(ns).List(labels.Everything())
		if err != nil {
			utils.AviLog.Warnf("key: %s, msg: model discovery: listing pods in %s: %v", key, ns, err)
			continue
		}
		pods = append(pods, nsPods...)
	}
	return pods
}

// mergeDiscoveredModels reduces candidate pods to an alias→tier map. Pure
// function — all edge policy lives here so it is unit-testable:
//
//   - only pods carrying a non-empty alias annotation participate;
//   - terminating pods (DeletionTimestamp set) are ignored, so a deleted model
//     un-registers on the pod's way out rather than at the last possible moment;
//   - an alias containing "*" is rejected (it would be interpreted as a glob by
//     the tier-resolution table);
//   - the tier label must name a declared tier — a workload typo must never
//     alter routing;
//   - a static modelTiers entry always wins over a discovered alias;
//   - two pods claiming the same alias with different tiers: the oldest pod
//     wins (creation timestamp, then namespace/name as a stable tiebreak) —
//     newer claimants lose loudly, never flappingly. Same-tier duplicates
//     (replicas of one model) are the normal case and merge silently.
func mergeDiscoveredModels(
	knownTiers map[string]bool,
	static map[string]string,
	pods []*corev1.Pod,
	aliasAnnotation, tierLabel string,
) (map[string]string, []string) {
	type claim struct {
		pod  *corev1.Pod
		tier string
	}
	var warnings []string
	winners := make(map[string]claim)

	// Deterministic iteration: oldest first, stable tiebreak. The first valid
	// claim for an alias then wins by construction.
	sorted := make([]*corev1.Pod, len(pods))
	copy(sorted, pods)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, tj := sorted[i].CreationTimestamp, sorted[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return ti.Before(&tj)
		}
		ki := sorted[i].Namespace + "/" + sorted[i].Name
		kj := sorted[j].Namespace + "/" + sorted[j].Name
		return ki < kj
	})

	for _, pod := range sorted {
		alias := pod.Annotations[aliasAnnotation]
		if alias == "" {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if strings.Contains(alias, "*") {
			warnings = append(warnings, fmt.Sprintf("pod %s/%s: alias %q contains %q and is ignored (globs are reserved for static modelTiers keys)", pod.Namespace, pod.Name, alias, "*"))
			continue
		}
		tier := pod.Labels[tierLabel]
		if tier == "" {
			warnings = append(warnings, fmt.Sprintf("pod %s/%s: alias %q has no %q label, ignored", pod.Namespace, pod.Name, alias, tierLabel))
			continue
		}
		if !knownTiers[tier] {
			warnings = append(warnings, fmt.Sprintf("pod %s/%s: alias %q names undeclared tier %q, ignored", pod.Namespace, pod.Name, alias, tier))
			continue
		}
		if staticTier, ok := static[alias]; ok {
			warnings = append(warnings, fmt.Sprintf("pod %s/%s: alias %q is statically mapped to tier %q, pod claim ignored (static wins)", pod.Namespace, pod.Name, alias, staticTier))
			continue
		}
		if prev, ok := winners[alias]; ok {
			if prev.tier != tier {
				warnings = append(warnings, fmt.Sprintf("pod %s/%s: alias %q already claimed for tier %q by older pod %s/%s, claim for tier %q ignored", pod.Namespace, pod.Name, alias, prev.tier, prev.pod.Namespace, prev.pod.Name, tier))
			}
			continue // same tier: replicas of one model, normal
		}
		winners[alias] = claim{pod: pod, tier: tier}
	}

	out := make(map[string]string, len(winners))
	for alias, c := range winners {
		out[alias] = c.tier
	}
	return out, warnings
}

// ─── Pod event hook ────────────────────────────────────────────────────────────

// discoveryPodRelevant reports whether a pod could affect any discovery-enabled
// policy's alias table (cheap pre-filter: carries any known alias annotation).
func discoveryPodRelevant(pod *corev1.Pod, policies []*AIModelRoutePolicy) bool {
	for _, p := range policies {
		if pod.Annotations[p.Spec.Discovery.EffectiveAliasAnnotation()] != "" {
			return true
		}
	}
	return false
}

// GetDiscoveryPoliciesForNamespace returns the discovery-enabled
// AIModelRoutePolicies allowed to read pods in ns.
func (s *PolicyStore) GetDiscoveryPoliciesForNamespace(ns string) []*AIModelRoutePolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*AIModelRoutePolicy
	for _, p := range s.modelRoutePolicyByNsName {
		if p.Spec.Discovery.IsEnabled() && p.Spec.Discovery.AllowsNamespace(p.Namespace, ns) {
			out = append(out, p)
		}
	}
	return out
}

// HandleDiscoveryPodEvent re-enqueues the target route of every
// discovery-enabled policy a pod event may affect, so translation re-reads the
// informer cache. Pass the OLD pod as well on updates (annotation removal must
// also re-translate). Idempotent and rate-limited; cheap no-op when the AI
// gateway is disabled or the pod carries no alias annotation.
func HandleDiscoveryPodEvent(pod *corev1.Pod, wqs []workqueue.RateLimitingInterface, numWorkers uint32) { //nolint:staticcheck
	if pod == nil || !lib.IsAIGatewayEnabled() {
		return
	}
	policies := SharedPolicyStore().GetDiscoveryPoliciesForNamespace(pod.Namespace)
	if len(policies) == 0 || !discoveryPodRelevant(pod, policies) {
		return
	}
	for _, p := range policies {
		enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIModelRoutePolicy, wqs, numWorkers)
	}
}
