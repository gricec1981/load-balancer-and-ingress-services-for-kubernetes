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

package nodes

import (
	akogatewayapiaigateway "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/aigateway"
	akogatewayapilib "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/lib"
	akogatewayapiobjects "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/objects"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// ApplyModelRoutePolicy translates an AIModelRoutePolicy into Avi objects on the
// given child EVH VS: one Pool Group per tier (built from the tier's InferencePool
// backend, reusing the inference scraper-weighted pod members), plus the two
// model-route DataScripts (HTTP_REQ buffer-enable + HTTP_REQ_DATA model parse →
// avi.poolgroup.select). The HTTP_REQ_DATA script carries every tier Pool Group in
// its pool_group_refs so avi.poolgroup.select() validates.
//
// It must be invoked *before* ApplyTokenRateLimitPolicy so the model-route
// DataScripts get lower DataScript indices and run first — the per-tier token
// budget enforcement reads the ai_tier reqvar this script sets.
func (o *AviObjectGraph) ApplyModelRoutePolicy(key string, policy *akogatewayapiaigateway.AIModelRoutePolicy, childVsNode *nodes.AviEvhVsNode, parentNsName string, routeModel RouteModel, rule *Rule) {
	if policy == nil {
		return
	}
	if err := policy.Spec.Validate(); err != nil {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s invalid, skipping: %v", key, policy.Namespace, policy.Name, err)
		return
	}
	if !lib.IsInferenceExtensionEnabled() {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s requires the inference extension (tier backends are InferencePools); skipping", key, policy.Namespace, policy.Name)
		return
	}

	routeKey := lib.HTTPRoute + "/" + routeModel.GetNamespace() + "/" + routeModel.GetName()
	parentNs, _, parentName := lib.ExtractTypeNameNamespace(parentNsName)
	listeners := akogatewayapiobjects.GatewayApiLister().GetRouteToGatewayListener(routeKey, parentNsName)
	if len(listeners) == 0 {
		utils.AviLog.Warnf("key: %s, msg: no matching listener for route %s; skipping model routing", key, routeKey)
		return
	}
	listenerProtocol := listeners[0].Protocol

	// Build one Pool Group per tier and collect tier→PG-name for the DataScript.
	tierPG := make(map[string]string, len(policy.Spec.Tiers))
	var pgRefNames []string
	for _, tier := range policy.Spec.Tiers {
		if tier.BackendRef.Kind != lib.InferencePool {
			utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: only InferencePool backends are supported, skipping", key, policy.Namespace, policy.Name, tier.Name)
			continue
		}
		pgName := akogatewayapilib.GetPoolGroupName(parentNs, parentName,
			routeModel.GetNamespace(), routeModel.GetName(),
			"aimr-"+policy.Name+"-"+tier.Name)
		PG := &nodes.AviPoolGroupNode{Name: pgName, Tenant: childVsNode.Tenant}
		PG.AviMarkers = utils.AviObjectMarkers{
			GatewayName:        parentName,
			GatewayNamespace:   parentNs,
			HTTPRouteName:      routeModel.GetName(),
			HTTPRouteNamespace: routeModel.GetNamespace(),
		}
		hb := &HTTPBackend{Backend: &Backend{
			Name:      tier.BackendRef.Name,
			Namespace: policy.Namespace,
			Kind:      lib.InferencePool,
		}}
		o.buildInferencePoolMembers(key, routeKey, hb, parentNs, parentName, rule, PG, childVsNode, listenerProtocol, parentNsName)
		if len(PG.Members) == 0 {
			utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: pool group %s has no members yet (pool not reconciled?), skipping tier", key, policy.Namespace, policy.Name, tier.Name, pgName)
			continue
		}
		attachModelRoutePoolGroup(childVsNode, PG)
		tierPG[tier.Name] = pgName
		pgRefNames = append(pgRefNames, pgName)
	}
	if len(tierPG) == 0 {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s produced no tier pool groups; not attaching DataScripts", key, policy.Namespace, policy.Name)
		return
	}

	scripts := akogatewayapiaigateway.GenerateModelRouteScripts(policy, tierPG)
	vsName := childVsNode.Name
	// HTTP_REQ: enable request-body buffering (no pool refs needed).
	attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSModelRouteReqName(vsName),
		akogatewayapiaigateway.DSEvtHTTPReq, scripts.ReqScript, nil)
	// HTTP_REQ_DATA: parse model + select tier PG; declare every tier PG so
	// avi.poolgroup.select() validates.
	attachModelRouteDS(childVsNode, akogatewayapiaigateway.DSModelRouteReqDataName(vsName),
		akogatewayapiaigateway.DSEvtHTTPReqData, scripts.ReqDataScript, pgRefNames)

	utils.AviLog.Infof("key: %s, msg: AIModelRoutePolicy %s/%s: attached model-based tier routing on VS %s (%d tiers)",
		key, policy.Namespace, policy.Name, vsName, len(tierPG))
}

// attachModelRoutePoolGroup adds (or replaces by name) a tier Pool Group on the
// child VS. BuildPGPool already cleared PoolGroupRefs for this reconcile, so this
// adds the model-route tier PGs alongside the rule's own PG.
func attachModelRoutePoolGroup(childVsNode *nodes.AviEvhVsNode, PG *nodes.AviPoolGroupNode) {
	for i, existing := range childVsNode.PoolGroupRefs {
		if existing.Name == PG.Name {
			childVsNode.PoolGroupRefs[i] = PG
			return
		}
	}
	childVsNode.PoolGroupRefs = append(childVsNode.PoolGroupRefs, PG)
}

// attachModelRouteDS adds (or replaces by name) a model-route DataScript on the
// child VS, preserving slice position on replace so the model-route scripts keep a
// lower index than the later-appended token scripts.
func attachModelRouteDS(childVsNode *nodes.AviEvhVsNode, name, evt, script string, pgRefs []string) {
	ds := &nodes.AviHTTPDataScriptNode{
		Name:          name,
		Tenant:        childVsNode.Tenant,
		PoolGroupRefs: pgRefs,
		DataScript:    &nodes.DataScript{Evt: evt, Script: script},
	}
	for i, e := range childVsNode.HTTPDSrefs {
		if e.Name == name {
			childVsNode.HTTPDSrefs[i] = ds
			return
		}
	}
	childVsNode.HTTPDSrefs = append(childVsNode.HTTPDSrefs, ds)
}
