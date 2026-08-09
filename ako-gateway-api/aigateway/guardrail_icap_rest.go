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
	"net/url"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/vmware/alb-sdk/go/clients"
	avimodels "github.com/vmware/alb-sdk/go/models"
	avicache "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/cache"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// Semantic guardrail ICAP authoring.
// ──────────────────────────────────
// The signature layer (guardrail_rest.go) authors a WafPolicy for known-phrase /
// secret / PII detection. The SEMANTIC layer adds a prompt-injection classifier
// the SE calls over ICAP REQMOD, catching novel/paraphrased injection the WAF
// provably misses (see docs/gateway-api/ai-gateway-guardrails-semantic.md, §8
// Spike-4). Feasibility was proven with hand-made objects on Avi 32.1.1 and
// re-verified live on 31.2.1 (2026-08-09).
//
// The spike's key finding: attaching an icapprofile is NOT enough to make ICAP
// fire — the VS must ALSO carry an HTTPPolicySet whose http_security_policy rule
// has action HTTP_SECURITY_ACTION_REQUEST_CHECK_ICAP. So AKO authors BOTH the
// icapprofile and that HTTPPolicySet, then the translator sets both on the VS.
//
// 31.2.1 delta vs the 32.1.1 spike: the icapprofile POST requires an explicit
// cloud_ref matching the pool group's cloud, else the controller rejects it with
// "Illegal cross-cloud references". All three objects therefore carry cloud_ref.

// ─── Name derivation ─────────────────────────────────────────────────────────

func icapPoolName(policy *AIGuardrailPolicy) string {
	return fmt.Sprintf("%s-%s-icap-pool", policy.Namespace, policy.Name)
}
func icapPoolGroupName(policy *AIGuardrailPolicy) string {
	return fmt.Sprintf("%s-%s-icap-pg", policy.Namespace, policy.Name)
}
func icapProfileName(policy *AIGuardrailPolicy) string {
	return fmt.Sprintf("%s-%s-icap", policy.Namespace, policy.Name)
}
func icapSecurityPolicySetName(policy *AIGuardrailPolicy) string {
	return fmt.Sprintf("%s-%s-icap-security", policy.Namespace, policy.Name)
}

// ─── Endpoint resolution ─────────────────────────────────────────────────────

// resolveClassifierServers turns the classifier Service reference into
// Service-Engine-reachable pod IPs + port. Avi SEs cannot reach ClusterIPs, so
// the Service is resolved to its ready EndpointSlice addresses (the same pattern
// oauth_rest.go uses for the issuer). The Service namespace defaults to the
// policy namespace.
func resolveClassifierServers(policyNs string, ref GuardrailBackendRef) (ips []string, port int32, err error) {
	svc := ref.Name
	ns := ref.Namespace
	if ns == "" {
		ns = policyNs
	}
	port = ref.Port
	if port == 0 {
		port = 1344
	}

	sel := labels.Set{"kubernetes.io/service-name": svc}.AsSelector()
	slices, lerr := utils.GetInformers().EpSlicesInformer.Lister().EndpointSlices(ns).List(sel)
	if lerr != nil {
		return nil, 0, fmt.Errorf("list endpointslices for %s/%s: %w", ns, svc, lerr)
	}
	seen := map[string]bool{}
	for _, sl := range slices {
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				if !seen[addr] {
					seen[addr] = true
					ips = append(ips, addr)
				}
			}
		}
	}
	if len(ips) == 0 {
		return nil, 0, fmt.Errorf("no ready endpoints for classifier service %s/%s", ns, svc)
	}
	return ips, port, nil
}

// ─── ICAP Pool + PoolGroup ───────────────────────────────────────────────────

// ensureICAPPoolGroup creates/updates the Avi Pool (classifier endpoints) and a
// PoolGroup wrapping it (icapprofile requires a pool_group_ref, not a pool_ref).
func ensureICAPPoolGroup(key string, policy *AIGuardrailPolicy, tenant string) (string, error) {
	sem := policy.Spec.Semantic
	ips, port, err := resolveClassifierServers(policy.Namespace, sem.Classifier.BackendRef)
	if err != nil {
		return "", err
	}
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	cloudRef := "/api/cloud/?name=" + utils.CloudName
	tenantRef := "/api/tenant/?name=" + lib.GetEscapedValue(tenant)

	poolName := icapPoolName(policy)
	servers := make([]*avimodels.Server, 0, len(ips))
	for i := range ips {
		servers = append(servers, &avimodels.Server{
			IP:   &avimodels.IPAddr{Addr: proto.String(ips[i]), Type: proto.String("V4")},
			Port: proto.Int32(port),
		})
	}
	pool := avimodels.Pool{
		Name:              proto.String(poolName),
		TenantRef:         proto.String(tenantRef),
		CloudRef:          proto.String(cloudRef),
		DefaultServerPort: proto.Int32(port),
		Servers:           servers,
	}
	if err := postOrPut(client, "/api/pool", poolName, pool); err != nil {
		return "", err
	}
	utils.AviLog.Infof("key: %s, msg: ICAP classifier Pool %s ensured (%d servers, port %d)", key, poolName, len(ips), port)

	pgName := icapPoolGroupName(policy)
	pg := avimodels.PoolGroup{
		Name:      proto.String(pgName),
		TenantRef: proto.String(tenantRef),
		CloudRef:  proto.String(cloudRef),
		Members: []*avimodels.PoolGroupMember{{
			PoolRef: proto.String("/api/pool/?name=" + poolName),
		}},
	}
	if err := postOrPut(client, "/api/poolgroup", pgName, pg); err != nil {
		return "", err
	}
	utils.AviLog.Infof("key: %s, msg: ICAP classifier PoolGroup %s ensured", key, pgName)
	return pgName, nil
}

// ─── icapprofile ─────────────────────────────────────────────────────────────

// ensureICAPProfile creates/updates the icapprofile pointing at the classifier
// pool group. Per-policy knobs (threshold, block/log mode) ride the service_uri
// query string, which the shim parses — so one shim serves many policies.
func ensureICAPProfile(key string, policy *AIGuardrailPolicy, tenant, pgName string) (string, error) {
	sem := policy.Spec.Semantic
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	name := icapProfileName(policy)

	failAction := "ICAP_FAIL_OPEN"
	if !sem.SemanticFailOpen() {
		failAction = "ICAP_FAIL_CLOSED"
	}
	// service_uri is path-only (host/port come from the pool). Pass the threshold
	// and block/log mode as query params the shim reads per request.
	serviceURI := fmt.Sprintf("/scan?threshold=%s&mode=%s",
		url.QueryEscape(fmt.Sprintf("%g", sem.SemanticThreshold())), sem.SemanticMode())

	profile := map[string]interface{}{
		"name":             name,
		"tenant_ref":       "/api/tenant/?name=" + lib.GetEscapedValue(tenant),
		"cloud_ref":        "/api/cloud/?name=" + utils.CloudName, // 31.2.1: required or "Illegal cross-cloud references"
		"vendor":           "ICAP_VENDOR_GENERIC",
		"service_uri":      serviceURI,
		"pool_group_ref":   "/api/poolgroup/?name=" + pgName,
		"fail_action":      failAction,
		"allow_204":        true,
		"enable_preview":   false, // we need the full prompt body, not a preview slice
		"response_timeout": 10000,
	}
	if err := postOrPut(client, "/api/icapprofile", name, profile); err != nil {
		return "", err
	}
	utils.AviLog.Infof("key: %s, msg: icapprofile %s ensured (fail_action=%s, uri=%s)", key, name, failAction, serviceURI)
	return name, nil
}

// ─── security-policy HTTPPolicySet (the piece that makes ICAP fire) ───────────

// ensureICAPSecurityPolicySet authors the HTTPPolicySet whose
// http_security_policy rule action is HTTP_SECURITY_ACTION_REQUEST_CHECK_ICAP.
// Without this rule the SE never calls ICAP even with a profile attached
// (Spike-1). An empty path match scans every request on the VS.
func ensureICAPSecurityPolicySet(key string, policy *AIGuardrailPolicy, tenant string) (string, error) {
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	name := icapSecurityPolicySetName(policy)

	hps := map[string]interface{}{
		"name":       name,
		"tenant_ref": "/api/tenant/?name=" + lib.GetEscapedValue(tenant),
		"http_security_policy": map[string]interface{}{
			"rules": []map[string]interface{}{{
				"name":   "semantic-icap",
				"index":  1,
				"enable": true,
				"match": map[string]interface{}{
					"path": map[string]interface{}{
						"match_criteria": "BEGINS_WITH",
						"match_str":      []string{"/"},
					},
				},
				"action": map[string]interface{}{
					"action": "HTTP_SECURITY_ACTION_REQUEST_CHECK_ICAP",
				},
			}},
		},
	}
	if err := postOrPut(client, "/api/httppolicyset", name, hps); err != nil {
		return "", err
	}
	utils.AviLog.Infof("key: %s, msg: ICAP security HTTPPolicySet %s ensured (REQUEST_CHECK_ICAP)", key, name)
	return name, nil
}

// ─── Orchestration ───────────────────────────────────────────────────────────

// GuardrailIcapRefs are the Avi object refs the translator sets on the VS.
type GuardrailIcapRefs struct {
	ICAPProfileRef  string // for icap_request_profile_refs
	SecurityHPSRef  string // for http_policies (external HTTPPolicySet)
}

// EnsureGuardrailIcap authors the full ICAP object graph (Pool → PoolGroup →
// icapprofile, plus the REQUEST_CHECK_ICAP HTTPPolicySet) for a semantic policy
// and returns the two refs the VS needs. Both are mandatory: the profile without
// the security rule is a no-op (Spike-1).
func EnsureGuardrailIcap(key string, policy *AIGuardrailPolicy) (*GuardrailIcapRefs, error) {
	if !policy.Spec.SemanticEnabled() {
		return nil, nil
	}
	if err := policy.Spec.Validate(); err != nil {
		return nil, fmt.Errorf("invalid AIGuardrailPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
	}
	tenant := lib.GetTenantInNamespace(policy.Namespace)

	pgName, err := ensureICAPPoolGroup(key, policy, tenant)
	if err != nil {
		return nil, err
	}
	profName, err := ensureICAPProfile(key, policy, tenant, pgName)
	if err != nil {
		return nil, err
	}
	hpsName, err := ensureICAPSecurityPolicySet(key, policy, tenant)
	if err != nil {
		return nil, err
	}
	return &GuardrailIcapRefs{
		ICAPProfileRef: "/api/icapprofile?name=" + profName,
		SecurityHPSRef: "/api/httppolicyset?name=" + hpsName,
	}, nil
}

// DeleteGuardrailIcap removes the AKO-authored ICAP object graph. Order matters:
// the icapprofile references the pool group, and the VS (re-enqueued first) no
// longer references either object.
func DeleteGuardrailIcap(key string, policy *AIGuardrailPolicy) {
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
			} else {
				utils.AviLog.Infof("key: %s, msg: deleted %s/%s", key, api, name)
			}
		}
	}
	delByName("/api/httppolicyset", icapSecurityPolicySetName(policy))
	delByName("/api/icapprofile", icapProfileName(policy))
	delByName("/api/poolgroup", icapPoolGroupName(policy))
	delByName("/api/pool", icapPoolName(policy))
}

// ─── helper ──────────────────────────────────────────────────────────────────

// postOrPut creates the named object, or PUTs it if it already exists (idempotent
// reconcile). Mirrors the create-or-update dance in guardrail_rest.go/oauth_rest.go.
func postOrPut(client *clients.AviClient, api, name string, body interface{}) error {
	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, api+"?name="+name, &check)
	var resp interface{}
	if check.Count > 0 {
		if err := lib.AviPut(client, api+"/"+check.Results[0].UUID, body, &resp); err != nil {
			return fmt.Errorf("%s PUT %s: %w", api, name, err)
		}
		return nil
	}
	if err := lib.AviPost(client, api, body, &resp); err != nil {
		return fmt.Errorf("%s POST %s: %w", api, name, err)
	}
	return nil
}
