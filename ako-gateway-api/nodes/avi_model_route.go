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
	"google.golang.org/protobuf/proto"

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
	// The Tier-1 (VPC) path of this child VS: every pool of every tier, node-graph
	// or REST-authored, must carry the same one (see aigateway/tier1.go).
	tier1LR := akogatewayapiaigateway.GatewayTier1LR(key, parentNsName)

	// Build one Pool Group per tier and collect tier→PG-name for the DataScript.
	tierPG := make(map[string]string, len(policy.Spec.Tiers))
	providers := make(map[string]*akogatewayapiaigateway.ProviderRuntime)
	remotes := make(map[string]*akogatewayapiaigateway.RemoteRuntime)
	var pgRefNames []string
	for _, tier := range policy.Spec.Tiers {
		// Per-tier rules (backendTLS only on Service tiers, backendRef.namespace
		// only on Service tiers): a violation skips this tier, not the policy.
		if err := akogatewayapiaigateway.ValidateTierBackend(policy.Namespace, tier); err != nil {
			utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: %v; skipping tier", key, policy.Namespace, policy.Name, tier.Name, err)
			continue
		}
		// External-provider tier (e.g. Gemini): AKO authors an FQDN pool + pool
		// group over REST (backend TLS/SNI); the DataScript rewrites path/Host and
		// injects the key. No node-graph pool — the DataScript selects the REST PG
		// by name (declared in pool_group_refs).
		if tier.IsProvider() {
			rt, err := akogatewayapiaigateway.EnsureProviderTier(key, policy, tier, tier1LR)
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
			rt, err := akogatewayapiaigateway.EnsureRemoteTier(key, policy, tier, tier1LR)
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
			//
			// The Service may live in another namespace when a ReferenceGrant there
			// allows it; without one the tier is skipped — never retried against a
			// same-named Service in the policy namespace.
			backendNs, err := akogatewayapiaigateway.ServiceTierBackendNamespace(policy, tier)
			if err != nil {
				utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: %v; skipping tier", key, policy.Namespace, policy.Name, tier.Name, err)
				continue
			}
			// backendTLS must resolve completely (grants, Secrets, keys) or the tier
			// is skipped: an mTLS backend is never handed a plaintext pool.
			tlsMaterial, err := akogatewayapiaigateway.ResolveBackendTLS(policy, tier, backendNs)
			if err != nil {
				utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: backendTLS: %v; skipping tier (no plaintext fallback)", key, policy.Namespace, policy.Name, tier.Name, err)
				continue
			}
			o.buildModelRouteServicePool(key, policy.Namespace, policy.Name, tier.Name, backendNs, tier.BackendRef.Name,
				parentNs, parentName, parentNsName, routeModel, childVsNode, listenerProtocol, PG, tlsMaterial)
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
//
// backendNs is the Service's namespace — the policy's own, or another one a
// ReferenceGrant admitted. Every lookup, the pool name and the ServiceMetadata
// use it, so the pool is keyed to the Service it really fronts and NodePortLocal
// resolves the right pods. tlsMaterial (nil = plaintext) makes the pool TLS /
// mTLS to the backend (see applyModelTierBackendTLS).
func (o *AviObjectGraph) buildModelRouteServicePool(key, policyNs, policyName, tierName, backendNs, svcName string,
	parentNs, parentName, parentNsName string, routeModel RouteModel, childVsNode *nodes.AviEvhVsNode,
	listenerProtocol string, PG *nodes.AviPoolGroupNode, tlsMaterial *akogatewayapiaigateway.BackendTLSMaterial) {
	svcObj, err := utils.GetInformers().ServiceInformer.Lister().Services(backendNs).Get(svcName)
	if err != nil {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: service %s/%s not found: %v", key, policyNs, policyName, tierName, backendNs, svcName, err)
		return
	}
	if len(svcObj.Spec.Ports) == 0 {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: service %s/%s has no ports", key, policyNs, policyName, tierName, backendNs, svcName)
		return
	}
	port := svcObj.Spec.Ports[0].Port
	poolName := akogatewayapilib.GetPoolName(parentNs, parentName,
		routeModel.GetNamespace(), routeModel.GetName(),
		"aimr-"+policyName+"-"+tierName,
		backendNs, svcName, strconv.Itoa(int(port)))
	poolNode := &nodes.AviPoolNode{
		Name:       poolName,
		Tenant:     childVsNode.Tenant,
		Protocol:   listenerProtocol,
		PortName:   akogatewayapilib.FindPortName(svcName, backendNs, port, key),
		TargetPort: akogatewayapilib.FindTargetPort(svcName, backendNs, port, key),
		Port:       port,
		ServiceMetadata: lib.ServiceMetadataObj{
			NamespaceServiceName: []string{backendNs + "/" + svcName},
		},
		VrfContext: lib.GetVrf(),
	}
	poolNode.AviMarkers = utils.AviObjectMarkers{
		GatewayName:        parentName,
		GatewayNamespace:   parentNs,
		HTTPRouteName:      routeModel.GetName(),
		HTTPRouteNamespace: routeModel.GetNamespace(),
		BackendNs:          backendNs,
		BackendName:        svcName,
	}
	// NSX-T / VPC clouds: a pool must carry the Tier-1 (or VPC) path, else the Controller
	// rejects it with "Tier 1 cannot be derived from vrf". Same resolution as BuildPGPool
	// and the REST-authored tier pools: the AKO-wide T1LR, overridden by an accepted
	// AviInfraSetting bound to the Gateway.
	if t1LR := akogatewayapiaigateway.GatewayTier1LR(key, parentNsName); t1LR != "" {
		poolNode.T1Lr = t1LR
		poolNode.VrfContext = ""
	}
	poolNode.NetworkPlacementSettings = lib.GetNodeNetworkMap()
	serviceType := lib.GetServiceType()
	if serviceType == lib.NodePortLocal && len(svcObj.Spec.Selector) == 0 {
		// Selectorless Service = off-cluster tier backend via a hand-written EndpointSlice;
		// NodePortLocal has no pods to map for it, so use the endpoints as-is (ClusterIP mode).
		utils.AviLog.Infof("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: service %s/%s has no selector; populating servers from its endpoints instead of NodePortLocal", key, policyNs, policyName, tierName, backendNs, svcName)
		if servers := nodes.PopulateServers(poolNode, backendNs, svcName, false, key); servers != nil {
			poolNode.Servers = servers
		}
	} else if serviceType == lib.NodePortLocal {
		if servers := nodes.PopulateServersForNPL(poolNode, backendNs, svcName, false, key); servers != nil {
			poolNode.Servers = servers
		}
	} else if serviceType == lib.NodePort {
		if servers := nodes.PopulateServersForNodePort(poolNode, backendNs, svcName, false, key); servers != nil {
			poolNode.Servers = servers
		}
	} else {
		if servers := nodes.PopulateServers(poolNode, backendNs, svcName, false, key); servers != nil {
			poolNode.Servers = servers
		}
	}
	if len(poolNode.Servers) == 0 {
		utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: service %s/%s has no endpoints", key, policyNs, policyName, tierName, backendNs, svcName)
		return
	}
	if tlsMaterial != nil {
		applyModelTierBackendTLS(key, childVsNode, poolNode, tlsMaterial)
		utils.AviLog.Infof("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: pool %s speaks TLS to %s/%s (sni=%q, hostCheck=%t, pki=%t, clientCert=%t)",
			key, policyNs, policyName, tierName, poolNode.Name, backendNs, svcName, tlsMaterial.SNI, tlsMaterial.HostCheck,
			poolNode.PkiProfile != nil, poolNode.SslKeyAndCertificateRef != nil)
	}
	if childVsNode.CheckPoolNChecksum(poolNode.Name, poolNode.GetCheckSum()) {
		childVsNode.ReplaceEvhPoolInEVHNode(poolNode, key)
	}
	poolRef := fmt.Sprintf("/api/pool?name=%s", poolNode.Name)
	ratio := uint32(1)
	PG.Members = append(PG.Members, &models.PoolGroupMember{PoolRef: &poolRef, Ratio: &ratio})
}

// applyModelTierBackendTLS turns a tier pool into a TLS (optionally mutual TLS)
// pool, reusing the node-graph objects AKO already manages for backend TLS:
//
//   - ssl_profile_ref = System-Standard (lib.DefaultPoolSSLProfile), sni_enabled,
//     server_name = sni, host_check_enabled (+ domain_name = [sni]) — the pool
//     fields RouteBackendExtension.backendTLS and route re-encrypt set;
//   - PKI profile = the pool's PkiProfile node (named lib.GetPoolPKIProfileName),
//     created/updated/deleted with the pool by the REST layer's PkiProfileCU;
//   - client certificate = TLSKeyCert nodes on the child VS's CACertRefs (the
//     leaf, type VIRTUALSERVICE, linked to its intermediates, type CA), named
//     after the pool (modelTierClientCertName) and created, checksum-updated and
//     deleted with the child VS by KeyCertCU/SSLKeyCertDelete.
//     They are deliberately not SSLKeyCertRefs: those become the VS's own
//     server certificates.
func applyModelTierBackendTLS(key string, childVsNode *nodes.AviEvhVsNode, poolNode *nodes.AviPoolNode,
	m *akogatewayapiaigateway.BackendTLSMaterial) {
	poolNode.SniEnabled = true
	poolNode.SslProfileRef = proto.String(fmt.Sprintf("/api/sslprofile?name=%s", lib.DefaultPoolSSLProfile))
	if m.SNI != "" {
		poolNode.ServerName = proto.String(m.SNI)
	}
	poolNode.HostCheckEnabled = proto.Bool(m.HostCheck)
	if m.HostCheck && m.SNI != "" {
		poolNode.DomainName = []string{m.SNI}
	}
	if m.CABundle != "" {
		poolNode.PkiProfile = &nodes.AviPkiProfileNode{
			Name:       lib.GetPoolPKIProfileName(poolNode.Name),
			Tenant:     poolNode.Tenant,
			CACert:     m.CABundle,
			AviMarkers: poolNode.AviMarkers,
		}
	}
	if m.ClientCert != nil {
		leafName := attachModelTierClientCert(key, childVsNode, poolNode.Name, m.ClientCert, poolNode.AviMarkers)
		poolNode.SslKeyAndCertificateRef = proto.String("/api/sslkeyandcertificate?name=" + leafName)
	}
}

// modelTierClientCertName names the Avi SSLKeyAndCertificate AKO authors for a
// tier client certificate. It is deterministic in (pool, Secret) and follows the
// pool exactly, as GetPoolPKIProfileName does: the tier pool is named per route
// (not per rule), so every rule's child VS of a multi-rule HTTPRoute builds the
// same pool — and must point it at the same certificate. Keying the name on the
// child VS instead made each child re-point the shared pool at its own copy and,
// after a restart, delete the copy another child's pool still referenced. Each
// child VS carries an identical node, which replaceModelTierCertNode dedups.
// The readable form (when enabled) shows only the Secret. index 0 is the leaf;
// index i > 0 is the i-th certificate after the leaf in tls.crt (an intermediate CA).
func modelTierClientCertName(poolName, secretNs, secretName string, index int) string {
	s := poolName + "/aimr-clientcert/" + secretNs + "/" + secretName
	hint := "aimr-clientcert-" + secretNs + "-" + secretName
	if index > 0 {
		s = fmt.Sprintf("%s/ca%d", s, index)
		hint = fmt.Sprintf("%s-ca%d", hint, index)
	}
	return lib.EncodeWithHint(s, hint, lib.SSLKeyCert)
}

// attachModelTierClientCert adds the client certificate (and its intermediates)
// to the child VS as TLSKeyCert nodes and returns the leaf's name. The leaf links
// its issuer (ca_certs), each intermediate links the next, so the SE sends the
// chain. Issuers are placed first: KeyCertCU emits creates in slice order and a
// certificate's CA must exist before it is referenced. poolName is the pool that
// presents the certificate; the names derive from it (see modelTierClientCertName).
func attachModelTierClientCert(key string, childVsNode *nodes.AviEvhVsNode, poolName string,
	cc *akogatewayapiaigateway.ClientCertMaterial, markers utils.AviObjectMarkers) string {
	leafName := modelTierClientCertName(poolName, cc.SecretNamespace, cc.SecretName, 0)
	chainNames := make([]string, len(cc.ChainPEM))
	for i := range cc.ChainPEM {
		chainNames[i] = modelTierClientCertName(poolName, cc.SecretNamespace, cc.SecretName, i+1)
	}
	// Top of the chain first.
	for i := len(cc.ChainPEM) - 1; i >= 0; i-- {
		caNode := &nodes.AviTLSKeyCertNode{
			Name:       chainNames[i],
			Tenant:     childVsNode.Tenant,
			Type:       lib.CertTypeCA,
			Cert:       []byte(cc.ChainPEM[i]),
			AviMarkers: markers,
		}
		if i+1 < len(chainNames) {
			caNode.CACert = chainNames[i+1]
		}
		replaceModelTierCertNode(childVsNode, caNode)
	}
	leaf := &nodes.AviTLSKeyCertNode{
		Name:       leafName,
		Tenant:     childVsNode.Tenant,
		Type:       lib.CertTypeVS,
		Cert:       []byte(cc.LeafPEM),
		Key:        []byte(cc.KeyPEM),
		AviMarkers: markers,
	}
	if len(chainNames) > 0 {
		leaf.CACert = chainNames[0]
	}
	replaceModelTierCertNode(childVsNode, leaf)
	utils.AviLog.Debugf("key: %s, msg: child VS %s: client certificate %s from Secret %s/%s (%d intermediates)",
		key, childVsNode.Name, leafName, cc.SecretNamespace, cc.SecretName, len(chainNames))
	return leafName
}

// replaceModelTierCertNode adds cert to the child VS's CACertRefs, replacing a
// same-named node in place (a rebuild of the same pool's certificate).
func replaceModelTierCertNode(childVsNode *nodes.AviEvhVsNode, cert *nodes.AviTLSKeyCertNode) {
	for i, existing := range childVsNode.CACertRefs {
		if existing.Name == cert.Name {
			childVsNode.CACertRefs[i] = cert
			return
		}
	}
	childVsNode.CACertRefs = append(childVsNode.CACertRefs, cert)
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
