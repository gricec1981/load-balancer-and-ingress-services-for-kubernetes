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
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// allHandlerKinds is every policy kind whose handler populates the store: the
// four of handlerKinds plus auth and guardrail (whose delete handlers talk to
// Avi, so the enqueue-order test leaves them out; only adds are driven here).
func allHandlerKinds() []handlerKind {
	ps := SharedPolicyStore
	return append(handlerKinds(),
		handlerKind{
			kind: "AIGatewayAuthPolicy", gvr: AIGatewayAuthPolicyGVR, setup: SetupAuthPolicyEventHandlers,
			spec:   map[string]interface{}{"jwt": map[string]interface{}{"issuer": "https://issuer.example"}},
			forRte: func(r string) int { return len(ps().GetAuthPoliciesForRoute(r)) },
		},
		handlerKind{
			kind: "AIGuardrailPolicy", gvr: AIGuardrailPolicyGVR, setup: SetupGuardrailPolicyEventHandlers,
			spec:   map[string]interface{}{},
			forRte: func(r string) int { return len(ps().GetGuardrailPoliciesForRoute(r)) },
		},
	)
}

// gatedDynamic is a dynamic client whose GETs wait for gate to close.
type gatedDynamic struct {
	dynamic.Interface
	gate <-chan struct{}
}

func (g gatedDynamic) Resource(r schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return gatedNamespaceable{g.Interface.Resource(r), g.gate}
}

type gatedNamespaceable struct {
	dynamic.NamespaceableResourceInterface
	gate <-chan struct{}
}

func (g gatedNamespaceable) Namespace(ns string) dynamic.ResourceInterface {
	return gatedResource{g.NamespaceableResourceInterface.Namespace(ns), g.gate}
}

type gatedResource struct {
	dynamic.ResourceInterface
	gate <-chan struct{}
}

func (g gatedResource) Get(ctx context.Context, name string, opts metav1.GetOptions, sub ...string) (*unstructured.Unstructured, error) {
	<-g.gate
	return g.ResourceInterface.Get(ctx, name, opts, sub...)
}

func gvrListKinds(kinds []handlerKind) map[schema.GroupVersionResource]string {
	out := map[schema.GroupVersionResource]string{}
	for _, k := range kinds {
		out[k.gvr] = k.kind + "List"
	}
	return out
}

// Regression test for the startup window in which a protected route was served
// unauthenticated. The store is populated by the policy event handlers, and an
// informer reports HasSynced as soon as its OWN cache holds the initial list,
// before its handlers have been called for those objects. Startup used to wait
// only on the informers (and registered the handlers after the boot full sync
// had already translated every route). PolicyHandlersSynced / PolicyStoreSynced
// must stay false until every pre-existing policy is in the store.
func TestPolicyStoreSyncWaitsForHandlersNotInformers(t *testing.T) {
	resetPolicyHandlerSync()
	defer resetPolicyHandlerSync()
	kinds := allHandlerKinds()
	ns := "startup-sync"

	var objs []runtime.Object
	for _, k := range kinds {
		objs = append(objs, handlerObj(k, ns, "route-"+strings.ToLower(k.kind)))
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrListKinds(kinds), objs...)
	// Every handler GETs the policy it was handed. Hold those GETs, as a slow
	// API server would, so the informers are synced while the store is empty.
	// (Gated in a wrapper, not a fake reactor: the fake runs reactors under
	// the lock its informers' LIST/WATCH need.)
	release := make(chan struct{})
	handlerClient := gatedDynamic{Interface: dyn, gate: release}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)
	q := &recordingQueue{}
	var informerSynced []cache.InformerSynced
	for _, k := range kinds {
		inf := factory.ForResource(k.gvr)
		k.setup(inf, handlerClient, []workqueue.RateLimitingInterface{q}, 1) //nolint:staticcheck
		informerSynced = append(informerSynced, inf.Informer().HasSynced)
	}
	if got := len(PolicyHandlersSynced()); got != len(kinds) {
		t.Fatalf("%d handler registrations tracked, want %d", got, len(kinds))
	}
	if PolicyStoreSynced() {
		t.Fatal("policy store reported synced before any informer ran")
	}

	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)
	if !cache.WaitForCacheSync(stopCh, informerSynced...) {
		t.Fatal("informers did not sync")
	}
	// The informers are synced; the handlers are still blocked in their GETs.
	if PolicyStoreSynced() {
		t.Fatal("policy store reported synced while the handlers had not stored a single policy")
	}
	if un := PolicyStoreUnsyncedKinds(); len(un) != len(kinds) {
		t.Fatalf("unsynced kinds = %v, want all %d", un, len(kinds))
	}
	for _, k := range kinds {
		if n := k.forRte(ns + "/route-" + strings.ToLower(k.kind)); n != 0 {
			t.Fatalf("%s: store already resolves %d policies with the handler blocked", k.kind, n)
		}
	}

	close(release)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.WaitForCacheSync(stopCh, PolicyHandlersSynced()...)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("policy handlers never reported synced")
	}
	if !PolicyStoreSynced() {
		t.Fatalf("PolicyStoreSynced false after the handlers synced (unsynced: %v)", PolicyStoreUnsyncedKinds())
	}
	// The moment the wait returns, every pre-existing policy is in the store.
	for _, k := range kinds {
		if n := k.forRte(ns + "/route-" + strings.ToLower(k.kind)); n != 1 {
			t.Errorf("%s: store resolves %d policies for its route once synced, want 1", k.kind, n)
		}
	}
}

// A failed or missing handler registration must block startup (fail closed),
// never read as "synced".
func TestPolicyStoreSyncFailsClosedOnBadRegistration(t *testing.T) {
	resetPolicyHandlerSync()
	defer resetPolicyHandlerSync()
	if !PolicyStoreSynced() {
		t.Fatal("no handlers registered must read as synced (nothing to wait for)")
	}
	trackPolicyHandler("AIGatewayAuthPolicy", nil, errors.New("informer stopped"))
	if PolicyStoreSynced() {
		t.Fatal("a registration that failed must never read as synced")
	}
	resetPolicyHandlerSync()
	trackPolicyHandler("AIGatewayAuthPolicy", nil, nil)
	if PolicyStoreSynced() {
		t.Fatal("a nil registration must never read as synced")
	}
}

// A failed live GET used to drop the event: the policy never reached the store
// and, with no informer resync, its route stayed unprotected until the policy
// was next edited. The handler must fall back to the informer's copy.
func TestPolicyHandlerStoresInformerCopyWhenGetFails(t *testing.T) {
	defer resetPolicyHandlerSync()
	for _, k := range allHandlerKinds() {
		t.Run(k.kind, func(t *testing.T) {
			ns := "get-fails-" + strings.ToLower(k.kind)
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{k.gvr: k.kind + "List"})
			dyn.PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("etcdserver: request timed out")
			})
			inf := &capturingInformer{}
			q := &recordingQueue{}
			k.setup(capturingGenericInformer{inf}, dyn, []workqueue.RateLimitingInterface{q}, 1) //nolint:staticcheck
			if inf.handler == nil {
				t.Fatal("no handler registered")
			}
			obj := handlerObj(k, ns, "route-a")
			inf.handler.OnAdd(obj, true)
			if n := k.forRte(ns + "/route-a"); n != 1 {
				t.Fatalf("store resolves %d policies after an add whose GET failed, want 1", n)
			}
			if got := q.take(); len(got) != 1 || got[0] != "HTTPRoute/"+ns+"/route-a" {
				t.Fatalf("add enqueued %v, want [HTTPRoute/%s/route-a]", got, ns)
			}
		})
	}
}
