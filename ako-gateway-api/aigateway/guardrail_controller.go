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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// AIGuardrailPolicyGVR is the GroupVersionResource for AIGuardrailPolicy.
var AIGuardrailPolicyGVR = schema.GroupVersionResource{
	Group:    PolicyGroup,
	Version:  PolicyVersion,
	Resource: "aiguardrailpolicies",
}

// ─── PolicyStore accessors ─────────────────────────────────────────────────────

// GetGuardrailPoliciesForRoute returns all AIGuardrailPolicies targeting the route.
func (s *PolicyStore) GetGuardrailPoliciesForRoute(routeNsName string) []*AIGuardrailPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*AIGuardrailPolicy
	for _, pNsName := range s.routeToGuardrailPolicies[routeNsName] {
		if p, ok := s.guardrailPolicyByNsName[pNsName]; ok {
			out = append(out, p)
		}
	}
	return out
}

// upsertGuardrailPolicy stores the policy and updates route→policy mappings.
// It returns the targetRef name the policy left, or "".
func (s *PolicyStore) upsertGuardrailPolicy(p *AIGuardrailPolicy) (movedFrom string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	var prevTarget *string
	if prev := s.guardrailPolicyByNsName[pNsName]; prev != nil {
		prevTarget = &prev.Spec.TargetRef.Name
	}
	s.guardrailPolicyByNsName[pNsName] = p
	return indexPolicyRoute(s.routeToGuardrailPolicies, p.Namespace, pNsName, prevTarget, p.Spec.TargetRef.Name)
}

// deleteGuardrailPolicy removes the policy and cleans route→policy mappings.
// It returns the removed policy, or nil if it was not stored.
func (s *PolicyStore) deleteGuardrailPolicy(ns, name string) *AIGuardrailPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.guardrailPolicyByNsName[pNsName]
	if !ok {
		return nil
	}
	unindexPolicyRoute(s.routeToGuardrailPolicies, p.Namespace+"/"+p.Spec.TargetRef.Name, pNsName)
	delete(s.guardrailPolicyByNsName, pNsName)
	return p
}

// ─── Event handlers ────────────────────────────────────────────────────────────

// SetupGuardrailPolicyEventHandlers wires Add/Update/Delete handlers for
// AIGuardrailPolicy. On any change it re-enqueues the targeted HTTPRoute so the
// graph layer re-authors the WafPolicy and re-attaches it.
func SetupGuardrailPolicyEventHandlers(
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
			p, err := parseGuardrailPolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AIGuardrailPolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertGuardrailPolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AIGuardrailPolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseGuardrailPolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AIGuardrailPolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertGuardrailPolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AIGuardrailPolicy, workqueues, numWorkers)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utils.AviLog.Errorf("AIGuardrailPolicy delete: couldn't get object from tombstone %#v", obj)
					return
				}
				u, ok = tombstone.Obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
			}
			ns, name := u.GetNamespace(), u.GetName()
			// Remove from the store first, then re-enqueue (the rebuild drops
			// waf_policy_ref / ICAP refs off the VS only if it no longer sees the
			// policy), then delete the AKO-authored objects for both layers.
			if p := SharedPolicyStore().deleteGuardrailPolicy(ns, name); p != nil {
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AIGuardrailPolicy, workqueues, numWorkers)
				DeleteGuardrailWafPolicy("AIGuardrailPolicy/"+ns+"/"+name, p)
				if p.Spec.SemanticEnabled() {
					DeleteGuardrailIcap("AIGuardrailPolicy/"+ns+"/"+name, p)
				}
			}
		},
	}
	// Tracked so startup can wait until this handler has put every
	// existing policy in the store before any route is translated.
	reg, err := informer.Informer().AddEventHandler(handler)
	trackPolicyHandler(lib.AIGuardrailPolicy, reg, err)
}

// ─── Parsing ───────────────────────────────────────────────────────────────────

func parseGuardrailPolicy(client dynamic.Interface, u *unstructured.Unstructured) (*AIGuardrailPolicy, error) {
	return unstructuredToGuardrailPolicy(livePolicyObject(client, AIGuardrailPolicyGVR, lib.AIGuardrailPolicy, u))
}

func unstructuredToGuardrailPolicy(obj *unstructured.Unstructured) (*AIGuardrailPolicy, error) {
	p := &AIGuardrailPolicy{}
	p.Name = obj.GetName()
	p.Namespace = obj.GetNamespace()

	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("spec not found in AIGuardrailPolicy %s/%s", p.Namespace, p.Name)
	}

	if v, _, _ := unstructured.NestedString(spec, "targetRef", "group"); v != "" {
		p.Spec.TargetRef.Group = v
	}
	if v, _, _ := unstructured.NestedString(spec, "targetRef", "kind"); v != "" {
		p.Spec.TargetRef.Kind = v
	}
	if v, _, _ := unstructured.NestedString(spec, "targetRef", "name"); v != "" {
		p.Spec.TargetRef.Name = v
	}
	if v, _, _ := unstructured.NestedString(spec, "profile"); v != "" {
		p.Spec.Profile = v
	}

	if iv, found, _ := unstructured.NestedMap(spec, "inspect"); found {
		ins := &GuardrailInspect{}
		if b, ok, _ := unstructured.NestedBool(iv, "request"); ok {
			ins.Request = &b
		}
		if b, ok, _ := unstructured.NestedBool(iv, "response"); ok {
			ins.Response = &b
		}
		p.Spec.Inspect = ins
	}

	if dv, found, _ := unstructured.NestedMap(spec, "detectors"); found {
		d := &GuardrailDetectors{}
		if s, _, _ := unstructured.NestedStringSlice(dv, "secrets"); len(s) > 0 {
			d.Secrets = s
		}
		if s, _, _ := unstructured.NestedStringSlice(dv, "pii"); len(s) > 0 {
			d.PII = s
		}
		if b, _, _ := unstructured.NestedBool(dv, "promptInjection"); b {
			d.PromptInjection = true
		}
		if b, _, _ := unstructured.NestedBool(dv, "toolAbuse"); b {
			d.ToolAbuse = true
		}
		if kw, found, _ := unstructured.NestedMap(dv, "keywords"); found {
			k := &GuardrailKeywords{}
			if m, _, _ := unstructured.NestedStringSlice(kw, "match"); len(m) > 0 {
				k.Match = m
			}
			if cs, _, _ := unstructured.NestedBool(kw, "caseSensitive"); cs {
				k.CaseSensitive = true
			}
			d.Keywords = k
		}
		if custom, found, _ := unstructured.NestedSlice(dv, "custom"); found {
			for _, cr := range custom {
				cm, ok := cr.(map[string]interface{})
				if !ok {
					continue
				}
				name, _, _ := unstructured.NestedString(cm, "name")
				regex, _, _ := unstructured.NestedString(cm, "regex")
				if name != "" && regex != "" {
					d.Custom = append(d.Custom, GuardrailCustomRule{Name: name, Regex: regex})
				}
			}
		}
		p.Spec.Detectors = d
	}

	if av, found, _ := unstructured.NestedMap(spec, "action"); found {
		a := &GuardrailAction{}
		if v, _, _ := unstructured.NestedString(av, "type"); v != "" {
			a.Type = v
		}
		if v, found, _ := unstructured.NestedInt64(av, "statusCode"); found {
			a.StatusCode = int(v)
		}
		p.Spec.Action = a
	}

	if sv, found, _ := unstructured.NestedMap(spec, "semantic"); found {
		sem := &GuardrailSemantic{}
		if b, _, _ := unstructured.NestedBool(sv, "enabled"); b {
			sem.Enabled = true
		}
		if a, _, _ := unstructured.NestedString(sv, "action"); a != "" {
			sem.Action = a
		}
		// threshold may arrive as float64 or int64 depending on the YAML literal.
		if t, found, _ := unstructured.NestedFloat64(sv, "threshold"); found {
			sem.Threshold = &t
		} else if ti, found, _ := unstructured.NestedInt64(sv, "threshold"); found {
			tf := float64(ti)
			sem.Threshold = &tf
		}
		if fo, found, _ := unstructured.NestedBool(sv, "failOpen"); found {
			sem.FailOpen = &fo
		}
		if cv, found, _ := unstructured.NestedMap(sv, "classifier"); found {
			c := &GuardrailClassifier{}
			if br, found, _ := unstructured.NestedMap(cv, "backendRef"); found {
				if v, _, _ := unstructured.NestedString(br, "name"); v != "" {
					c.BackendRef.Name = v
				}
				if v, _, _ := unstructured.NestedString(br, "namespace"); v != "" {
					c.BackendRef.Namespace = v
				}
				if v, found, _ := unstructured.NestedInt64(br, "port"); found {
					c.BackendRef.Port = int32(v)
				}
			}
			sem.Classifier = c
		}
		p.Spec.Semantic = sem
	}

	return p, nil
}
