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
	"sort"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// Startup ordering for the AI policy store.
//
// SharedPolicyStore is written only by the policy informers' event handlers,
// and it is all the graph layer consults when it translates a route: a route
// translated while the store is still missing its policies is built with no
// SSO policy, no DataScripts and no deny rule, i.e. served UNAUTHENTICATED
// until the late Add event re-enqueues it.
//
// An informer's HasSynced only says its own cache holds the initial list; it
// says nothing about whether the handler has been called for those objects.
// So every handler registration is tracked here, and startup waits on the
// registrations' HasSynced (true once every object of the initial list has
// been delivered to the handler and the handler has returned, i.e. the object
// is in the store) before any route is built. PolicyStoreSynced is the gate
// the full sync checks before it translates a single route.

var policyHandlerSync = struct {
	mu     sync.Mutex
	synced map[string]cache.InformerSynced
}{synced: map[string]cache.InformerSynced{}}

func neverSynced() bool { return false }

// trackPolicyHandler records the registration of the event handler that
// populates the store for one policy kind. A failed or missing registration is
// recorded as never synced: startup then blocks (fail closed) rather than
// building routes against a store that can never see that kind's policies.
func trackPolicyHandler(kind string, reg cache.ResourceEventHandlerRegistration, err error) {
	fn := cache.InformerSynced(neverSynced)
	switch {
	case err != nil:
		utils.AviLog.Errorf("%s: the policy event handler was not registered: %v; routes will not be synced", kind, err)
	case reg == nil:
		utils.AviLog.Errorf("%s: the policy event handler registration is nil; routes will not be synced", kind)
	default:
		fn = reg.HasSynced
	}
	policyHandlerSync.mu.Lock()
	defer policyHandlerSync.mu.Unlock()
	policyHandlerSync.synced[kind] = fn
}

// PolicyHandlersSynced returns, for every policy kind whose event handler has
// been registered, a func reporting whether that handler has processed its
// informer's whole initial list. Pass them to cache.WaitForCacheSync together
// with the informers' own HasSynced.
func PolicyHandlersSynced() []cache.InformerSynced {
	policyHandlerSync.mu.Lock()
	defer policyHandlerSync.mu.Unlock()
	kinds := make([]string, 0, len(policyHandlerSync.synced))
	for k := range policyHandlerSync.synced {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	out := make([]cache.InformerSynced, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, policyHandlerSync.synced[k])
	}
	return out
}

// PolicyStoreSynced reports whether every registered policy event handler has
// processed its informer's initial list, so SharedPolicyStore holds every
// policy that existed at startup. No route may be translated before it is true.
func PolicyStoreSynced() bool {
	for _, fn := range PolicyHandlersSynced() {
		if !fn() {
			return false
		}
	}
	return true
}

// PolicyStoreUnsyncedKinds names the policy kinds whose handler has not yet
// processed its initial list (for logs and errors).
func PolicyStoreUnsyncedKinds() []string {
	policyHandlerSync.mu.Lock()
	defer policyHandlerSync.mu.Unlock()
	var out []string
	for k, fn := range policyHandlerSync.synced {
		if !fn() {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// resetPolicyHandlerSync forgets every tracked registration (tests only).
func resetPolicyHandlerSync() {
	policyHandlerSync.mu.Lock()
	defer policyHandlerSync.mu.Unlock()
	policyHandlerSync.synced = map[string]cache.InformerSynced{}
}

// livePolicyObject returns the policy as the API server has it now, falling
// back to the copy the informer delivered with the event when the GET fails.
// A failed GET used to drop the event: the policy never reached the store, and
// with no informer resync nothing retried it, so its route stayed
// unauthenticated until the policy was next edited. The informer's copy is at
// least as new as the event being handled; if the object has since been
// deleted, the Delete event that follows removes it again.
func livePolicyObject(client dynamic.Interface, gvr schema.GroupVersionResource, kind string, u *unstructured.Unstructured) *unstructured.Unstructured {
	if client != nil {
		obj, err := client.Resource(gvr).Namespace(u.GetNamespace()).Get(context.TODO(), u.GetName(), metav1.GetOptions{})
		if err == nil {
			return obj
		}
		utils.AviLog.Warnf("%s %s/%s: live GET failed (%v); using the informer's copy",
			kind, u.GetNamespace(), u.GetName(), err)
	}
	return u
}
