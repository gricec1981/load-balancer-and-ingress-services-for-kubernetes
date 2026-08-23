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
	"fmt"

	"github.com/vmware/alb-sdk/go/clients"

	avicache "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/cache"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// Remote-site tier authoring.
// ───────────────────────────
// A `remote` tier forwards to a peer AI Gateway at another site. AKO authors an
// Avi Pool whose single server is the peer's FQDN with resolve_server_by_dns, so
// the Service Engine resolves the name itself and re-resolves it when it changes.
// Nothing in the cluster tracks the peer's VIP: no EndpointSlice to maintain, no
// credentials for the peer's kube API, no controller loop.
//
// Why this is not GSLB by another route: the name is resolved by the SE, at
// connection time, to find one specific known peer's current address. The
// decision of WHICH site serves the request was already made by tier selection
// in the model-route DataScript, after the request body was read. DNS is doing a
// name lookup, not load balancing — so a remote tier can be chosen on the
// `model` field, which a DNS-time mechanism structurally cannot do.
//
// The pool carries a health monitor (GET <healthPath>) so a dead peer is marked
// down in seconds rather than waiting out a DNS TTL.

func remotePoolName(policyNs, policyName, tier string) string {
	return fmt.Sprintf("%s-%s-%s-remote-pool", policyNs, policyName, tier)
}
func remotePoolGroupName(policyNs, policyName, tier string) string {
	return fmt.Sprintf("%s-%s-%s-remote-pg", policyNs, policyName, tier)
}
func remoteHealthMonitorName(policyNs, policyName, tier string) string {
	return fmt.Sprintf("%s-%s-%s-remote-hm", policyNs, policyName, tier)
}

// RemoteRuntime is the per-tier data the DataScript generator bakes into Lua for
// a remote tier. It is deliberately thinner than ProviderRuntime: a peer gateway
// speaks the same dialect we do, so there is no path rewrite, no credential
// injection, and no metering opt-out.
type RemoteRuntime struct {
	Tier   string
	PGName string // Avi Pool Group name the DataScript selects
	Host   string // peer FQDN, used as the rewritten Host header ("" = preserve)
}

// fqdnPoolSpec describes an Avi pool with one DNS-resolved server.
type fqdnPoolSpec struct {
	Name      string
	TenantRef string
	CloudRef  string
	Host      string
	Port      int32
	TLS       bool
	// HealthMonitorRefs is optional; nil attaches no monitor.
	HealthMonitorRefs []string
}

// ensureFQDNPool creates or updates a pool whose only server is an FQDN the SE
// resolves itself. Shared by provider tiers (a vendor API served from many
// rotating IPs) and remote tiers (a peer gateway whose VIP may be renumbered) —
// in both cases the point is that AKO never learns, stores, or refreshes an
// address.
func ensureFQDNPool(client *clients.AviClient, spec fqdnPoolSpec) error {
	return postOrPut(client, "/api/pool", spec.Name, fqdnPoolBody(spec))
}

// fqdnPoolBody builds the pool payload. Split out from ensureFQDNPool so the
// shape can be asserted without an Avi Controller — in particular that it never
// contains an address.
func fqdnPoolBody(spec fqdnPoolSpec) map[string]interface{} {
	pool := map[string]interface{}{
		"name":                spec.Name,
		"tenant_ref":          spec.TenantRef,
		"cloud_ref":           spec.CloudRef,
		"default_server_port": spec.Port,
		// FQDN server: the SE resolves it by DNS and re-resolves on rotation, so a
		// backend whose address changes stays reachable without pinning.
		//
		// `ip` is required even here -- Avi 31.2.1 rejects the POST outright with
		// "Pool is missing required fields: servers[0].ip". A DNS-typed IpAddr is
		// how a name is carried in that field: `addr` holds the FQDN, not an
		// address, so nothing is pinned and the SE still does the resolving.
		"servers": []map[string]interface{}{{
			"hostname":              spec.Host,
			"ip":                    map[string]interface{}{"type": "DNS", "addr": spec.Host},
			"resolve_server_by_dns": true,
		}},
	}
	if spec.TLS {
		// Backend TLS; SNI is the server hostname (System-Standard is a stock profile).
		pool["ssl_profile_ref"] = "/api/sslprofile/?name=System-Standard"
	}
	if len(spec.HealthMonitorRefs) > 0 {
		pool["health_monitor_refs"] = spec.HealthMonitorRefs
	}
	return pool
}

// ensureRemoteHealthMonitor authors the HTTP(S) monitor that decides whether the
// peer site is up. Failure is not fatal: a tier that reaches its peer but cannot
// prove liveness is still better than a tier that does not exist, so the caller
// logs and continues with an unmonitored pool.
func ensureRemoteHealthMonitor(client *clients.AviClient, name, tenantRef, path string, tls bool) error {
	return postOrPut(client, "/api/healthmonitor", name, remoteHealthMonitorBody(name, tenantRef, path, tls))
}

// remoteHealthMonitorBody builds the monitor payload. An HTTPS peer needs
// https_monitor and an HTTP one http_monitor; Avi rejects the mismatched pair.
// send_interval 5s with 2 failed checks puts peer-down detection around 10s —
// fast enough that failing over to a lower-preference tier is worth doing, which
// is the whole reason a DNS TTL was the wrong mechanism.
func remoteHealthMonitorBody(name, tenantRef, path string, tls bool) map[string]interface{} {
	monitor := map[string]interface{}{
		"http_request":       "GET " + path + " HTTP/1.0",
		"http_response_code": []string{"HTTP_2XX"},
	}
	hm := map[string]interface{}{
		"name":              name,
		"tenant_ref":        tenantRef,
		"send_interval":     5,
		"receive_timeout":   4,
		"failed_checks":     2,
		"successful_checks": 2,
	}
	if tls {
		hm["type"] = "HEALTH_MONITOR_HTTPS"
		hm["https_monitor"] = monitor
	} else {
		hm["type"] = "HEALTH_MONITOR_HTTP"
		hm["http_monitor"] = monitor
	}
	return hm
}

// EnsureRemoteTier authors the FQDN Pool (+ health monitor) and Pool Group for a
// remote-site tier and returns the runtime data the DataScript needs. Tenant is
// resolved from the policy namespace (never GetTenant()).
func EnsureRemoteTier(key string, policy *AIModelRoutePolicy, tier ModelTier) (*RemoteRuntime, error) {
	rem := tier.Remote
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	tenantRef := "/api/tenant/?name=" + lib.GetEscapedValue(tenant)
	cloudRef := "/api/cloud/?name=" + utils.CloudName

	poolName := remotePoolName(policy.Namespace, policy.Name, tier.Name)
	pgName := remotePoolGroupName(policy.Namespace, policy.Name, tier.Name)

	var hmRefs []string
	if path := rem.EffectiveHealthPath(); path != "-" {
		hmName := remoteHealthMonitorName(policy.Namespace, policy.Name, tier.Name)
		if err := ensureRemoteHealthMonitor(client, hmName, tenantRef, path, rem.EffectiveTLS()); err != nil {
			utils.AviLog.Warnf("key: %s, msg: AIModelRoutePolicy %s/%s tier %q: health monitor %s failed, pool will be unmonitored: %v",
				key, policy.Namespace, policy.Name, tier.Name, hmName, err)
		} else {
			hmRefs = append(hmRefs, "/api/healthmonitor/?name="+hmName)
		}
	}

	if err := ensureFQDNPool(client, fqdnPoolSpec{
		Name:              poolName,
		TenantRef:         tenantRef,
		CloudRef:          cloudRef,
		Host:              rem.Host,
		Port:              rem.EffectivePort(),
		TLS:               rem.EffectiveTLS(),
		HealthMonitorRefs: hmRefs,
	}); err != nil {
		return nil, err
	}

	pg := map[string]interface{}{
		"name":       pgName,
		"tenant_ref": tenantRef,
		"cloud_ref":  cloudRef,
		"members":    []map[string]interface{}{{"pool_ref": "/api/pool/?name=" + poolName}},
	}
	if err := postOrPut(client, "/api/poolgroup", pgName, pg); err != nil {
		return nil, err
	}

	rt := &RemoteRuntime{Tier: tier.Name, PGName: pgName}
	if !rem.EffectivePreserveHost() {
		rt.Host = rem.Host
	}
	utils.AviLog.Infof("key: %s, msg: AIModelRoutePolicy %s/%s: remote tier %q pool %s → %s:%d ensured (monitored=%t)",
		key, policy.Namespace, policy.Name, tier.Name, poolName, rem.Host, rem.EffectivePort(), len(hmRefs) > 0)
	return rt, nil
}

// DeleteRemoteTiers removes the AKO-authored remote Pools/PoolGroups/monitors
// when the policy is deleted. Order matters: the pool group references the pool,
// and the pool references the monitor.
func DeleteRemoteTiers(key string, policy *AIModelRoutePolicy) {
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	for _, t := range policy.Spec.Tiers {
		if !t.IsRemote() {
			continue
		}
		deleteAviObjectByName(key, client, "/api/poolgroup", remotePoolGroupName(policy.Namespace, policy.Name, t.Name))
		deleteAviObjectByName(key, client, "/api/pool", remotePoolName(policy.Namespace, policy.Name, t.Name))
		deleteAviObjectByName(key, client, "/api/healthmonitor", remoteHealthMonitorName(policy.Namespace, policy.Name, t.Name))
	}
}

// deleteAviObjectByName removes an Avi object by name if it exists, logging (not
// returning) failures — deletion runs on the policy-delete path where there is no
// caller left to handle an error.
func deleteAviObjectByName(key string, client *clients.AviClient, api, name string) {
	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, api+"?name="+name, &check)
	if check.Count == 0 {
		return
	}
	if err := lib.AviDelete(client, api+"/"+check.Results[0].UUID); err != nil {
		utils.AviLog.Warnf("key: %s, msg: delete %s/%s failed: %v", key, api, name, err)
	}
}
