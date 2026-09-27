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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	akogatewayapilib "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/lib"
	akogatewayapiobjects "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/objects"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	akov1beta1 "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/apis/ako/v1beta1"
	v1beta1fake "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/client/v1beta1/clientset/versioned/fake"
	v1beta1informers "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/client/v1beta1/informers/externalversions"
)

const (
	globalT1 = "/infra/tier-1s/global-t1"
	infraT1  = "/orgs/default/projects/default/vpcs/gw-vpc"
)

// withInfraSettings binds gateway ("ns/name") to an AviInfraSetting with the
// given status and T1LR (nil = none), served from an in-memory informer, and
// sets the AKO-wide NSXT_T1_LR, for one test.
func withInfraSettings(t *testing.T, gateway, status string, t1 *string) {
	t.Helper()
	t.Setenv("NSXT_T1_LR", globalT1)
	is := &akov1beta1.AviInfraSetting{ObjectMeta: metav1.ObjectMeta{Name: "gw-infra"}}
	is.Spec.NSXSettings.T1LR = t1
	is.Status.Status = status
	informer := v1beta1informers.NewSharedInformerFactory(v1beta1fake.NewSimpleClientset(), 0).Ako().V1beta1().AviInfraSettings()
	if err := informer.Informer().GetIndexer().Add(is); err != nil {
		t.Fatal(err)
	}
	cfg := akogatewayapilib.AKOControlConfig()
	prev := cfg.AviInfraSettingInformer()
	cfg.SetAviInfraSettingInformer(informer)
	akogatewayapiobjects.GatewayApiLister().UpdateGatewayToAviInfraSettingMappings(gateway, is.Name)
	t.Cleanup(func() {
		akogatewayapiobjects.GatewayApiLister().DeleteGatewayToAviInfraSettingMappings(gateway)
		cfg.SetAviInfraSettingInformer(prev)
	})
}

// The REST-authored AI pools resolve the Tier-1 exactly like BuildPGPool: the
// AKO-wide path, overridden only by an ACCEPTED AviInfraSetting on the Gateway.
func TestGatewayTier1LR(t *testing.T) {
	t1 := infraT1
	empty := ""
	cases := map[string]struct {
		status string
		t1     *string
		gw     string // Gateway looked up
		want   string
	}{
		"accepted setting overrides":              {lib.StatusAccepted, &t1, "ai/gw", infraT1},
		"rejected setting is ignored":             {lib.StatusRejected, &t1, "ai/gw", globalT1},
		"accepted setting without t1lr":           {lib.StatusAccepted, nil, "ai/gw", globalT1},
		"accepted setting with an empty t1lr":     {lib.StatusAccepted, &empty, "ai/gw", ""},
		"other gateway keeps the AKO-wide path":   {lib.StatusAccepted, &t1, "ai/other", globalT1},
		"unknown gateway keeps the AKO-wide path": {lib.StatusAccepted, &t1, "", globalT1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			withInfraSettings(t, "ai/gw", c.status, c.t1)
			if got := GatewayTier1LR("key", c.gw); got != c.want {
				t.Fatalf("GatewayTier1LR(%q) = %q, want %q", c.gw, got, c.want)
			}
		})
	}
}

// No NSX Tier-1 anywhere: nothing is set, as before.
func TestGatewayTier1LRUnset(t *testing.T) {
	t.Setenv("NSXT_T1_LR", "")
	if got := GatewayTier1LR("key", "ai/unbound"); got != "" {
		t.Fatalf("GatewayTier1LR = %q, want \"\" on a cloud without a Tier-1", got)
	}
}

// VSTier1LR follows the child VS's Gateway (markers, else ServiceMetadata), and
// falls back to the AKO-wide path for a VS that names none.
func TestVSTier1LR(t *testing.T) {
	t1 := infraT1
	withInfraSettings(t, "ai/gw", lib.StatusAccepted, &t1)

	byMarkers := childVS()
	byMarkers.AviMarkers.GatewayNamespace, byMarkers.AviMarkers.GatewayName = "ai", "gw"
	byMetadata := childVS()
	byMetadata.ServiceMetadata.Gateway = "ai/gw"
	cases := map[string]struct {
		vs   nodes.AviVsEvhSniModel
		want string
	}{
		"gateway markers":         {byMarkers, infraT1},
		"service metadata":        {byMetadata, infraT1},
		"no gateway on the node":  {childVS(), globalT1},
		"not an EVH child (nil)":  {(*nodes.AviEvhVsNode)(nil), globalT1},
		"not an EVH child (type)": {&nodes.AviVsNode{Name: "sni"}, globalT1},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := VSTier1LR("key", c.vs); got != c.want {
				t.Fatalf("VSTier1LR = %q, want %q", got, c.want)
			}
		})
	}
}

// The issuer and classifier pools carry the Tier-1 they are given (the serving
// VS's), not the AKO-wide one, and none when it is empty.
func TestRESTPoolBodiesTier1(t *testing.T) {
	t.Setenv("NSXT_T1_LR", globalT1) // must not leak into the bodies
	ips := []string{"10.0.0.1"}

	for name, tier1 := range map[string]*string{
		"issuer": issuerPoolBody("p", "admin", ips, 443, infraT1).Tier1Lr,
		"icap":   icapPoolBody("p", "/api/tenant/?name=admin", "/api/cloud/?name=c", ips, 1344, infraT1).Tier1Lr,
	} {
		if tier1 == nil || *tier1 != infraT1 {
			t.Errorf("%s pool tier1_lr = %v, want %s", name, tier1, infraT1)
		}
	}
	for name, tier1 := range map[string]*string{
		"issuer": issuerPoolBody("p", "admin", ips, 443, "").Tier1Lr,
		"icap":   icapPoolBody("p", "/api/tenant/?name=admin", "/api/cloud/?name=c", ips, 1344, "").Tier1Lr,
	} {
		if tier1 != nil {
			t.Errorf("%s pool tier1_lr = %q, want unset when the VS has no Tier-1", name, *tier1)
		}
	}
}

// A per-policy pool written for VSes on two different Tier-1s is flagged; the
// same Tier-1 again, or a pool re-created after delete, is not.
func TestNoteSharedPoolTier1(t *testing.T) {
	const pool = "ai-test-shared-pool"
	t.Cleanup(func() { forgetSharedPoolTier1(pool) })

	if noteSharedPoolTier1("key", "admin", pool, globalT1) {
		t.Error("first write must not warn")
	}
	if noteSharedPoolTier1("key", "admin", pool, globalT1) {
		t.Error("same Tier-1 again must not warn")
	}
	if !noteSharedPoolTier1("key", "admin", pool, infraT1) {
		t.Error("a different Tier-1 for the same per-policy pool must warn")
	}
	forgetSharedPoolTier1(pool)
	if noteSharedPoolTier1("key", "admin", pool, globalT1) {
		t.Error("a pool re-created after delete must not warn")
	}
}
