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
	"sync"

	akogatewayapilib "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/lib"
	akogatewayapiobjects "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/objects"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// Tier-1 placement of AKO-authored AI pools
// ─────────────────────────────────────────
// On NSX-T / VPC clouds every pool must carry the Tier-1 (or VPC) path, else the
// Controller rejects it ("Tier 1 cannot be derived from vrf"), and it must be the
// SAME path as the VS the pool serves. The node-graph pools (BuildPGPool, the
// model-route Service/InferencePool tiers) resolve it as the AKO-wide
// NSXT_T1_LR, overridden by an ACCEPTED AviInfraSetting bound to the parent
// Gateway. The pools AKO authors over REST (OAuth issuer pool, ICAP classifier
// pool, provider and remote-site FQDN pools) resolve it here the same way, from
// the child VS they are attached to.

// GatewayTier1LR returns the Tier-1 (VPC) path for pools serving the Gateway
// gwNsName ("namespace/name"): lib.GetT1LRPath(), overridden by the T1LR of an
// ACCEPTED AviInfraSetting bound to that Gateway. "" means no Tier-1 is
// configured (non-NSX clouds), and the pool is left without one.
func GatewayTier1LR(key, gwNsName string) string {
	t1LR := lib.GetT1LRPath()
	if gwNsName == "" {
		return t1LR
	}
	found, infraSettingName := akogatewayapiobjects.GatewayApiLister().GetGatewayToAviInfraSetting(gwNsName)
	if !found {
		return t1LR
	}
	informer := akogatewayapilib.AKOControlConfig().AviInfraSettingInformer()
	if informer == nil {
		return t1LR
	}
	infraSetting, err := informer.Lister().Get(infraSettingName)
	if err != nil {
		utils.AviLog.Warnf("key: %s, msg: failed to retrieve AviInfraSetting %s, err: %s", key, infraSettingName, err.Error())
		return t1LR
	}
	if infraSetting != nil && infraSetting.Status.Status == lib.StatusAccepted && infraSetting.Spec.NSXSettings.T1LR != nil {
		t1LR = *infraSetting.Spec.NSXSettings.T1LR
	}
	return t1LR
}

// VSTier1LR returns the Tier-1 path of the Gateway API child VS vsNode, i.e. the
// one its node-graph pools carry (see GatewayTier1LR). A VS that does not name
// its Gateway falls back to the AKO-wide lib.GetT1LRPath().
func VSTier1LR(key string, vsNode nodes.AviVsEvhSniModel) string {
	return GatewayTier1LR(key, vsGatewayNsName(vsNode))
}

// vsGatewayNsName returns the "namespace/name" of the Gateway a child VS was
// built for, or "" when the node does not carry one.
func vsGatewayNsName(vsNode nodes.AviVsEvhSniModel) string {
	evh, ok := vsNode.(*nodes.AviEvhVsNode)
	if !ok || evh == nil {
		return ""
	}
	if evh.AviMarkers.GatewayNamespace != "" && evh.AviMarkers.GatewayName != "" {
		return evh.AviMarkers.GatewayNamespace + "/" + evh.AviMarkers.GatewayName
	}
	return evh.ServiceMetadata.Gateway
}

// Per-policy pools (the issuer pool of an auth policy, the classifier pool of a
// guardrail policy, a model-route policy's provider/remote tier pools) are named
// after the policy, not the VS, so every VS the policy serves shares one pool.
// If those VSes sit on different Tier-1s (an HTTPRoute attached to Gateways
// with different AviInfraSettings, or an auth policy shared through authRef),
// the pool can carry only one of them: the reconciles fight over it. That is
// not redesigned here; it is logged so the operator can align the Gateways.
var (
	sharedPoolTier1Mu sync.Mutex
	sharedPoolTier1   = map[string]string{}
)

// noteSharedPoolTier1 records the Tier-1 path a per-policy pool is being
// written with and warns when it differs from the previous write for the same
// pool, i.e. the pool serves VSes on different Tier-1s. It reports whether it
// warned. Pool names are namespace- and policy-qualified, so the name is the key.
func noteSharedPoolTier1(key, tenant, poolName, t1LR string) bool {
	sharedPoolTier1Mu.Lock()
	prev, seen := sharedPoolTier1[poolName]
	sharedPoolTier1[poolName] = t1LR
	sharedPoolTier1Mu.Unlock()
	if seen && prev != t1LR {
		utils.AviLog.Warnf("key: %s, msg: pool %s (tenant %s) is per-policy and shared by VSes on different Tier-1s: was %q, now %q; "+
			"the last reconcile wins. Bind the routes' Gateways to the same Tier-1 (AviInfraSetting nsxSettings.t1lr)",
			key, poolName, tenant, prev, t1LR)
		return true
	}
	return false
}

// forgetSharedPoolTier1 drops the record of a per-policy pool that was deleted,
// so a policy re-created on another Tier-1 does not warn.
func forgetSharedPoolTier1(poolName string) {
	sharedPoolTier1Mu.Lock()
	delete(sharedPoolTier1, poolName)
	sharedPoolTier1Mu.Unlock()
}
