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

// Package aipolicysync pins the gateway-api container's startup ordering: the
// AI policy store must hold every pre-existing policy by the time
// GatewayController.Start returns, because the boot full sync (FullSyncK8s)
// that follows translates every HTTPRoute and pushes it to Avi.
//
// It used to be populated only by handlers registered in
// SetupGatewayApiEventHandlers, i.e. AFTER FullSyncK8s: after an ako-0 restart
// every protected route was pushed without its SSO policy / DataScripts and
// served unauthenticated (~10 s live) until the replayed Add events
// re-enqueued it.
package aipolicysync

import (
	"os"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/aigateway"
	akogatewayapik8s "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/k8s"
	akogatewayapilib "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
	akogatewayapitests "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/tests/gatewayapitests"
)

const ns = "ai"

type policyKind struct {
	gvr    schema.GroupVersionResource
	kind   string
	route  string
	spec   map[string]interface{}
	forRte func(route string) int
}

func policyKinds() []policyKind {
	ps := aigateway.SharedPolicyStore
	return []policyKind{
		{aigateway.AIGatewayAuthPolicyGVR, "AIGatewayAuthPolicy", "chat",
			map[string]interface{}{"jwt": map[string]interface{}{"issuer": "https://issuer.example"}},
			func(r string) int { return len(ps().GetAuthPoliciesForRoute(r)) }},
		{aigateway.AITokenRateLimitPolicyGVR, "AITokenRateLimitPolicy", "chat",
			map[string]interface{}{"limits": []interface{}{map[string]interface{}{
				"name": "l", "key": "consumer", "budget": int64(10), "window": "1h"}}},
			func(r string) int { return len(ps().GetTokenRateLimitPoliciesForRoute(r)) }},
		{aigateway.AIModelRoutePolicyGVR, "AIModelRoutePolicy", "chat",
			map[string]interface{}{"tiers": []interface{}{map[string]interface{}{
				"name": "t", "backendRef": map[string]interface{}{"kind": "Service", "name": "svc"}}}},
			func(r string) int { return len(ps().GetModelRoutePoliciesForRoute(r)) }},
		{aigateway.AIGuardrailPolicyGVR, "AIGuardrailPolicy", "chat",
			map[string]interface{}{},
			func(r string) int { return len(ps().GetGuardrailPoliciesForRoute(r)) }},
		{aigateway.AIMCPRoutePolicyGVR, "AIMCPRoutePolicy", "tools",
			map[string]interface{}{"authRef": map[string]interface{}{"name": "chat-auth"}},
			func(r string) int { return len(ps().GetMCPRoutePoliciesForRoute(r)) }},
		{aigateway.AIA2ARoutePolicyGVR, "AIA2ARoutePolicy", "agents",
			map[string]interface{}{"authRef": map[string]interface{}{"name": "chat-auth"}},
			func(r string) int { return len(ps().GetA2ARoutePoliciesForRoute(r)) }},
	}
}

func policyObj(k policyKind) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	for key, v := range k.spec {
		spec[key] = v
	}
	spec["targetRef"] = map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": k.route}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": aigateway.PolicyGroup + "/" + aigateway.PolicyVersion,
		"kind":       k.kind,
		"metadata":   map[string]interface{}{"name": "p-" + k.kind, "namespace": ns},
		"spec":       spec,
	}}
}

var (
	ctrl   *akogatewayapik8s.GatewayController
	stopCh <-chan struct{}
)

func TestMain(m *testing.M) {
	os.Setenv("AI_GATEWAY_ENABLED", "true")
	os.Setenv(aigateway.JWKSRefreshIntervalEnv, "0")
	os.Setenv("CLUSTER_NAME", "cluster")
	os.Setenv("CLOUD_NAME", "CLOUD_VCENTER")
	os.Setenv("SEG_NAME", "Default-Group")
	os.Setenv("POD_NAMESPACE", utils.AKO_DEFAULT_NS)
	os.Setenv("POD_NAME", "ako-0")

	// The policies exist BEFORE the controller starts, as after a pod restart.
	gvrToKind := map[schema.GroupVersionResource]string{}
	for gvr, kind := range akogatewayapitests.GvrToKind {
		gvrToKind[gvr] = kind
	}
	var objs []runtime.Object
	for _, k := range policyKinds() {
		gvrToKind[k.gvr] = k.kind + "List"
		objs = append(objs, policyObj(k))
	}
	l7 := akogatewayapitests.GetL7RuleFakeData()
	objs = append(objs, &l7)

	akogatewayapitests.KubeClient = k8sfake.NewSimpleClientset()
	akogatewayapitests.GatewayClient = gatewayfake.NewSimpleClientset()
	akogatewayapitests.DynamicClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToKind, objs...)

	_ = lib.AKOControlConfig()
	lib.SetAKOUser(akogatewayapilib.Prefix)
	lib.SetNamePrefix(akogatewayapilib.Prefix)
	akoControlConfig := akogatewayapilib.AKOControlConfig()
	akoControlConfig.SetEventRecorder(lib.AKOGatewayEventComponent, akogatewayapitests.KubeClient, true)
	akogatewayapilib.SetDynamicClientSet(akogatewayapitests.DynamicClient)
	akogatewayapilib.NewDynamicInformers(akogatewayapitests.DynamicClient, false)
	utils.NewInformers(utils.KubeClientIntf{ClientSet: akogatewayapitests.KubeClient},
		[]string{utils.ServiceInformer, utils.SecretInformer, utils.NSInformer, utils.EndpointSlicesInformer},
		map[string]interface{}{})

	// As in InitController: the queues exist (not yet draining) before Start.
	ingestionQueueParams := utils.WorkerQueue{NumWorkers: 1, WorkqueueName: utils.ObjectIngestionLayer}
	statusQueueParams := utils.WorkerQueue{NumWorkers: 1, WorkqueueName: utils.StatusQueue}
	utils.SharedWorkQueue(&ingestionQueueParams, &statusQueueParams)

	ctrl = akogatewayapik8s.SharedGatewayController()
	ctrl.InitGatewayAPIInformers(akogatewayapitests.GatewayClient)
	akoControlConfig.SetGatewayAPIClientset(akogatewayapitests.GatewayClient)
	stopCh = utils.SetupSignalHandler()
	os.Exit(m.Run())
}

var startOnce sync.Once

func start() { startOnce.Do(func() { ctrl.Start(stopCh) }) }

// When Start returns (and before any other handler is set up), the boot full
// sync may translate routes: every pre-existing policy must be in the store.
func TestPolicyStorePopulatedWhenStartReturns(t *testing.T) {
	start()
	if !aigateway.PolicyStoreSynced() {
		t.Fatalf("Start returned with the AI policy store unsynced: %v", aigateway.PolicyStoreUnsyncedKinds())
	}
	for _, k := range policyKinds() {
		if n := k.forRte(ns + "/" + k.route); n != 1 {
			t.Errorf("%s: store resolves %d policies for route %s/%s when Start returns, want 1 "+
				"(the boot full sync would push the route without it)", k.kind, n, ns, k.route)
		}
	}
	if got := len(aigateway.PolicyHandlersSynced()); got != len(policyKinds()) {
		t.Errorf("%d policy handler registrations tracked, want %d", got, len(policyKinds()))
	}
}

// The ingestion queue has not started draining when the store is populated;
// the handlers' route keys must be waiting in it so each protected route is
// (re)built with its policies once it does.
func TestPolicyHandlersEnqueuedRoutesForLaterDrain(t *testing.T) {
	start()
	q := utils.SharedWorkQueue().GetQueueByName(utils.ObjectIngestionLayer)
	want := map[string]bool{}
	for _, k := range policyKinds() {
		want[lib.HTTPRoute+"/"+ns+"/"+k.route] = true
	}
	// The handlers use AddRateLimited, so keys become visible after a short delay.
	got := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < len(want) && time.Now().Before(deadline) {
		for _, wq := range q.Workqueue {
			for wq.Len() > 0 {
				item, _ := wq.Get()
				got[item.(string)] = true
				wq.Done(item)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	for key := range want {
		if !got[key] {
			t.Errorf("route key %s not enqueued by the policy handlers (got %v)", key, got)
		}
	}
}
