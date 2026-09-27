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

// AIModelRoutePolicyGVR is the GroupVersionResource for AIModelRoutePolicy.
var AIModelRoutePolicyGVR = schema.GroupVersionResource{
	Group:    PolicyGroup,
	Version:  PolicyVersion,
	Resource: "aimodelroutepolicies",
}

// ─── PolicyStore accessors ─────────────────────────────────────────────────────

// GetModelRoutePoliciesForRoute returns all AIModelRoutePolicies targeting the route.
func (s *PolicyStore) GetModelRoutePoliciesForRoute(routeNsName string) []*AIModelRoutePolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*AIModelRoutePolicy
	for _, pNsName := range s.routeToModelRoutePolicies[routeNsName] {
		if p, ok := s.modelRoutePolicyByNsName[pNsName]; ok {
			out = append(out, p)
		}
	}
	return out
}

// AllModelRoutePolicies returns every stored AIModelRoutePolicy (any order).
func (s *PolicyStore) AllModelRoutePolicies() []*AIModelRoutePolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AIModelRoutePolicy, 0, len(s.modelRoutePolicyByNsName))
	for _, p := range s.modelRoutePolicyByNsName {
		out = append(out, p)
	}
	return out
}

// upsertModelRoutePolicy stores the policy and updates route→policy mappings.
// It returns the targetRef name the policy left, or "".
func (s *PolicyStore) upsertModelRoutePolicy(p *AIModelRoutePolicy) (movedFrom string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	var prevTarget *string
	if prev := s.modelRoutePolicyByNsName[pNsName]; prev != nil {
		prevTarget = &prev.Spec.TargetRef.Name
	}
	s.modelRoutePolicyByNsName[pNsName] = p
	return indexPolicyRoute(s.routeToModelRoutePolicies, p.Namespace, pNsName, prevTarget, p.Spec.TargetRef.Name)
}

// deleteModelRoutePolicy removes the policy and cleans route→policy mappings.
// It returns the removed policy, or nil if it was not stored.
func (s *PolicyStore) deleteModelRoutePolicy(ns, name string) *AIModelRoutePolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.modelRoutePolicyByNsName[pNsName]
	if !ok {
		return nil
	}
	unindexPolicyRoute(s.routeToModelRoutePolicies, p.Namespace+"/"+p.Spec.TargetRef.Name, pNsName)
	delete(s.modelRoutePolicyByNsName, pNsName)
	return p
}

// ─── Event handlers ────────────────────────────────────────────────────────────

// SetupModelRoutePolicyEventHandlers wires Add/Update/Delete handlers for
// AIModelRoutePolicy. On any change it re-enqueues the targeted HTTPRoute so the
// graph layer rebuilds the VS model with updated tier routing.
func SetupModelRoutePolicyEventHandlers(
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
			p, err := parseModelRoutePolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AIModelRoutePolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertModelRoutePolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AIModelRoutePolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseModelRoutePolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AIModelRoutePolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertModelRoutePolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AIModelRoutePolicy, workqueues, numWorkers)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utils.AviLog.Errorf("AIModelRoutePolicy delete: couldn't get object from tombstone %#v", obj)
					return
				}
				u, ok = tombstone.Obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
			}
			ns, name := u.GetNamespace(), u.GetName()
			// Remove from the store first, then re-enqueue the route the removed
			// policy targeted: a rebuild that still saw the policy would re-apply
			// its tiers (and re-create the pools torn down below).
			if p := SharedPolicyStore().deleteModelRoutePolicy(ns, name); p != nil {
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AIModelRoutePolicy, workqueues, numWorkers)
				// Tear down any AKO-authored FQDN pools/pool groups (external providers
				// and remote-site peers); node-graph tiers are cleaned by the translator.
				DeleteProviderTiers("AIModelRoutePolicy/"+ns+"/"+name, p)
				DeleteRemoteTiers("AIModelRoutePolicy/"+ns+"/"+name, p)
			}
		},
	}
	// Tracked so startup can wait until this handler has put every
	// existing policy in the store before any route is translated.
	reg, err := informer.Informer().AddEventHandler(handler)
	trackPolicyHandler(lib.AIModelRoutePolicy, reg, err)
}

// ─── Parsing ───────────────────────────────────────────────────────────────────

// parseModelRoutePolicy fetches and parses an AIModelRoutePolicy from the API server.
func parseModelRoutePolicy(client dynamic.Interface, u *unstructured.Unstructured) (*AIModelRoutePolicy, error) {
	return unstructuredToModelRoutePolicy(livePolicyObject(client, AIModelRoutePolicyGVR, lib.AIModelRoutePolicy, u))
}

// unstructuredToModelRoutePolicy converts an unstructured object to AIModelRoutePolicy.
func unstructuredToModelRoutePolicy(obj *unstructured.Unstructured) (*AIModelRoutePolicy, error) {
	p := &AIModelRoutePolicy{}
	p.Name = obj.GetName()
	p.Namespace = obj.GetNamespace()

	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("spec not found in AIModelRoutePolicy %s/%s", p.Namespace, p.Name)
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

	if v, _, _ := unstructured.NestedString(spec, "modelField"); v != "" {
		p.Spec.ModelField = v
	}
	if v, _, _ := unstructured.NestedString(spec, "defaultTier"); v != "" {
		p.Spec.DefaultTier = v
	}

	// tiers
	if tiersRaw, found, _ := unstructured.NestedSlice(spec, "tiers"); found {
		for _, tr := range tiersRaw {
			tm, ok := tr.(map[string]interface{})
			if !ok {
				continue
			}
			tier := ModelTier{}
			if v, _, _ := unstructured.NestedString(tm, "name"); v != "" {
				tier.Name = v
			}
			if v, _, _ := unstructured.NestedString(tm, "backendRef", "group"); v != "" {
				tier.BackendRef.Group = v
			}
			if v, _, _ := unstructured.NestedString(tm, "backendRef", "kind"); v != "" {
				tier.BackendRef.Kind = v
			}
			if v, _, _ := unstructured.NestedString(tm, "backendRef", "name"); v != "" {
				tier.BackendRef.Name = v
			}
			if v, _, _ := unstructured.NestedString(tm, "backendRef", "namespace"); v != "" {
				tier.BackendRef.Namespace = v
			}
			if bt, found, _ := unstructured.NestedMap(tm, "backendTLS"); found {
				tier.BackendTLS = parseModelBackendTLS(bt)
			}
			if pv, found, _ := unstructured.NestedMap(tm, "provider"); found {
				prov := &ModelProvider{}
				if v, _, _ := unstructured.NestedString(pv, "host"); v != "" {
					prov.Host = v
				}
				if v, _, _ := unstructured.NestedString(pv, "path"); v != "" {
					prov.Path = v
				}
				if v, found, _ := unstructured.NestedInt64(pv, "port"); found {
					prov.Port = int32(v)
				}
				if v, found, _ := unstructured.NestedBool(pv, "tls"); found {
					prov.TLS = &v
				}
				if av, found, _ := unstructured.NestedMap(pv, "auth"); found {
					auth := &ProviderAuth{}
					if v, _, _ := unstructured.NestedString(av, "header"); v != "" {
						auth.Header = v
					}
					if v, found, _ := unstructured.NestedString(av, "scheme"); found {
						auth.Scheme = &v
					}
					if v, _, _ := unstructured.NestedString(av, "secretRef", "name"); v != "" {
						auth.SecretRef.Name = v
					}
					if v, _, _ := unstructured.NestedString(av, "secretRef", "key"); v != "" {
						auth.SecretRef.Key = v
					}
					prov.Auth = auth
				}
				tier.Provider = prov
			}
			if rm, found, _ := unstructured.NestedMap(tm, "remote"); found {
				rem := &ModelRemote{}
				if v, _, _ := unstructured.NestedString(rm, "host"); v != "" {
					rem.Host = v
				}
				if v, found, _ := unstructured.NestedInt64(rm, "port"); found {
					rem.Port = int32(v)
				}
				if v, found, _ := unstructured.NestedBool(rm, "tls"); found {
					rem.TLS = &v
				}
				if v, found, _ := unstructured.NestedBool(rm, "preserveHost"); found {
					rem.PreserveHost = &v
				}
				if v, _, _ := unstructured.NestedString(rm, "healthPath"); v != "" {
					rem.HealthPath = v
				}
				tier.Remote = rem
			}
			p.Spec.Tiers = append(p.Spec.Tiers, tier)
		}
	}

	// modelTiers (map[string]string)
	if mt, found, _ := unstructured.NestedMap(spec, "modelTiers"); found {
		p.Spec.ModelTiers = make(map[string]string, len(mt))
		for k, v := range mt {
			if sv, ok := v.(string); ok {
				p.Spec.ModelTiers[k] = sv
			}
		}
	}

	// entitlements
	if ent, found, _ := unstructured.NestedMap(spec, "entitlements"); found {
		e := &ModelEntitlements{}
		if v, _, _ := unstructured.NestedString(ent, "groupClaim"); v != "" {
			e.GroupClaim = v
		}
		if rulesRaw, found, _ := unstructured.NestedSlice(ent, "rules"); found {
			for _, rr := range rulesRaw {
				rm, ok := rr.(map[string]interface{})
				if !ok {
					continue
				}
				rule := EntitlementRule{}
				if v, _, _ := unstructured.NestedString(rm, "group"); v != "" {
					rule.Group = v
				}
				if allow, _, _ := unstructured.NestedStringSlice(rm, "allow"); len(allow) > 0 {
					rule.Allow = allow
				}
				e.Rules = append(e.Rules, rule)
			}
		}
		p.Spec.Entitlements = e
	}

	// onUnentitled
	if ou, found, _ := unstructured.NestedMap(spec, "onUnentitled"); found {
		a := &UnentitledAction{}
		if v, _, _ := unstructured.NestedString(ou, "type"); v != "" {
			a.Type = v
		}
		if v, found, _ := unstructured.NestedInt64(ou, "statusCode"); found {
			a.StatusCode = int(v)
		}
		p.Spec.OnUnentitled = a
	}

	// discovery
	if d, found, _ := unstructured.NestedMap(spec, "discovery"); found {
		disc := &ModelDiscovery{}
		if v, found, _ := unstructured.NestedBool(d, "enabled"); found {
			disc.Enabled = v
		}
		if v, _, _ := unstructured.NestedString(d, "aliasAnnotation"); v != "" {
			disc.AliasAnnotation = v
		}
		if v, _, _ := unstructured.NestedString(d, "tierLabel"); v != "" {
			disc.TierLabel = v
		}
		if nss, _, _ := unstructured.NestedStringSlice(d, "namespaces"); len(nss) > 0 {
			disc.Namespaces = nss
		}
		p.Spec.Discovery = disc
	}

	return p, nil
}

// parseModelBackendTLS reads a tier's backendTLS block. Only references are
// read here; the Secrets themselves are resolved at translation time.
func parseModelBackendTLS(bt map[string]interface{}) *ModelBackendTLS {
	out := &ModelBackendTLS{}
	if v, _, _ := unstructured.NestedString(bt, "sni"); v != "" {
		out.SNI = v
	}
	if v, found, _ := unstructured.NestedBool(bt, "hostCheck"); found {
		out.HostCheck = v
	}
	if ca, found, _ := unstructured.NestedMap(bt, "caSecretRef"); found {
		ref := &BackendTLSCASecretRef{}
		ref.Name, _, _ = unstructured.NestedString(ca, "name")
		ref.Namespace, _, _ = unstructured.NestedString(ca, "namespace")
		ref.Key, _, _ = unstructured.NestedString(ca, "key")
		out.CASecretRef = ref
	}
	if cc, found, _ := unstructured.NestedMap(bt, "clientCertificateSecretRef"); found {
		ref := &BackendTLSSecretRef{}
		ref.Name, _, _ = unstructured.NestedString(cc, "name")
		ref.Namespace, _, _ = unstructured.NestedString(cc, "namespace")
		out.ClientCertificateSecretRef = ref
	}
	return out
}
