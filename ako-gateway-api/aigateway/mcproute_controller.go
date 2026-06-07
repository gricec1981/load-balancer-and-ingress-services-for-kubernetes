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

// ─── PolicyStore accessors ─────────────────────────────────────────────────────

// GetMCPRoutePoliciesForRoute returns all AIMCPRoutePolicies targeting the route.
func (s *PolicyStore) GetMCPRoutePoliciesForRoute(routeNsName string) []*AIMCPRoutePolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*AIMCPRoutePolicy
	for _, pNsName := range s.routeToMCPRoutePolicies[routeNsName] {
		if p, ok := s.mcpRoutePolicyByNsName[pNsName]; ok {
			out = append(out, p)
		}
	}
	return out
}

// GetAuthPolicyByNsName returns the AIGatewayAuthPolicy named ns/name, or nil. It
// lets the MCP translator resolve an AIMCPRoutePolicy's authRef to reuse the LLM
// gateway's identity provider (shared IdP, §5 of the MCP design doc).
func (s *PolicyStore) GetAuthPolicyByNsName(ns, name string) *AIGatewayAuthPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authPolicyByNsName[ns+"/"+name]
}

// upsertMCPRoutePolicy stores the policy and updates route→policy mappings.
func (s *PolicyStore) upsertMCPRoutePolicy(p *AIMCPRoutePolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	s.mcpRoutePolicyByNsName[pNsName] = p
	routeNsName := p.Namespace + "/" + p.Spec.TargetRef.Name
	s.routeToMCPRoutePolicies[routeNsName] = addUnique(s.routeToMCPRoutePolicies[routeNsName], pNsName)
}

// deleteMCPRoutePolicy removes the policy and cleans route→policy mappings.
func (s *PolicyStore) deleteMCPRoutePolicy(ns, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.mcpRoutePolicyByNsName[pNsName]
	if !ok {
		return
	}
	routeNsName := p.Namespace + "/" + p.Spec.TargetRef.Name
	s.routeToMCPRoutePolicies[routeNsName] = removeElem(s.routeToMCPRoutePolicies[routeNsName], pNsName)
	delete(s.mcpRoutePolicyByNsName, pNsName)
}

// ─── Event handlers ────────────────────────────────────────────────────────────

// SetupMCPRoutePolicyEventHandlers wires Add/Update/Delete handlers for
// AIMCPRoutePolicy. On any change it re-enqueues the targeted HTTPRoute so the
// graph layer rebuilds the VS model with updated MCP configuration.
func SetupMCPRoutePolicyEventHandlers(
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
			p, err := parseMCPRoutePolicy(dynamicClient, u.GetNamespace(), u.GetName())
			if err != nil {
				utils.AviLog.Warnf("AIMCPRoutePolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			SharedPolicyStore().upsertMCPRoutePolicy(p)
			enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIMCPRoutePolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseMCPRoutePolicy(dynamicClient, u.GetNamespace(), u.GetName())
			if err != nil {
				utils.AviLog.Warnf("AIMCPRoutePolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			SharedPolicyStore().upsertMCPRoutePolicy(p)
			enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIMCPRoutePolicy, workqueues, numWorkers)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utils.AviLog.Errorf("AIMCPRoutePolicy delete: couldn't get object from tombstone %#v", obj)
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
			p := ps.mcpRoutePolicyByNsName[pNsName]
			ps.mu.RUnlock()
			if p != nil {
				// Re-enqueue before deleting so the translator sees the last targetRef.
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AIMCPRoutePolicy, workqueues, numWorkers)
			}
			ps.deleteMCPRoutePolicy(ns, name)
		},
	}
	informer.Informer().AddEventHandler(handler)
}

// ─── Parsing ───────────────────────────────────────────────────────────────────

// parseMCPRoutePolicy fetches and parses an AIMCPRoutePolicy from the API server.
func parseMCPRoutePolicy(client dynamic.Interface, ns, name string) (*AIMCPRoutePolicy, error) {
	obj, err := client.Resource(AIMCPRoutePolicyGVR).Namespace(ns).Get(
		context.TODO(), name, metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("get AIMCPRoutePolicy %s/%s: %w", ns, name, err)
	}
	return unstructuredToMCPRoutePolicy(obj)
}

// unstructuredToMCPRoutePolicy converts an unstructured object to AIMCPRoutePolicy.
func unstructuredToMCPRoutePolicy(obj *unstructured.Unstructured) (*AIMCPRoutePolicy, error) {
	p := &AIMCPRoutePolicy{}
	p.Name = obj.GetName()
	p.Namespace = obj.GetNamespace()

	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("spec not found in AIMCPRoutePolicy %s/%s", p.Namespace, p.Name)
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

	// session
	if sess, found, _ := unstructured.NestedMap(spec, "session"); found {
		s := &MCPSession{}
		if v, _, _ := unstructured.NestedString(sess, "header"); v != "" {
			s.Header = v
		}
		if v, _, _ := unstructured.NestedString(sess, "timeout"); v != "" {
			s.Timeout = v
		}
		p.Spec.Session = s
	}

	// toolAccess
	if ta, found, _ := unstructured.NestedMap(spec, "toolAccess"); found {
		t := &MCPToolAccess{}
		if v, _, _ := unstructured.NestedString(ta, "roleClaim"); v != "" {
			t.RoleClaim = v
		}
		if rulesRaw, found, _ := unstructured.NestedSlice(ta, "rules"); found {
			for _, rr := range rulesRaw {
				rm, ok := rr.(map[string]interface{})
				if !ok {
					continue
				}
				rule := ToolAccessRule{}
				if v, _, _ := unstructured.NestedString(rm, "role"); v != "" {
					rule.Role = v
				}
				if allow, _, _ := unstructured.NestedStringSlice(rm, "allow"); len(allow) > 0 {
					rule.Allow = allow
				}
				t.Rules = append(t.Rules, rule)
			}
		}
		p.Spec.ToolAccess = t
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
