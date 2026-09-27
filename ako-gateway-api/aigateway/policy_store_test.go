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
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// ─── authRef parsing (MCP / A2A) ─────────────────────────────────────────────

func routePolicyUnstructured(kind string, authRef interface{}, withAuthRef bool) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"targetRef": map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "tools-route"},
	}
	if withAuthRef {
		spec["authRef"] = authRef
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": PolicyGroup + "/" + PolicyVersion,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": "p", "namespace": "tools"},
		"spec":       spec,
	}}
}

// A declared authRef must survive parsing even when its name is empty or
// missing: dropping it would turn "this route requires auth" into "no auth",
// and the route would be served unauthenticated with no deny. Kept, it fails
// Validate, and the translators' fail-closed path (DenyUnauthenticated before
// skipping the invalid policy) takes over.
func TestRoutePolicyParserKeepsDeclaredAuthRef(t *testing.T) {
	// Each parser returns the parsed authRef and the policy's Validate result.
	parsers := map[string]func(*unstructured.Unstructured) (*AuthPolicyRef, error, error){
		"AIMCPRoutePolicy": func(u *unstructured.Unstructured) (*AuthPolicyRef, error, error) {
			p, err := unstructuredToMCPRoutePolicy(u)
			if err != nil {
				return nil, nil, err
			}
			return p.Spec.AuthRef, p.Spec.Validate(), nil
		},
		"AIA2ARoutePolicy": func(u *unstructured.Unstructured) (*AuthPolicyRef, error, error) {
			p, err := unstructuredToA2ARoutePolicy(u)
			if err != nil {
				return nil, nil, err
			}
			return p.Spec.AuthRef, p.Spec.Validate(), nil
		},
	}
	cases := map[string]struct {
		authRef     interface{}
		withAuthRef bool
		wantRef     bool
		wantName    string
	}{
		"named":                {map[string]interface{}{"name": "llm-auth"}, true, true, "llm-auth"},
		"empty name":           {map[string]interface{}{"name": ""}, true, true, ""},
		"no name":              {map[string]interface{}{}, true, true, ""},
		"null":                 {nil, true, true, ""},
		"name is not a string": {map[string]interface{}{"name": int64(7)}, true, true, ""},
		"absent":               {nil, false, false, ""},
	}
	for kind, parse := range parsers {
		for name, c := range cases {
			t.Run(kind+"/"+name, func(t *testing.T) {
				ref, invalid, err := parse(routePolicyUnstructured(kind, c.authRef, c.withAuthRef))
				if err != nil {
					t.Fatal(err)
				}
				if (ref != nil) != c.wantRef {
					t.Fatalf("AuthRef = %+v, want present=%v", ref, c.wantRef)
				}
				if ref != nil && ref.Name != c.wantName {
					t.Fatalf("AuthRef.Name = %q, want %q", ref.Name, c.wantName)
				}
				// Only a declared authRef without a name makes the policy invalid.
				if wantInvalid := c.wantRef && c.wantName == ""; (invalid != nil) != wantInvalid {
					t.Fatalf("Validate() = %v, want invalid=%v", invalid, wantInvalid)
				}
			})
		}
	}
}

// End to end at the parser boundary: authRef {name: ""} yields an invalid
// policy whose AuthRef is non-nil, which is exactly what the MCP/A2A
// translators fail closed on.
func TestEmptyAuthRefFailsValidation(t *testing.T) {
	mcp, err := unstructuredToMCPRoutePolicy(routePolicyUnstructured("AIMCPRoutePolicy", map[string]interface{}{"name": ""}, true))
	if err != nil {
		t.Fatal(err)
	}
	if mcp.Spec.AuthRef == nil || mcp.Spec.Validate() == nil {
		t.Fatalf("MCP: authRef {name: \"\"} must parse to a non-nil AuthRef that fails Validate (AuthRef=%+v)", mcp.Spec.AuthRef)
	}
	a2a, err := unstructuredToA2ARoutePolicy(routePolicyUnstructured("AIA2ARoutePolicy", map[string]interface{}{"name": ""}, true))
	if err != nil {
		t.Fatal(err)
	}
	if a2a.Spec.AuthRef == nil || a2a.Spec.Validate() == nil {
		t.Fatalf("A2A: authRef {name: \"\"} must parse to a non-nil AuthRef that fails Validate (AuthRef=%+v)", a2a.Spec.AuthRef)
	}
}

// ─── Policy store: targetRef moves and deletes ───────────────────────────────

// storeKind drives one policy kind through the PolicyStore.
type storeKind struct {
	upsert  func(s *PolicyStore, ns, name, target string) string
	del     func(s *PolicyStore, ns, name string) (target string, ok bool)
	forRte  func(s *PolicyStore, route string) int
	indexOf func(s *PolicyStore) map[string][]string
}

func storeKinds() map[string]storeKind {
	return map[string]storeKind{
		"auth": {
			upsert: func(s *PolicyStore, ns, name, target string) string {
				p := &AIGatewayAuthPolicy{}
				p.Namespace, p.Name, p.Spec.TargetRef.Name = ns, name, target
				return s.upsertAuthPolicy(p)
			},
			del: func(s *PolicyStore, ns, name string) (string, bool) {
				if p := s.deleteAuthPolicy(ns, name); p != nil {
					return p.Spec.TargetRef.Name, true
				}
				return "", false
			},
			forRte:  func(s *PolicyStore, r string) int { return len(s.GetAuthPoliciesForRoute(r)) },
			indexOf: func(s *PolicyStore) map[string][]string { return s.routeToAuthPolicies },
		},
		"token": {
			upsert: func(s *PolicyStore, ns, name, target string) string {
				p := &AITokenRateLimitPolicy{}
				p.Namespace, p.Name, p.Spec.TargetRef.Name = ns, name, target
				return s.upsertTokenRateLimitPolicy(p)
			},
			del: func(s *PolicyStore, ns, name string) (string, bool) {
				if p := s.deleteTokenRateLimitPolicy(ns, name); p != nil {
					return p.Spec.TargetRef.Name, true
				}
				return "", false
			},
			forRte:  func(s *PolicyStore, r string) int { return len(s.GetTokenRateLimitPoliciesForRoute(r)) },
			indexOf: func(s *PolicyStore) map[string][]string { return s.routeToTokenPolicies },
		},
		"modelroute": {
			upsert: func(s *PolicyStore, ns, name, target string) string {
				p := &AIModelRoutePolicy{}
				p.Namespace, p.Name, p.Spec.TargetRef.Name = ns, name, target
				return s.upsertModelRoutePolicy(p)
			},
			del: func(s *PolicyStore, ns, name string) (string, bool) {
				if p := s.deleteModelRoutePolicy(ns, name); p != nil {
					return p.Spec.TargetRef.Name, true
				}
				return "", false
			},
			forRte:  func(s *PolicyStore, r string) int { return len(s.GetModelRoutePoliciesForRoute(r)) },
			indexOf: func(s *PolicyStore) map[string][]string { return s.routeToModelRoutePolicies },
		},
		"guardrail": {
			upsert: func(s *PolicyStore, ns, name, target string) string {
				p := &AIGuardrailPolicy{}
				p.Namespace, p.Name, p.Spec.TargetRef.Name = ns, name, target
				return s.upsertGuardrailPolicy(p)
			},
			del: func(s *PolicyStore, ns, name string) (string, bool) {
				if p := s.deleteGuardrailPolicy(ns, name); p != nil {
					return p.Spec.TargetRef.Name, true
				}
				return "", false
			},
			forRte:  func(s *PolicyStore, r string) int { return len(s.GetGuardrailPoliciesForRoute(r)) },
			indexOf: func(s *PolicyStore) map[string][]string { return s.routeToGuardrailPolicies },
		},
		"mcp": {
			upsert: func(s *PolicyStore, ns, name, target string) string {
				return s.upsertMCPRoutePolicy(mcpPolicyFor(ns, name, target, ""))
			},
			del: func(s *PolicyStore, ns, name string) (string, bool) {
				if p := s.deleteMCPRoutePolicy(ns, name); p != nil {
					return p.Spec.TargetRef.Name, true
				}
				return "", false
			},
			forRte:  func(s *PolicyStore, r string) int { return len(s.GetMCPRoutePoliciesForRoute(r)) },
			indexOf: func(s *PolicyStore) map[string][]string { return s.routeToMCPRoutePolicies },
		},
		"a2a": {
			upsert: func(s *PolicyStore, ns, name, target string) string {
				p := &AIA2ARoutePolicy{}
				p.Namespace, p.Name, p.Spec.TargetRef.Name = ns, name, target
				return s.upsertA2ARoutePolicy(p)
			},
			del: func(s *PolicyStore, ns, name string) (string, bool) {
				if p := s.deleteA2ARoutePolicy(ns, name); p != nil {
					return p.Spec.TargetRef.Name, true
				}
				return "", false
			},
			forRte:  func(s *PolicyStore, r string) int { return len(s.GetA2ARoutePoliciesForRoute(r)) },
			indexOf: func(s *PolicyStore) map[string][]string { return s.routeToA2ARoutePolicies },
		},
	}
}

// Re-targeting a policy moves it: the route it left no longer resolves it (it
// used to keep the policy for good), the upsert reports the route it left so
// the handler can rebuild it, and a same-target update reports nothing. A
// delete returns the removed policy so the handler can enqueue after removal.
func TestPolicyStoreTargetRefMoveAndDelete(t *testing.T) {
	for kind, k := range storeKinds() {
		t.Run(kind, func(t *testing.T) {
			s := newTestPolicyStore()
			if moved := k.upsert(s, "ai", "p", "route-a"); moved != "" {
				t.Fatalf("first upsert reported a move from %q", moved)
			}
			k.upsert(s, "ai", "other", "route-a") // a second policy on the same route stays put
			if moved := k.upsert(s, "ai", "p", "route-a"); moved != "" {
				t.Fatalf("same-target update reported a move from %q", moved)
			}
			if n := k.forRte(s, "ai/route-a"); n != 2 {
				t.Fatalf("route-a has %d policies, want 2", n)
			}

			if moved := k.upsert(s, "ai", "p", "route-b"); moved != "route-a" {
				t.Fatalf("re-targeting upsert returned %q, want the route it left (route-a)", moved)
			}
			if n := k.forRte(s, "ai/route-a"); n != 1 {
				t.Fatalf("route-a still resolves %d policies after p moved away, want 1 (only 'other')", n)
			}
			if n := k.forRte(s, "ai/route-b"); n != 1 {
				t.Fatalf("route-b resolves %d policies, want 1", n)
			}

			target, ok := k.del(s, "ai", "p")
			if !ok || target != "route-b" {
				t.Fatalf("delete returned (%q, %v), want the removed policy targeting route-b", target, ok)
			}
			if n := k.forRte(s, "ai/route-b"); n != 0 {
				t.Fatalf("route-b resolves %d policies after delete, want 0", n)
			}
			if _, stale := k.indexOf(s)["ai/route-b"]; stale {
				t.Error("an emptied route must be dropped from the index")
			}
			if _, ok := k.del(s, "ai", "p"); ok {
				t.Error("deleting a policy that is not stored must return nil")
			}
		})
	}
}

// ─── Event handlers: enqueue order ───────────────────────────────────────────

// capturingInformer records the handler a Setup*EventHandlers call registers.
type capturingInformer struct {
	cache.SharedIndexInformer
	handler cache.ResourceEventHandler
}

func (c *capturingInformer) AddEventHandler(h cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	c.handler = h
	return nil, nil
}

type capturingGenericInformer struct{ inf *capturingInformer }

func (g capturingGenericInformer) Informer() cache.SharedIndexInformer { return g.inf }
func (g capturingGenericInformer) Lister() cache.GenericLister         { return nil }

var _ informers.GenericInformer = capturingGenericInformer{}

// recordingQueue records rate-limited adds and runs onAdd at the moment of
// each one, standing in for a worker that dequeues immediately.
type recordingQueue struct {
	workqueue.RateLimitingInterface //nolint:staticcheck
	mu                              sync.Mutex
	added                           []string
	onAdd                           func(item string)
}

func (q *recordingQueue) AddRateLimited(item interface{}) {
	q.mu.Lock()
	q.added = append(q.added, item.(string))
	fn := q.onAdd
	q.mu.Unlock()
	if fn != nil {
		fn(item.(string))
	}
}

func (q *recordingQueue) take() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.added
	q.added = nil
	return out
}

type handlerKind struct {
	kind  string
	gvr   schema.GroupVersionResource
	setup func(informers.GenericInformer, dynamic.Interface, []workqueue.RateLimitingInterface, uint32) //nolint:staticcheck
	// spec is the policy spec without targetRef.
	spec map[string]interface{}
	// forRte counts the kind's policies the SHARED store resolves for a route.
	forRte func(route string) int
}

func handlerKinds() []handlerKind {
	ps := SharedPolicyStore
	return []handlerKind{
		{
			kind: "AITokenRateLimitPolicy", gvr: AITokenRateLimitPolicyGVR, setup: SetupTokenRateLimitPolicyEventHandlers,
			spec: map[string]interface{}{"limits": []interface{}{map[string]interface{}{
				"name": "l", "key": "consumer", "budget": int64(10), "window": "1h"}}},
			forRte: func(r string) int { return len(ps().GetTokenRateLimitPoliciesForRoute(r)) },
		},
		{
			kind: "AIMCPRoutePolicy", gvr: AIMCPRoutePolicyGVR, setup: SetupMCPRoutePolicyEventHandlers,
			spec:   map[string]interface{}{},
			forRte: func(r string) int { return len(ps().GetMCPRoutePoliciesForRoute(r)) },
		},
		{
			kind: "AIA2ARoutePolicy", gvr: AIA2ARoutePolicyGVR, setup: SetupA2ARoutePolicyEventHandlers,
			spec:   map[string]interface{}{},
			forRte: func(r string) int { return len(ps().GetA2ARoutePoliciesForRoute(r)) },
		},
		{
			// The delete handler also tears down REST-authored Avi objects, which
			// needs a Controller; only add/update are driven for this kind.
			kind: "AIModelRoutePolicy", gvr: AIModelRoutePolicyGVR, setup: SetupModelRoutePolicyEventHandlers,
			spec: map[string]interface{}{"tiers": []interface{}{map[string]interface{}{
				"name": "t", "backendRef": map[string]interface{}{"kind": "Service", "name": "svc"}}}},
			forRte: func(r string) int { return len(ps().GetModelRoutePoliciesForRoute(r)) },
		},
	}
}

func handlerObj(k handlerKind, ns, target string) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	for key, v := range k.spec {
		spec[key] = v
	}
	spec["targetRef"] = map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": target}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": PolicyGroup + "/" + PolicyVersion,
		"kind":       k.kind,
		"metadata":   map[string]interface{}{"name": "p", "namespace": ns},
		"spec":       spec,
	}}
}

// The handlers must (1) re-enqueue BOTH routes when an update moves a policy,
// each only after the store stopped mapping it to the route it left, and (2) on
// delete, remove the policy from the store before enqueueing its route, so a
// worker that dequeues at once rebuilds without it.
func TestPolicyHandlersEnqueueAfterStoreChange(t *testing.T) {
	for _, k := range handlerKinds() {
		t.Run(k.kind, func(t *testing.T) {
			ns := "handler-test-" + strings.ToLower(k.kind)
			routeA, routeB := ns+"/route-a", ns+"/route-b"
			keyA, keyB := "HTTPRoute/"+routeA, "HTTPRoute/"+routeB

			inf := &capturingInformer{}
			q := &recordingQueue{}
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{k.gvr: k.kind + "List"})
			k.setup(capturingGenericInformer{inf}, dyn, []workqueue.RateLimitingInterface{q}, 1) //nolint:staticcheck
			if inf.handler == nil {
				t.Fatal("no handler registered")
			}
			res := dyn.Resource(k.gvr).Namespace(ns)

			// Add, targeting route-a.
			objA := handlerObj(k, ns, "route-a")
			if _, err := res.Create(context.TODO(), objA, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			inf.handler.OnAdd(objA, false)
			if got := q.take(); len(got) != 1 || got[0] != keyA {
				t.Fatalf("add enqueued %v, want [%s]", got, keyA)
			}

			// Update, re-targeting to route-b: both routes rebuilt, route-a no
			// longer resolving the policy when its rebuild is enqueued.
			var bad []string
			q.onAdd = func(item string) {
				if item == keyA && k.forRte(routeA) != 0 {
					bad = append(bad, "route-a enqueued while the store still maps the policy to it")
				}
				if item == keyB && k.forRte(routeB) != 1 {
					bad = append(bad, "route-b enqueued before the store maps the policy to it")
				}
			}
			objB := handlerObj(k, ns, "route-b")
			if _, err := res.Update(context.TODO(), objB, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			inf.handler.OnUpdate(objA, objB)
			got := q.take()
			if len(got) != 2 || got[0] != keyA || got[1] != keyB {
				t.Fatalf("re-targeting update enqueued %v, want [%s %s]", got, keyA, keyB)
			}
			if len(bad) > 0 {
				t.Fatal(strings.Join(bad, "; "))
			}
			if n := k.forRte(routeA); n != 0 {
				t.Fatalf("route-a still resolves %d policies after the move", n)
			}

			// Unchanged target: only route-b.
			inf.handler.OnUpdate(objB, objB)
			if got := q.take(); len(got) != 1 || got[0] != keyB {
				t.Fatalf("same-target update enqueued %v, want [%s]", got, keyB)
			}

			if k.kind == "AIModelRoutePolicy" {
				SharedPolicyStore().deleteModelRoutePolicy(ns, "p")
				return
			}
			// Delete: removed from the store before route-b is enqueued.
			q.onAdd = func(item string) {
				if item == keyB && k.forRte(routeB) != 0 {
					bad = append(bad, "route-b enqueued while the deleted policy is still stored")
				}
			}
			inf.handler.OnDelete(objB)
			if got := q.take(); len(got) != 1 || got[0] != keyB {
				t.Fatalf("delete enqueued %v, want [%s]", got, keyB)
			}
			if len(bad) > 0 {
				t.Fatal(strings.Join(bad, "; "))
			}
			// A tombstone for an already-removed policy enqueues nothing.
			inf.handler.OnDelete(cache.DeletedFinalStateUnknown{Key: ns + "/p", Obj: objB})
			if got := q.take(); len(got) != 0 {
				t.Fatalf("deleting an unknown policy enqueued %v", got)
			}
		})
	}
}
