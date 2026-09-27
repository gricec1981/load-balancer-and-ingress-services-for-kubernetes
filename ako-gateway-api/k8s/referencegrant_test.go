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

package k8s

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

func fakeGatewayClientset(serveReferenceGrants bool) *gatewayfake.Clientset {
	cs := gatewayfake.NewSimpleClientset()
	if serveReferenceGrants {
		cs.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
			GroupVersion: gatewayv1beta1.GroupVersion.String(),
			APIResources: []metav1.APIResource{{Name: "referencegrants", Namespaced: true, Kind: "ReferenceGrant"}},
		}}
	}
	return cs
}

func forbidReferenceGrantList(cs *gatewayfake.Clientset, err error) {
	cs.PrependReactor("list", "referencegrants", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

// The informer is created only when the resource is served AND AKO may list it:
// an informer the ClusterRole forbids never syncs and would block Start (and
// with it every route) forever.
func TestReferenceGrantWatchable(t *testing.T) {
	if ok, _ := referenceGrantWatchable(fakeGatewayClientset(true)); !ok {
		t.Fatal("served and listable: informer must be created")
	}
	if ok, reason := referenceGrantWatchable(fakeGatewayClientset(false)); ok || reason == "" {
		t.Fatalf("not served: got ok=%t reason=%q", ok, reason)
	}

	gr := schema.GroupResource{Group: gatewayv1beta1.GroupName, Resource: "referencegrants"}
	cs := fakeGatewayClientset(true)
	forbidReferenceGrantList(cs, apierrors.NewForbidden(gr, "", errors.New("RBAC: list not granted")))
	if ok, reason := referenceGrantWatchable(cs); ok || reason == "" {
		t.Fatalf("Forbidden list must skip the informer, got ok=%t reason=%q", ok, reason)
	}

	cs = fakeGatewayClientset(true)
	forbidReferenceGrantList(cs, apierrors.NewUnauthorized("no"))
	if ok, _ := referenceGrantWatchable(cs); ok {
		t.Fatal("Unauthorized list must skip the informer")
	}

	// A transient failure keeps the informer: the reflector retries it.
	cs = fakeGatewayClientset(true)
	forbidReferenceGrantList(cs, apierrors.NewServiceUnavailable("etcd"))
	if ok, _ := referenceGrantWatchable(cs); !ok {
		t.Fatal("transient list error must keep the informer")
	}

	if ok, _ := referenceGrantWatchable(nil); ok {
		t.Fatal("nil clientset must not create the informer")
	}
}

// Informer resyncs re-deliver every grant as an update with an unchanged
// ResourceVersion; those (and metadata-only edits) must not re-translate routes.
func TestReferenceGrantChanged(t *testing.T) {
	grant := func(rv, fromNs string) *gatewayv1beta1.ReferenceGrant {
		return &gatewayv1beta1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Namespace: "pais", Name: "gateway-to-pais", ResourceVersion: rv},
			Spec: gatewayv1beta1.ReferenceGrantSpec{
				From: []gatewayv1beta1.ReferenceGrantFrom{{Group: "ai.ako.vmware.com", Kind: "AIModelRoutePolicy", Namespace: gatewayv1beta1.Namespace(fromNs)}},
				To:   []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
			},
		}
	}
	if referenceGrantChanged(grant("1", "gateway"), grant("1", "gateway")) {
		t.Error("resync (same ResourceVersion) must not count as a change")
	}
	labelled := grant("2", "gateway")
	labelled.Labels = map[string]string{"x": "y"}
	if referenceGrantChanged(grant("1", "gateway"), labelled) {
		t.Error("metadata-only update must not count as a change")
	}
	if !referenceGrantChanged(grant("1", "gateway"), grant("2", "other")) {
		t.Error("spec change must count")
	}
	if !referenceGrantChanged(nil, grant("2", "gateway")) {
		t.Error("unexpected types must fall through to a re-translate")
	}
}
