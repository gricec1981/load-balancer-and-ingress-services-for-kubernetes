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
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
func (s *PolicyStore) upsertA2ARoutePolicy(p *AIA2ARoutePolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	s.a2aRoutePolicyByNsName[pNsName] = p
	routeNsName := p.Namespace + "/" + p.Spec.TargetRef.Name
	s.routeToA2ARoutePolicies[routeNsName] = addUnique(s.routeToA2ARoutePolicies[routeNsName], pNsName)
}

// deleteA2ARoutePolicy removes the policy and cleans route→policy mappings.
func (s *PolicyStore) deleteA2ARoutePolicy(ns, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.a2aRoutePolicyByNsName[pNsName]
	if !ok {
		return
	}
	routeNsName := p.Namespace + "/" + p.Spec.TargetRef.Name
	s.routeToA2ARoutePolicies[routeNsName] = removeElem(s.routeToA2ARoutePolicies[routeNsName], pNsName)
	delete(s.a2aRoutePolicyByNsName, pNsName)
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
			p, err := parseA2ARoutePolicy(dynamicClient, u.GetNamespace(), u.GetName())
			if err != nil {
				utils.AviLog.Warnf("AIA2ARoutePolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			SharedPolicyStore().upsertA2ARoutePolicy(p)
			enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIA2ARoutePolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseA2ARoutePolicy(dynamicClient, u.GetNamespace(), u.GetName())
			if err != nil {
				utils.AviLog.Warnf("AIA2ARoutePolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			SharedPolicyStore().upsertA2ARoutePolicy(p)
			enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIA2ARoutePolicy, workqueues, numWorkers)
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
			ps := SharedPolicyStore()
			pNsName := ns + "/" + name
			ps.mu.RLock()
			p := ps.a2aRoutePolicyByNsName[pNsName]
			ps.mu.RUnlock()
			if p != nil {
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AIA2ARoutePolicy, workqueues, numWorkers)
			}
			ps.deleteA2ARoutePolicy(ns, name)
		},
	}
	informer.Informer().AddEventHandler(handler)
}

// ─── Parsing ──────────────────────────────────────────────────────────────────

// parseA2ARoutePolicy fetches and parses an AIA2ARoutePolicy from the API server.
func parseA2ARoutePolicy(client dynamic.Interface, ns, name string) (*AIA2ARoutePolicy, error) {
	obj, err := client.Resource(AIA2ARoutePolicyGVR).Namespace(ns).Get(
		context.TODO(), name, metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("get AIA2ARoutePolicy %s/%s: %w", ns, name, err)
	}
	return unstructuredToA2ARoutePolicy(obj)
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

	// authRef
	if v, _, _ := unstructured.NestedString(spec, "authRef", "name"); v != "" {
		p.Spec.AuthRef = &AuthPolicyRef{Name: v}
	}

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
