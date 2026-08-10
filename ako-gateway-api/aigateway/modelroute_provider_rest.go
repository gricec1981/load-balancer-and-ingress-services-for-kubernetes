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
	"strings"

	avicache "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/cache"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// External-provider tier authoring.
// ─────────────────────────────────
// A `provider` tier (e.g. Gemini) routes to an external OpenAI-compatible API
// over the Service Engine's egress. AKO authors an Avi Pool whose server is the
// provider FQDN (SE resolves by DNS) with backend TLS/SNI, wrapped in a Pool
// Group the model-route DataScript selects. The DataScript also rewrites the
// request path + Host and injects the API key (see modelroute_datascript.go);
// this file only builds the Avi Pool/PoolGroup.
//
// SPIKE-VERIFIED on Avi 31.2.1 (2026-08-09): the SE resolves
// generativelanguage.googleapis.com, connects over backend TLS, and Gemini
// returns 200. Path rewrite uses avi.http.set_path (DataScripts have no
// replace_uri); Host uses avi.http.replace_header.

// ProviderRuntime is the per-tier data the DataScript generator bakes into Lua.
type ProviderRuntime struct {
	Tier       string
	PGName     string // Avi Pool Group name the DataScript selects
	Path       string // provider chat-completions path (set_path)
	Host       string // provider FQDN (replace_header Host)
	AuthHeader string // e.g. Authorization ("" = no auth header)
	AuthValue  string // e.g. "Bearer <key>"
}

func providerPoolName(policyNs, policyName, tier string) string {
	return fmt.Sprintf("%s-%s-%s-provider-pool", policyNs, policyName, tier)
}
func providerPoolGroupName(policyNs, policyName, tier string) string {
	return fmt.Sprintf("%s-%s-%s-provider-pg", policyNs, policyName, tier)
}

// resolveProviderAuthValue reads the API key from the referenced Secret and
// builds the header value ("<scheme> <key>", or the raw key when scheme is "").
func resolveProviderAuthValue(policyNs string, auth *ProviderAuth) (string, error) {
	if auth == nil {
		return "", nil
	}
	ns := policyNs
	sec, err := utils.GetInformers().SecretInformer.Lister().Secrets(ns).Get(auth.SecretRef.Name)
	if err != nil {
		return "", fmt.Errorf("provider secret %s/%s: %w", ns, auth.SecretRef.Name, err)
	}
	key := auth.SecretRef.Key
	var raw []byte
	if key != "" {
		raw = sec.Data[key]
		if raw == nil {
			return "", fmt.Errorf("provider secret %s/%s has no key %q", ns, auth.SecretRef.Name, key)
		}
	} else {
		if len(sec.Data) != 1 {
			return "", fmt.Errorf("provider secret %s/%s: specify secretRef.key (has %d keys)", ns, auth.SecretRef.Name, len(sec.Data))
		}
		for _, v := range sec.Data {
			raw = v
		}
	}
	val := strings.TrimSpace(string(raw))
	if s := auth.EffectiveScheme(); s != "" {
		val = s + " " + val
	}
	return val, nil
}

// EnsureProviderTier authors the FQDN Pool + Pool Group for a provider tier and
// returns the runtime data the DataScript needs. Tenant is resolved from the
// policy namespace (never GetTenant()).
func EnsureProviderTier(key string, policy *AIModelRoutePolicy, tier ModelTier) (*ProviderRuntime, error) {
	prov := tier.Provider
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	tenantRef := "/api/tenant/?name=" + lib.GetEscapedValue(tenant)
	cloudRef := "/api/cloud/?name=" + utils.CloudName

	poolName := providerPoolName(policy.Namespace, policy.Name, tier.Name)
	pgName := providerPoolGroupName(policy.Namespace, policy.Name, tier.Name)

	pool := map[string]interface{}{
		"name":                poolName,
		"tenant_ref":          tenantRef,
		"cloud_ref":           cloudRef,
		"default_server_port": prov.EffectivePort(),
		// FQDN server: the SE resolves it by DNS and re-resolves on rotation, so a
		// public API served from many IPs (Google) stays reachable without pinning.
		"servers": []map[string]interface{}{{
			"hostname":             prov.Host,
			"resolve_server_by_dns": true,
		}},
	}
	if prov.EffectiveTLS() {
		// Backend TLS + SNI to the provider host (System-Standard is a stock profile).
		pool["ssl_profile_ref"] = "/api/sslprofile/?name=System-Standard"
	}
	if err := postOrPut(client, "/api/pool", poolName, pool); err != nil {
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

	authVal, err := resolveProviderAuthValue(policy.Namespace, prov.Auth)
	if err != nil {
		return nil, err
	}
	rt := &ProviderRuntime{
		Tier: tier.Name, PGName: pgName, Path: prov.Path, Host: prov.Host,
	}
	if prov.Auth != nil && authVal != "" {
		rt.AuthHeader = prov.Auth.EffectiveHeader()
		rt.AuthValue = authVal
	}
	utils.AviLog.Infof("key: %s, msg: AIModelRoutePolicy %s/%s: provider tier %q pool %s → %s%s ensured",
		key, policy.Namespace, policy.Name, tier.Name, poolName, prov.Host, prov.Path)
	return rt, nil
}

// DeleteProviderTiers removes the AKO-authored provider Pools/PoolGroups when the
// policy is deleted.
func DeleteProviderTiers(key string, policy *AIModelRoutePolicy) {
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	delByName := func(api, name string) {
		var check struct {
			Count   int `json:"count"`
			Results []struct {
				UUID string `json:"uuid"`
			} `json:"results"`
		}
		_ = lib.AviGet(client, api+"?name="+name, &check)
		if check.Count > 0 {
			if err := lib.AviDelete(client, api+"/"+check.Results[0].UUID); err != nil {
				utils.AviLog.Warnf("key: %s, msg: delete %s/%s failed: %v", key, api, name, err)
			}
		}
	}
	for _, t := range policy.Spec.Tiers {
		if !t.IsProvider() {
			continue
		}
		delByName("/api/poolgroup", providerPoolGroupName(policy.Namespace, policy.Name, t.Name))
		delByName("/api/pool", providerPoolName(policy.Namespace, policy.Name, t.Name))
	}
}
