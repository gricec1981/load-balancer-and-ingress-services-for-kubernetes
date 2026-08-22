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
	"fmt"
	"strconv"

	"github.com/vmware/alb-sdk/go/models"

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
func (o *AviObjectGraph) ApplyModelRoutePolicy(key string, policy *akogatewayapiaigateway.AIModelRoutePolicy, childVsNode *nodes.AviEvhVsNode, parentNsName string, routeModel RouteModel, rule *Rule, mode akogatewayapiaigateway.AuthClaimMode) {
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

	// Merge pod-discovered model aliases into the model→tier table (no-op when
	// spec.discovery is off; the stored policy is never mutated).
	policy = akogatewayapiaigateway.WithDiscoveredModels(key, policy)

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
	providers := make(map[string]*akogatewayapiaigateway.ProviderRuntime)
	remotes := make(map[string]*akogatewayapiaigateway.RemoteRuntime)
	var pgRefNames []string
	for _, tier := range policy.Spec.Tiers {
		// External-provider tier (e.g. Gemini): AKO authors an FQDN pool + pool
		// group over REST (backend TLS/SNI); the DataScript rewrites path/Host and
		// injects the key. No node-graph pool — the DataScript selects the REST PG
		// by name (declared in pool_group_refs).
		if tier.IsProvider() {
			rt, err := akogatewayapiaigateway.EnsureProviderTier(key, policy, tier)
			if err != nil {
				utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: provider setup failed: %v", key, policy.Namespace, policy.Name, tier.Name, err)
				continue
			}
			tierPG[tier.Name] = rt.PGName
			pgRefNames = append(pgRefNames, rt.PGName)
			providers[tier.Name] = rt
			continue
		}
		// Remote-site tier: a peer AI Gateway at another site, addressed by FQDN.
		// Same shape as a provider tier — an FQDN pool + pool group authored over
		// REST, selected by the DataScript — but the SE resolves a peer VIP rather
		// than a vendor's rotating addresses, and nothing is rewritten except Host.
		if tier.IsRemote() {
			rt, err := akogatewayapiaigateway.EnsureRemoteTier(key, policy, tier)
			if err != nil {
				utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: remote setup failed: %v", key, policy.Namespace, policy.Name, tier.Name, err)
				continue
			}
			tierPG[tier.Name] = rt.PGName
			pgRefNames = append(pgRefNames, rt.PGName)
			remotes[tier.Name] = rt
			continue
		}
		if tier.BackendRef.Kind != lib.InferencePool && tier.BackendRef.Kind != utils.Service {
			utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: only InferencePool and Service backends are supported, skipping", key, policy.Namespace, policy.Name, tier.Name)
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
		if tier.BackendRef.Kind == utils.Service {
			// A core Service tier: members come from the Service's endpoints, so a
			// selectorless Service + manual EndpointSlice can front infrastructure
			// outside the cluster (GPU VMs, bare metal) as a first-class tier.
			o.buildModelRouteServicePool(key, policy.Namespace, policy.Name, tier.Name, tier.BackendRef.Name,
				parentNs, parentName, routeModel, childVsNode, listenerProtocol, PG)
		} else {
			hb := &HTTPBackend{Backend: &Backend{
				Name:      tier.BackendRef.Name,
				Namespace: policy.Namespace,
				Kind:      lib.InferencePool,
			}}
			o.buildInferencePoolMembers(key, routeKey, hb, parentNs, parentName, rule, PG, childVsNode, listenerProtocol, parentNsName)
		}
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

	scripts := akogatewayapiaigateway.GenerateModelRouteScripts(policy, tierPG, providers, remotes, mode)
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

// buildModelRouteServicePool builds one Avi pool from a core-Service tier backend
// and adds it to the tier's Pool Group. Members come from the Service's endpoints
// (PopulateServers), so a selectorless Service backed by a manual EndpointSlice
// exposes out-of-cluster serving infrastructure (GPU VMs, bare metal) as a tier.
func (o *AviObjectGraph) buildModelRouteServicePool(key, policyNs, policyName, tierName, svcName string,
	parentNs, parentName string, routeModel RouteModel, childVsNode *nodes.AviEvhVsNode,
	listenerProtocol string, PG *nodes.AviPoolGroupNode) {
	svcObj, err := utils.GetInformers().ServiceInformer.Lister().Services(policyNs).Get(svcName)
	if err != nil {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: service %s/%s not found: %v", key, policyNs, policyName, tierName, policyNs, svcName, err)
		return
	}
	if len(svcObj.Spec.Ports) == 0 {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: service %s/%s has no ports", key, policyNs, policyName, tierName, policyNs, svcName)
		return
	}
	port := svcObj.Spec.Ports[0].Port
	poolName := akogatewayapilib.GetPoolName(parentNs, parentName,
		routeModel.GetNamespace(), routeModel.GetName(),
		"aimr-"+policyName+"-"+tierName,
		policyNs, svcName, strconv.Itoa(int(port)))
	poolNode := &nodes.AviPoolNode{
		Name:       poolName,
		Tenant:     childVsNode.Tenant,
		Protocol:   listenerProtocol,
		PortName:   akogatewayapilib.FindPortName(svcName, policyNs, port, key),
		TargetPort: akogatewayapilib.FindTargetPort(svcName, policyNs, port, key),
		Port:       port,
		ServiceMetadata: lib.ServiceMetadataObj{
			NamespaceServiceName: []string{policyNs + "/" + svcName},
		},
		VrfContext: lib.GetVrf(),
	}
	poolNode.AviMarkers = utils.AviObjectMarkers{
		GatewayName:        parentName,
		GatewayNamespace:   parentNs,
		HTTPRouteName:      routeModel.GetName(),
		HTTPRouteNamespace: routeModel.GetNamespace(),
		BackendNs:          policyNs,
		BackendName:        svcName,
	}
	poolNode.NetworkPlacementSettings = lib.GetNodeNetworkMap()
	serviceType := lib.GetServiceType()
	if serviceType == lib.NodePortLocal {
		if servers := nodes.PopulateServersForNPL(poolNode, policyNs, svcName, false, key); servers != nil {
			poolNode.Servers = servers
		}
	} else if serviceType == lib.NodePort {
		if servers := nodes.PopulateServersForNodePort(poolNode, policyNs, svcName, false, key); servers != nil {
			poolNode.Servers = servers
		}
	} else {
		if servers := nodes.PopulateServers(poolNode, policyNs, svcName, false, key); servers != nil {
			poolNode.Servers = servers
		}
	}
	if len(poolNode.Servers) == 0 {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: service %s/%s has no endpoints", key, policyNs, policyName, tierName, policyNs, svcName)
		return
	}
	if childVsNode.CheckPoolNChecksum(poolNode.Name, poolNode.GetCheckSum()) {
		childVsNode.ReplaceEvhPoolInEVHNode(poolNode, key)
	}
	poolRef := fmt.Sprintf("/api/pool?name=%s", poolNode.Name)
	ratio := uint32(1)
	PG.Members = append(PG.Members, &models.PoolGroupMember{PoolRef: &poolRef, Ratio: &ratio})
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
