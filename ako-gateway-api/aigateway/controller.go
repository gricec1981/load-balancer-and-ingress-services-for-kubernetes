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
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

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

	// modelRoutePolicyByNsName: "namespace/name" → *AIModelRoutePolicy
	modelRoutePolicyByNsName map[string]*AIModelRoutePolicy

	// routeToModelRoutePolicies: routeNsName → []policyNsName
	routeToModelRoutePolicies map[string][]string

	// mcpRoutePolicyByNsName: "namespace/name" → *AIMCPRoutePolicy
	mcpRoutePolicyByNsName map[string]*AIMCPRoutePolicy

	// routeToMCPRoutePolicies: routeNsName → []policyNsName
	routeToMCPRoutePolicies map[string][]string

	// guardrailPolicyByNsName: "namespace/name" → *AIGuardrailPolicy
	guardrailPolicyByNsName map[string]*AIGuardrailPolicy

	// routeToGuardrailPolicies: routeNsName → []policyNsName
	routeToGuardrailPolicies map[string][]string

	// a2aRoutePolicyByNsName: "namespace/name" → *AIA2ARoutePolicy
	a2aRoutePolicyByNsName map[string]*AIA2ARoutePolicy

	// routeToA2ARoutePolicies: routeNsName → []policyNsName
	routeToA2ARoutePolicies map[string][]string
}

var (
	globalPolicyStore *PolicyStore
	policyStoreOnce   sync.Once
)

// SharedPolicyStore returns the process-wide singleton PolicyStore.
func SharedPolicyStore() *PolicyStore {
	policyStoreOnce.Do(func() {
		globalPolicyStore = &PolicyStore{
			authPolicyByNsName:        make(map[string]*AIGatewayAuthPolicy),
			tokenPolicyByNsName:       make(map[string]*AITokenRateLimitPolicy),
			routeToAuthPolicies:       make(map[string][]string),
			routeToTokenPolicies:      make(map[string][]string),
			modelRoutePolicyByNsName:  make(map[string]*AIModelRoutePolicy),
			routeToModelRoutePolicies: make(map[string][]string),
			mcpRoutePolicyByNsName:    make(map[string]*AIMCPRoutePolicy),
			routeToMCPRoutePolicies:   make(map[string][]string),
			guardrailPolicyByNsName:   make(map[string]*AIGuardrailPolicy),
			routeToGuardrailPolicies:  make(map[string][]string),
			a2aRoutePolicyByNsName:    make(map[string]*AIA2ARoutePolicy),
			routeToA2ARoutePolicies:   make(map[string][]string),
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

// Every policy kind is stored the same way: the policy by "ns/name", and an
// index route "ns/targetRef.name" → policy names. Upserts return the targetRef
// name the policy moved away from ("" when it did not move), and deletes return
// the removed policy, so the event handlers can re-enqueue every route whose
// policies changed, and only after the store reflects the change: a rebuild
// dequeued in between must never still see the old mapping.

// upsertAuthPolicy stores the policy and updates route→policy mappings. It
// returns the targetRef name the policy left, or "".
func (s *PolicyStore) upsertAuthPolicy(p *AIGatewayAuthPolicy) (movedFrom string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	var prevTarget *string
	if prev := s.authPolicyByNsName[pNsName]; prev != nil {
		prevTarget = &prev.Spec.TargetRef.Name
	}
	s.authPolicyByNsName[pNsName] = p
	return indexPolicyRoute(s.routeToAuthPolicies, p.Namespace, pNsName, prevTarget, p.Spec.TargetRef.Name)
}

// deleteAuthPolicy removes the policy and cleans route→policy mappings. It
// returns the removed policy, or nil if it was not stored.
func (s *PolicyStore) deleteAuthPolicy(ns, name string) *AIGatewayAuthPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.authPolicyByNsName[pNsName]
	if !ok {
		return nil
	}
	unindexPolicyRoute(s.routeToAuthPolicies, p.Namespace+"/"+p.Spec.TargetRef.Name, pNsName)
	delete(s.authPolicyByNsName, pNsName)
	return p
}

// upsertTokenRateLimitPolicy stores the policy and updates route→policy
// mappings. It returns the targetRef name the policy left, or "".
func (s *PolicyStore) upsertTokenRateLimitPolicy(p *AITokenRateLimitPolicy) (movedFrom string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := p.Namespace + "/" + p.Name
	var prevTarget *string
	if prev := s.tokenPolicyByNsName[pNsName]; prev != nil {
		prevTarget = &prev.Spec.TargetRef.Name
	}
	s.tokenPolicyByNsName[pNsName] = p
	return indexPolicyRoute(s.routeToTokenPolicies, p.Namespace, pNsName, prevTarget, p.Spec.TargetRef.Name)
}

// deleteTokenRateLimitPolicy removes the policy and cleans route→policy
// mappings. It returns the removed policy, or nil if it was not stored.
func (s *PolicyStore) deleteTokenRateLimitPolicy(ns, name string) *AITokenRateLimitPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	pNsName := ns + "/" + name
	p, ok := s.tokenPolicyByNsName[pNsName]
	if !ok {
		return nil
	}
	unindexPolicyRoute(s.routeToTokenPolicies, p.Namespace+"/"+p.Spec.TargetRef.Name, pNsName)
	delete(s.tokenPolicyByNsName, pNsName)
	return p
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
	// A route whose auth could not be realized (fail-closed) or is serving on a
	// last-known-good keyset is re-enqueued after a delay: the informers have no
	// periodic resync, so nothing else would retry it.
	setAuthRetryHook(func(ns, name string, after time.Duration) {
		routeKey := lib.HTTPRoute + "/" + ns + "/" + name
		workqueues[utils.Bkt(ns, numWorkers)].AddAfter(routeKey, after)
	})
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := toUnstructured(obj)
			if !ok {
				return
			}
			p, err := parseAuthPolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AIGatewayAuthPolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertAuthPolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AIGatewayAuthPolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseAuthPolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AIGatewayAuthPolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertAuthPolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AIGatewayAuthPolicy, workqueues, numWorkers)
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
			// Remove from the store first, then re-enqueue the route the removed
			// policy targeted: the rebuild must no longer see the policy.
			if p := SharedPolicyStore().deleteAuthPolicy(ns, name); p != nil {
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AIGatewayAuthPolicy, workqueues, numWorkers)
				// Clean up AKO-managed Avi objects for whichever auth mode was used:
				// OAuth (SSOPolicy, AuthProfile, issuer Pool) or JWT query/header
				// (SSOPolicy, AuthProfile, JWTServerProfile).
				delKey := "AIGatewayAuthPolicy/" + ns + "/" + name
				if p.Spec.EffectiveAuthMode().IsJWTMode() {
					DeleteJWTObjects(delKey, p)
				} else {
					DeleteOAuthObjects(delKey, p)
				}
			}
		},
	}
	// Tracked so startup can wait until this handler has put every
	// existing policy in the store before any route is translated.
	reg, err := informer.Informer().AddEventHandler(handler)
	trackPolicyHandler(lib.AIGatewayAuthPolicy, reg, err)
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
			p, err := parseTokenRateLimitPolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AITokenRateLimitPolicy add: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertTokenRateLimitPolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AITokenRateLimitPolicy, workqueues, numWorkers)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := toUnstructured(newObj)
			if !ok {
				return
			}
			p, err := parseTokenRateLimitPolicy(dynamicClient, u)
			if err != nil {
				utils.AviLog.Warnf("AITokenRateLimitPolicy update: failed to parse %s/%s: %v", u.GetNamespace(), u.GetName(), err)
				return
			}
			movedFrom := SharedPolicyStore().upsertTokenRateLimitPolicy(p)
			enqueuePolicyRoutes(p.Namespace, p.Spec.TargetRef.Name, movedFrom, lib.AITokenRateLimitPolicy, workqueues, numWorkers)
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
			// Remove first, then re-enqueue: the rebuild must not see the policy.
			if p := SharedPolicyStore().deleteTokenRateLimitPolicy(ns, name); p != nil {
				enqueueTargetRoute(ns, p.Spec.TargetRef.Name, lib.AITokenRateLimitPolicy, workqueues, numWorkers)
			}
		},
	}
	// Tracked so startup can wait until this handler has put every
	// existing policy in the store before any route is translated.
	reg, err := informer.Informer().AddEventHandler(handler)
	trackPolicyHandler(lib.AITokenRateLimitPolicy, reg, err)
}

// ─── Parsing ─────────────────────────────────────────────────────────────────

// parseAuthPolicy fetches and parses an AIGatewayAuthPolicy from the API server.
func parseAuthPolicy(client dynamic.Interface, u *unstructured.Unstructured) (*AIGatewayAuthPolicy, error) {
	return unstructuredToAuthPolicy(livePolicyObject(client, AIGatewayAuthPolicyGVR, lib.AIGatewayAuthPolicy, u))
}

// parseTokenRateLimitPolicy fetches and parses an AITokenRateLimitPolicy.
func parseTokenRateLimitPolicy(client dynamic.Interface, u *unstructured.Unstructured) (*AITokenRateLimitPolicy, error) {
	obj := livePolicyObject(client, AITokenRateLimitPolicyGVR, lib.AITokenRateLimitPolicy, u)
	ns, name := obj.GetNamespace(), obj.GetName()
	p, err := unstructuredToTokenRateLimitPolicy(obj)
	if err != nil {
		return nil, err
	}
	// Resolve the admin-token Secret (for the read-only counters endpoint) if the
	// annotation is present. Read via the dynamic client (not an informer cache)
	// so a freshly-created Secret is picked up on the next policy reconcile.
	if secretName := obj.GetAnnotations()[AdminTokenSecretAnnotation]; secretName != "" {
		p.AdminToken = resolveAdminToken(client, ns, secretName)
	}
	// Optional claim gate for the same endpoint, as "<claim>=<value>". Additive:
	// the header gate stays in place, so this can be set and unset freely.
	if claim := obj.GetAnnotations()[AdminClaimAnnotation]; claim != "" {
		claimName, claimValue, ok := strings.Cut(claim, "=")
		if ok && claimName != "" && claimValue != "" {
			p.AdminClaimName, p.AdminClaimValue = claimName, claimValue
		} else {
			utils.AviLog.Warnf("AITokenRateLimitPolicy %s/%s: annotation %s must be \"<claim>=<value>\", got %q — ignored",
				ns, name, AdminClaimAnnotation, claim)
		}
	}
	return p, nil
}

// resolveAdminToken fetches the "token" key from the named Secret in ns and
// returns its decoded value, or "" (with a warning) if it cannot be read. An
// empty result simply suppresses the counters endpoint — it never fails the
// policy reconcile.
func resolveAdminToken(client dynamic.Interface, ns, secretName string) string {
	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	sec, err := client.Resource(secretGVR).Namespace(ns).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		utils.AviLog.Warnf("AITokenRateLimitPolicy: admin-token secret %s/%s not readable: %v", ns, secretName, err)
		return ""
	}
	data, found, _ := unstructured.NestedMap(sec.Object, "data")
	if !found {
		utils.AviLog.Warnf("AITokenRateLimitPolicy: admin-token secret %s/%s has no data", ns, secretName)
		return ""
	}
	enc, ok := data["token"].(string)
	if !ok || enc == "" {
		utils.AviLog.Warnf("AITokenRateLimitPolicy: admin-token secret %s/%s missing key %q", ns, secretName, "token")
		return ""
	}
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		utils.AviLog.Warnf("AITokenRateLimitPolicy: admin-token secret %s/%s value not base64: %v", ns, secretName, err)
		return ""
	}
	return string(dec)
}

// unstructuredToAuthPolicy converts an unstructured object to AIGatewayAuthPolicy.
func unstructuredToAuthPolicy(obj *unstructured.Unstructured) (*AIGatewayAuthPolicy, error) {
	p := &AIGatewayAuthPolicy{}
	p.AdminSkipPath = obj.GetAnnotations()[AdminSkipPathAnnotation]
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

	// authMode (oauthBrowser default | jwtQuery)
	if mode, _, _ := unstructured.NestedString(spec, "authMode"); mode != "" {
		p.Spec.AuthMode = mode
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
	p.CounterEpoch = obj.GetAnnotations()[CounterEpochAnnotation]

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
			if v, _, _ := unstructured.NestedString(lm, "backend"); v != "" {
				tl.Backend = v
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
			if v, _, _ := unstructured.NestedString(lm, "groupHeader"); v != "" {
				tl.GroupHeader = v
			}
			if gb, found, _ := unstructured.NestedMap(lm, "groupBudgets"); found {
				tl.GroupBudgets = make(map[string]int64)
				for gk, gv := range gb {
					switch n := gv.(type) {
					case int64:
						tl.GroupBudgets[gk] = n
					case float64:
						tl.GroupBudgets[gk] = int64(n)
					}
				}
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

	// streaming (absent → Reserve; EffectiveStreaming applies the defaults)
	if sm, found, _ := unstructured.NestedMap(spec, "streaming"); found {
		st := &StreamingPolicy{}
		if v, _, _ := unstructured.NestedString(sm, "mode"); v != "" {
			switch strings.ToLower(v) {
			case "reserve", "deny", "allow":
				st.Mode = v
			default:
				utils.AviLog.Warnf("AITokenRateLimitPolicy %s/%s: unknown streaming.mode %q, using Reserve",
					p.Namespace, p.Name, v)
				st.Mode = StreamingModeReserve
			}
		}
		if v, _, _ := unstructured.NestedInt64(sm, "defaultMaxTokens"); v > 0 {
			st.DefaultMaxTokens = v
		}
		if v, _, _ := unstructured.NestedInt64(sm, "promptCharsPerToken"); v > 0 {
			st.PromptCharsPerToken = int(v)
		}
		p.Spec.Streaming = st
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

// enqueuePolicyRoutes re-enqueues the route a stored policy targets and, when
// the update moved the policy off another route (movedFrom, from an upsert),
// that route too, so its rebuild drops the policy.
func enqueuePolicyRoutes(ns, target, movedFrom, policyKind string, wqs []workqueue.RateLimitingInterface, numWorkers uint32) { //nolint:staticcheck
	if movedFrom != "" && movedFrom != target {
		utils.AviLog.Infof("%s: targetRef moved from HTTPRoute %s/%s to %s/%s; re-enqueuing both",
			policyKind, ns, movedFrom, ns, target)
		enqueueTargetRoute(ns, movedFrom, policyKind, wqs, numWorkers)
	}
	enqueueTargetRoute(ns, target, policyKind, wqs, numWorkers)
}

// indexPolicyRoute maps policy pNsName under route ns/target in index. When
// the policy was stored before (prevTarget non-nil) under a different target,
// that old mapping is dropped, so the route it left no longer resolves it, and
// the old target name is returned; otherwise it returns "". The caller holds
// the store lock.
func indexPolicyRoute(index map[string][]string, ns, pNsName string, prevTarget *string, target string) (movedFrom string) {
	if prevTarget != nil && *prevTarget != target {
		unindexPolicyRoute(index, ns+"/"+*prevTarget, pNsName)
		movedFrom = *prevTarget
	}
	route := ns + "/" + target
	index[route] = addUnique(index[route], pNsName)
	return movedFrom
}

// unindexPolicyRoute removes policy pNsName from route in index, dropping the
// route's entry once it is empty. The caller holds the store lock.
func unindexPolicyRoute(index map[string][]string, route, pNsName string) {
	if rest := removeElem(index[route], pNsName); len(rest) > 0 {
		index[route] = rest
	} else {
		delete(index, route)
	}
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
