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
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// AIGatewayAuthPolicyGVR is the GroupVersionResource for AIGatewayAuthPolicy.
var AIGatewayAuthPolicyGVR = schema.GroupVersionResource{
	Group:    PolicyGroup,
	Version:  PolicyVersion,
	Resource: "aigatewayauthpolicies",
}

// AITokenRateLimitPolicyGVR is the GroupVersionResource for AITokenRateLimitPolicy.
var AITokenRateLimitPolicyGVR = schema.GroupVersionResource{
	Group:    PolicyGroup,
	Version:  PolicyVersion,
	Resource: "aitokenratelimitpolicies",
}

// PolicyStore is the in-process index that maps HTTPRoute keys to the AI gateway
// policy objects targeting them, and vice versa.  It is written by event handlers
// and read by the graph-layer translator.
type PolicyStore struct {
	mu sync.RWMutex

	// authPolicyByNsName: "namespace/name" → *AIGatewayAuthPolicy
	authPolicyByNsName map[string]*AIGatewayAuthPolicy

	// tokenPolicyByNsName: "namespace/name" → *AITokenRateLimitPolicy
	tokenPolicyByNsName map[string]*AITokenRateLimitPolicy

	// routeToAuthPolicies: routeNsName → []policyNsName
	routeToAuthPolicies map[string][]string

	// routeToTokenPolicies: routeNsName → []policyNsName
	routeToTokenPolicies map[string][]string
}

var (
	globalPolicyStore     *PolicyStore
	policyStoreOnce       sync.Once
)

// SharedPolicyStore returns the process-wide singleton PolicyStore.
func SharedPolicyStore() *PolicyStore {
	policyStoreOnce.Do(func() {
		globalPolicyStore = &PolicyStore{
			authPolicyByNsName:   make(map[string]*AIGatewayAuthPolicy),
			tokenPolicyByNsName:  make(map[string]*AITokenRateLimitPolicy),
			routeToAuthPolicies:  make(map[string][]string),
			routeToTokenPolicies: make(map[string][]string),
		}
	})
	return globalPolicyStore
}

// GetAuthPoliciesForRoute returns all AIGatewayAuthPolicies targeting the route.
func (s *PolicyStore) GetAuthPoliciesForRoute(routeNsName string) []*AIGatewayAuthPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*AIGatewayAuthPolicy
	for _, pNsName := range s.routeToAuthPolicies[routeNsName] {
		if p, ok := s.authPolicyByNsName[pNsName]; ok {
			out = append(out, p)
		}
	}
	return out
}

// GetTokenRateLimitPoliciesForRoute returns all AITokenRateLimitPolicies targeting the route.
func (s *PolicyStore) GetTokenRateLimitPoliciesForRoute(routeNsName string) []*AITokenRateLimitPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*AITokenRateLimitPolicy
	for _, pNsName := range s.routeToTokenPolicies[routeNsName] {
		if p, ok := s.tokenPolicyByNsName[pNsName]; ok {
			out = append(out, p)
		}
	}
	return out
}

// upsertAuthPolicy stores the policy and updates route→policy mappings.
func (s *PolicyStore) upsertAuthPolicy(p *AIGatewayAuthPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	s.authPolicyByNsName[pNsName] = p
	routeNsName := p.Namespace + "/" + p.Spec.TargetRef.Name
	s.routeToAuthPolicies[routeNsName] = addUnique(s.routeToAuthPolicies[routeNsName], pNsName)
}

// deleteAuthPolicy removes the policy and cleans route→policy mappings.
func (s *PolicyStore) deleteAuthPolicy(ns, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.authPolicyByNsName[pNsName]
	if !ok {
		return
	}
	routeNsName := p.Namespace + "/" + p.Spec.TargetRef.Name
	s.routeToAuthPolicies[routeNsName] = removeElem(s.routeToAuthPolicies[routeNsName], pNsName)
	delete(s.authPolicyByNsName, pNsName)
}

// upsertTokenRateLimitPolicy stores the policy and updates route→policy mappings.
func (s *PolicyStore) upsertTokenRateLimitPolicy(p *AITokenRateLimitPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	s.tokenPolicyByNsName[pNsName] = p
	routeNsName := p.Namespace + "/" + p.Spec.TargetRef.Name
	s.routeToTokenPolicies[routeNsName] = addUnique(s.routeToTokenPolicies[routeNsName], pNsName)
}

// deleteTokenRateLimitPolicy removes the policy and cleans route→policy mappings.
func (s *PolicyStore) deleteTokenRateLimitPolicy(ns, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.tokenPolicyByNsName[pNsName]
	if !ok {
		return
	}
	routeNsName := p.Namespace + "/" + p.Spec.TargetRef.Name
	s.routeToTokenPolicies[routeNsName] = removeElem(s.routeToTokenPolicies[routeNsName], pNsName)
	delete(s.tokenPolicyByNsName, pNsName)
}

// ─── Event handlers ──────────────────────────────────────────────────────────

// SetupAuthPolicyEventHandlers wires Add/Update/Delete handlers for AIGatewayAuthPolicy.
// On any change it re-enqueues the targeted HTTPRoute so the graph layer rebuilds
// the VS model with updated JWT configuration.
func SetupAuthPolicyEventHandlers(
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
			p, err := parseAuthPolicy(dynamicClient, u.GetNamespace(), u.GetName())
			if err != nil {
				utils.AviLog.Warnf("AIGatewayAuthPolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			SharedPolicyStore().upsertAuthPolicy(p)
			enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIGatewayAuthPolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseAuthPolicy(dynamicClient, u.GetNamespace(), u.GetName())
			if err != nil {
				utils.AviLog.Warnf("AIGatewayAuthPolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			SharedPolicyStore().upsertAuthPolicy(p)
			enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AIGatewayAuthPolicy, workqueues, numWorkers)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utils.AviLog.Errorf("AIGatewayAuthPolicy delete: couldn't get object from tombstone %#v", obj)
					return
				}
				u, ok = tombstone.Obj.(*unstructured.Unstructured)
				if !ok {
					return
				}
			}
			ns, name := u.GetNamespace(), u.GetName()
			// Re-enqueue before deleting so the translator sees the last known targetRef.
			ps := SharedPolicyStore()
			pNsName := ns + "/" + name
			ps.mu.RLock()
			p := ps.authPolicyByNsName[pNsName]
			ps.mu.RUnlock()
			if p != nil {
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AIGatewayAuthPolicy, workqueues, numWorkers)
			}
			ps.deleteAuthPolicy(ns, name)
		},
	}
	informer.Informer().AddEventHandler(handler)
}

// SetupTokenRateLimitPolicyEventHandlers wires Add/Update/Delete handlers for
// AITokenRateLimitPolicy.
func SetupTokenRateLimitPolicyEventHandlers(
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
			p, err := parseTokenRateLimitPolicy(dynamicClient, u.GetNamespace(), u.GetName())
			if err != nil {
				utils.AviLog.Warnf("AITokenRateLimitPolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			SharedPolicyStore().upsertTokenRateLimitPolicy(p)
			enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AITokenRateLimitPolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseTokenRateLimitPolicy(dynamicClient, u.GetNamespace(), u.GetName())
			if err != nil {
				utils.AviLog.Warnf("AITokenRateLimitPolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			SharedPolicyStore().upsertTokenRateLimitPolicy(p)
			enqueueTargetRoute(p.Namespace, p.Spec.TargetRef.Name, lib.AITokenRateLimitPolicy, workqueues, numWorkers)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utils.AviLog.Errorf("AITokenRateLimitPolicy delete: couldn't get object from tombstone %#v", obj)
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
			p := ps.tokenPolicyByNsName[pNsName]
			ps.mu.RUnlock()
			if p != nil {
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AITokenRateLimitPolicy, workqueues, numWorkers)
			}
			ps.deleteTokenRateLimitPolicy(ns, name)
		},
	}
	informer.Informer().AddEventHandler(handler)
}

// ─── Parsing ─────────────────────────────────────────────────────────────────

// parseAuthPolicy fetches and parses an AIGatewayAuthPolicy from the API server.
func parseAuthPolicy(client dynamic.Interface, ns, name string) (*AIGatewayAuthPolicy, error) {
	obj, err := client.Resource(AIGatewayAuthPolicyGVR).Namespace(ns).Get(
		context.TODO(), name, metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("get AIGatewayAuthPolicy %s/%s: %w", ns, name, err)
	}
	return unstructuredToAuthPolicy(obj)
}

// parseTokenRateLimitPolicy fetches and parses an AITokenRateLimitPolicy.
func parseTokenRateLimitPolicy(client dynamic.Interface, ns, name string) (*AITokenRateLimitPolicy, error) {
	obj, err := client.Resource(AITokenRateLimitPolicyGVR).Namespace(ns).Get(
		context.TODO(), name, metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("get AITokenRateLimitPolicy %s/%s: %w", ns, name, err)
	}
	return unstructuredToTokenRateLimitPolicy(obj)
}

// unstructuredToAuthPolicy converts an unstructured object to AIGatewayAuthPolicy.
func unstructuredToAuthPolicy(obj *unstructured.Unstructured) (*AIGatewayAuthPolicy, error) {
	p := &AIGatewayAuthPolicy{}
	p.Name = obj.GetName()
	p.Namespace = obj.GetNamespace()

	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("spec not found in AIGatewayAuthPolicy %s/%s", p.Namespace, p.Name)
	}

	// targetRef
	if group, _, _ := unstructured.NestedString(spec, "targetRef", "group"); group != "" {
		p.Spec.TargetRef.Group = group
	}
	if kind, _, _ := unstructured.NestedString(spec, "targetRef", "kind"); kind != "" {
		p.Spec.TargetRef.Kind = kind
	}
	if name, _, _ := unstructured.NestedString(spec, "targetRef", "name"); name != "" {
		p.Spec.TargetRef.Name = name
	}

	// jwt
	if issuer, _, _ := unstructured.NestedString(spec, "jwt", "issuer"); issuer != "" {
		p.Spec.JWT.Issuer = issuer
	}
	if jwksUri, _, _ := unstructured.NestedString(spec, "jwt", "jwksUri"); jwksUri != "" {
		p.Spec.JWT.JwksUri = jwksUri
	}
	if claim, _, _ := unstructured.NestedString(spec, "jwt", "identityClaim"); claim != "" {
		p.Spec.JWT.IdentityClaim = claim
	}
	if audiences, _, _ := unstructured.NestedStringSlice(spec, "jwt", "audiences"); len(audiences) > 0 {
		p.Spec.JWT.Audiences = audiences
	}
	if fwd, _, _ := unstructured.NestedStringSlice(spec, "jwt", "forwardClaims"); len(fwd) > 0 {
		p.Spec.JWT.ForwardClaims = fwd
	}

	if hdr, _, _ := unstructured.NestedString(spec, "identityHeader"); hdr != "" {
		p.Spec.IdentityHeader = hdr
	}

	if sc, found, _ := unstructured.NestedInt64(spec, "onFailure", "statusCode"); found {
		p.Spec.OnFailure = &AuthFailureAction{StatusCode: int(sc)}
	}

	return p, nil
}

// unstructuredToTokenRateLimitPolicy converts an unstructured object to AITokenRateLimitPolicy.
func unstructuredToTokenRateLimitPolicy(obj *unstructured.Unstructured) (*AITokenRateLimitPolicy, error) {
	p := &AITokenRateLimitPolicy{}
	p.Name = obj.GetName()
	p.Namespace = obj.GetNamespace()

	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("spec not found in AITokenRateLimitPolicy %s/%s", p.Namespace, p.Name)
	}

	// targetRef
	if group, _, _ := unstructured.NestedString(spec, "targetRef", "group"); group != "" {
		p.Spec.TargetRef.Group = group
	}
	if kind, _, _ := unstructured.NestedString(spec, "targetRef", "kind"); kind != "" {
		p.Spec.TargetRef.Kind = kind
	}
	if name, _, _ := unstructured.NestedString(spec, "targetRef", "name"); name != "" {
		p.Spec.TargetRef.Name = name
	}

	// identitySource
	if hdr, _, _ := unstructured.NestedString(spec, "identitySource", "header"); hdr != "" {
		if p.Spec.IdentitySource == nil {
			p.Spec.IdentitySource = &IdentitySource{}
		}
		p.Spec.IdentitySource.Header = hdr
	}
	if fb, _, _ := unstructured.NestedString(spec, "identitySource", "fallback"); fb != "" {
		if p.Spec.IdentitySource == nil {
			p.Spec.IdentitySource = &IdentitySource{}
		}
		p.Spec.IdentitySource.Fallback = fb
	}

	// limits
	limitsRaw, found, _ := unstructured.NestedSlice(spec, "limits")
	if found {
		for _, lr := range limitsRaw {
			lm, ok := lr.(map[string]interface{})
			if !ok {
				continue
			}
			tl := TokenLimit{}
			if v, _, _ := unstructured.NestedString(lm, "name"); v != "" {
				tl.Name = v
			}
			if v, _, _ := unstructured.NestedString(lm, "key"); v != "" {
				tl.Key = v
			}
			if v, _, _ := unstructured.NestedString(lm, "tokens"); v != "" {
				tl.Tokens = v
			}
			if v, _, _ := unstructured.NestedInt64(lm, "budget"); v > 0 {
				tl.Budget = v
			}
			if v, _, _ := unstructured.NestedString(lm, "window"); v != "" {
				tl.Window = v
			}
			// action
			if actionType, _, _ := unstructured.NestedString(lm, "action", "type"); actionType != "" {
				sc, _, _ := unstructured.NestedInt64(lm, "action", "statusCode")
				ra, _, _ := unstructured.NestedBool(lm, "action", "retryAfter")
				tl.Action = &LimitAction{
					Type:       actionType,
					StatusCode: int(sc),
					RetryAfter: ra,
				}
			}
			p.Spec.Limits = append(p.Spec.Limits, tl)
		}
	}

	// requestRateLimit
	if rps, found, _ := unstructured.NestedInt64(spec, "requestRateLimit", "requestsPerSecond"); found && rps > 0 {
		rl := &RequestRateLimit{RequestsPerSecond: int(rps)}
		if burst, _, _ := unstructured.NestedInt64(spec, "requestRateLimit", "burst"); burst > 0 {
			rl.Burst = int(burst)
		}
		if key, _, _ := unstructured.NestedString(spec, "requestRateLimit", "key"); key != "" {
			rl.Key = key
		}
		p.Spec.RequestRateLimit = rl
	}

	return p, nil
}

// ─── Utilities ───────────────────────────────────────────────────────────────

func toUnstructured(obj interface{}) (*unstructured.Unstructured, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	return u, ok
}

// enqueueTargetRoute re-enqueues the HTTPRoute targeted by the policy so the
// graph layer rebuilds its VS model with the new policy configuration.
func enqueueTargetRoute(ns, routeName, policyKind string, wqs []workqueue.RateLimitingInterface, numWorkers uint32) { //nolint:staticcheck
	routeKey := lib.HTTPRoute + "/" + ns + "/" + routeName
	bkt := utils.Bkt(ns, numWorkers)
	wqs[bkt].AddRateLimited(routeKey)
	utils.AviLog.Debugf("%s: re-enqueued HTTPRoute %s", policyKind, routeKey)
}

func addUnique(slice []string, elem string) []string {
	for _, s := range slice {
		if s == elem {
			return slice
		}
	}
	return append(slice, elem)
}

func removeElem(slice []string, elem string) []string {
	out := slice[:0]
	for _, s := range slice {
		if s != elem {
			out = append(out, s)
		}
	}
	return out
}
