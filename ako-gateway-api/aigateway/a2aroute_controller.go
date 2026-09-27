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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// ─── PolicyStore accessors ────────────────────────────────────────────────────

// GetA2ARoutePoliciesForRoute returns all AIA2ARoutePolicies targeting the route.
func (s *PolicyStore) GetA2ARoutePoliciesForRoute(routeNsName string) []*AIA2ARoutePolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*AIA2ARoutePolicy
	for _, pNsName := range s.routeToA2ARoutePolicies[routeNsName] {
		if p, ok := s.a2aRoutePolicyByNsName[pNsName]; ok {
			out = append(out, p)
		}
	}
	return out
}

// upsertA2ARoutePolicy stores the policy and updates route→policy mappings.
// It returns the targetRef name the policy left, or "".
func (s *PolicyStore) upsertA2ARoutePolicy(p *AIA2ARoutePolicy) (movedFrom string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	var prevTarget *string
	if prev := s.a2aRoutePolicyByNsName[pNsName]; prev != nil {
		prevTarget = &prev.Spec.TargetRef.Name
	}
	s.a2aRoutePolicyByNsName[pNsName] = p
	return indexPolicyRoute(s.routeToA2ARoutePolicies, p.Namespace, pNsName, prevTarget, p.Spec.TargetRef.Name)
}

// deleteA2ARoutePolicy removes the policy and cleans route→policy mappings.
// It returns the removed policy, or nil if it was not stored.
func (s *PolicyStore) deleteA2ARoutePolicy(ns, name string) *AIA2ARoutePolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.a2aRoutePolicyByNsName[pNsName]
	if !ok {
		return nil
	}
	unindexPolicyRoute(s.routeToA2ARoutePolicies, p.Namespace+"/"+p.Spec.TargetRef.Name, pNsName)
	delete(s.a2aRoutePolicyByNsName, pNsName)
	return p
}

// ─── Event handlers ───────────────────────────────────────────────────────────

// SetupA2ARoutePolicyEventHandlers wires Add/Update/Delete handlers for
// AIA2ARoutePolicy. On any change it re-enqueues the targeted HTTPRoute so
// the graph layer rebuilds the VS model with updated A2A configuration.
func SetupA2ARoutePolicyEventHandlers(
	informer informers.GenericInformer,
	dynamicClient dynamic.Interface,
	workqueues []workqueue.RateLimitingInterface, //nolint:staticcheck
	numWorkers uint32,
) {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				return
			}
			p, err := parseA2ARoutePolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AIA2ARoutePolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertA2ARoutePolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AIA2ARoutePolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseA2ARoutePolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AIA2ARoutePolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertA2ARoutePolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AIA2ARoutePolicy, workqueues, numWorkers)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utils.AviLog.Errorf("AIA2ARoutePolicy delete: couldn't get object from tombstone %#v", obj)
					return
				}
				u, ok = tombstone.Obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
			}
			ns, name := u.GetNamespace(), u.GetName()
			// Remove first, then re-enqueue the route the removed policy
			// targeted: the rebuild must not see the policy.
			if p := SharedPolicyStore().deleteA2ARoutePolicy(ns, name); p != nil {
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AIA2ARoutePolicy, workqueues, numWorkers)
			}
		},
	}
	// Tracked so startup can wait until this handler has put every
	// existing policy in the store before any route is translated.
	reg, err := informer.Informer().AddEventHandler(handler)
	trackPolicyHandler(lib.AIA2ARoutePolicy, reg, err)
}

// ─── Parsing ──────────────────────────────────────────────────────────────────

// parseA2ARoutePolicy fetches and parses an AIA2ARoutePolicy from the API server.
func parseA2ARoutePolicy(client dynamic.Interface, u *unstructured.Unstructured) (*AIA2ARoutePolicy, error) {
	return unstructuredToA2ARoutePolicy(livePolicyObject(client, AIA2ARoutePolicyGVR, lib.AIA2ARoutePolicy, u))
}

// unstructuredToA2ARoutePolicy converts an unstructured object to AIA2ARoutePolicy.
func unstructuredToA2ARoutePolicy(obj *unstructured.Unstructured) (*AIA2ARoutePolicy, error) {
	p := &AIA2ARoutePolicy{}
	p.Name = obj.GetName()
	p.Namespace = obj.GetNamespace()

	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("spec not found in AIA2ARoutePolicy %s/%s", p.Namespace, p.Name)
	}

	// targetRef
	if v, _, _ := unstructured.NestedString(spec, "targetRef", "group"); v != "" {
		p.Spec.TargetRef.Group = v
	}
	if v, _, _ := unstructured.NestedString(spec, "targetRef", "kind"); v != "" {
		p.Spec.TargetRef.Kind = v
	}
	if v, _, _ := unstructured.NestedString(spec, "targetRef", "name"); v != "" {
		p.Spec.TargetRef.Name = v
	}

	// authRef: a declared authRef survives even with an empty name, so the
	// policy fails validation and the route fails closed (see authRefFromSpec).
	p.Spec.AuthRef = authRefFromSpec(spec)

	// agentCard
	if ac, found, _ := unstructured.NestedMap(spec, "agentCard"); found {
		c := &A2AAgentCard{}
		if v, _, _ := unstructured.NestedBool(ac, "rewrite"); v {
			c.Rewrite = true
		}
		if v, _, _ := unstructured.NestedString(ac, "url"); v != "" {
			c.URL = v
		}
		p.Spec.AgentCard = c
	}

	// taskAffinity
	if ta, found, _ := unstructured.NestedMap(spec, "taskAffinity"); found {
		t := &A2ATaskAffinity{}
		if v, _, _ := unstructured.NestedString(ta, "timeout"); v != "" {
			t.Timeout = v
		}
		p.Spec.TaskAffinity = t
	}

	// agentAccess
	if aa, found, _ := unstructured.NestedMap(spec, "agentAccess"); found {
		a := &A2AAgentAccess{}
		if v, _, _ := unstructured.NestedString(aa, "agentClaim"); v != "" {
			a.AgentClaim = v
		}
		if v, _, _ := unstructured.NestedString(aa, "skillClaim"); v != "" {
			a.SkillClaim = v
		}
		if v, found, _ := unstructured.NestedBool(aa, "requireMethod"); found {
			a.RequireMethod = v
		}
		if v, _, _ := unstructured.NestedString(aa, "targetAgent"); v != "" {
			a.TargetAgent = v
		}
		if v, found, _ := unstructured.NestedBool(aa, "authorizePaths"); found {
			a.AuthorizePaths = v
		}
		if rulesRaw, found, _ := unstructured.NestedSlice(aa, "rules"); found {
			for _, rr := range rulesRaw {
				rm, ok := rr.(map[string]interface{})
				if !ok {
					continue
				}
				rule := AgentAccessRule{}
				if v, _, _ := unstructured.NestedString(rm, "agent"); v != "" {
					rule.Agent = v
				}
				if allow, _, _ := unstructured.NestedStringSlice(rm, "allow"); len(allow) > 0 {
					rule.Allow = allow
				}
				a.Rules = append(a.Rules, rule)
			}
		}
		p.Spec.AgentAccess = a
	}

	// onUnauthorized
	if ou, found, _ := unstructured.NestedMap(spec, "onUnauthorized"); found {
		a := &UnauthorizedAction{}
		if v, _, _ := unstructured.NestedString(ou, "type"); v != "" {
			a.Type = v
		}
		if v, found, _ := unstructured.NestedInt64(ou, "statusCode"); found {
			a.StatusCode = int(v)
		}
		p.Spec.OnUnauthorized = a
	}

	return p, nil
}
