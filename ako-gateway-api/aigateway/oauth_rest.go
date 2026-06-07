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
	"net"
	"net/url"
	"strings"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/labels"

	avimodels "github.com/vmware/alb-sdk/go/models"
	avicache "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/cache"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// OAuth resource-server auth (Phase 2)
// ────────────────────────────────────
// Avi's SSO_TYPE_JWT validation strips the Authorization header before any
// DataScript runs and offers no claim-to-header injection on this build, so a
// token-rate-limit DataScript cannot read JWT claims (e.g. the per-group claim).
//
// The supported way to expose *validated* claims to a DataScript on Avi is the
// OAuth/OIDC resource-server flow: the SE validates the bearer token as an OAuth
// JWT access token and the DataScript reads claims via avi.http.oauth_get_claim().
// This requires (unlike the JWTServerProfile flow):
//   - an Avi Pool whose servers are the issuer's JWKS endpoint (the SE fetches
//     the keyset at runtime through this pool — it has no cluster DNS/egress),
//   - an AUTH_PROFILE_OAUTH AuthProfile referencing that pool + the issuer/JWKS,
//   - an SSO_TYPE_OAUTH SSO Policy,
//   - vs.oauth_vs_config with a resource_server{access_type: JWT} block.
// OAuth/OIDC on Avi requires the VS to terminate TLS, so the targeted Gateway
// listener must be HTTPS.
//
// The object graph is attached to the EVH child VS the same way the SSORule CRD
// attaches OauthVsConfig (see internal/nodes.BuildL7SSORule): AKO sets
// GeneratedFields.OauthVsConfig + SsoPolicyRef and publishes the VS holistically.

// ─── Name derivation ─────────────────────────────────────────────────────────

func issuerPoolName(policy *AIGatewayAuthPolicy) string {
	return fmt.Sprintf("%s-%s-oauth-pool", policy.Namespace, policy.Name)
}
func oauthAuthProfileName(policy *AIGatewayAuthPolicy) string {
	return fmt.Sprintf("%s-%s-oauth", policy.Namespace, policy.Name)
}
func oauthSSOPolicyName(policy *AIGatewayAuthPolicy) string {
	return fmt.Sprintf("%s-%s-oauth-sso", policy.Namespace, policy.Name)
}

// ─── Endpoint resolution ─────────────────────────────────────────────────────

// resolveIssuerServers turns the issuer JWKS URL into Service-Engine-reachable
// server IPs and a port. Avi SEs cannot resolve cluster DNS or reach ClusterIPs,
// so an in-cluster issuer (e.g. jwt-issuer.<ns>.svc.cluster.local) is resolved to
// its backing pod IPs via EndpointSlices. A literal IP host is used directly.
func resolveIssuerServers(rawURL string) (ips []string, port int32, err error) {
	u, perr := url.Parse(rawURL)
	if perr != nil {
		return nil, 0, fmt.Errorf("parse issuer url %q: %w", rawURL, perr)
	}
	host := u.Hostname()
	port = 8080
	if p := u.Port(); p != "" {
		fmt.Sscanf(p, "%d", &port)
	} else if u.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}

	// Literal IP — use directly.
	if ip := net.ParseIP(host); ip != nil {
		return []string{host}, port, nil
	}

	// Kubernetes service DNS: <svc>.<ns>.svc.cluster.local (or <svc>.<ns>).
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return nil, 0, fmt.Errorf("issuer host %q is not an IP or <svc>.<ns> DNS name", host)
	}
	svc, ns := parts[0], parts[1]

	sel := labels.Set{"kubernetes.io/service-name": svc}.AsSelector()
	slices, lerr := utils.GetInformers().EpSlicesInformer.Lister().EndpointSlices(ns).List(sel)
	if lerr != nil {
		return nil, 0, fmt.Errorf("list endpointslices for %s/%s: %w", ns, svc, lerr)
	}
	seen := map[string]bool{}
	for _, sl := range slices {
		for _, ep := range sl.Endpoints {
			// Only ready endpoints are valid JWKS targets.
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
		return nil, 0, fmt.Errorf("no ready endpoints found for issuer service %s/%s", ns, svc)
	}
	return ips, port, nil
}

// ─── Issuer Pool ─────────────────────────────────────────────────────────────

// EnsureIssuerPool creates/updates the Avi Pool the OAuth AuthProfile uses to
// reach the issuer's JWKS endpoint. Returns the pool name and the first server
// IP (used as the endpoint host so Avi's "endpoint host must be a pool server"
// validation passes) plus the port.
func EnsureIssuerPool(key string, policy *AIGatewayAuthPolicy) (poolName, firstIP string, port int32, err error) {
	jwksURI := policy.Spec.JWT.JwksUri
	if jwksURI == "" {
		return "", "", 0, fmt.Errorf("spec.jwt.jwksUri is required for OAuth resource-server auth")
	}
	ips, port, err := resolveIssuerServers(jwksURI)
	if err != nil {
		return "", "", 0, err
	}

	poolName = issuerPoolName(policy)
	tenant := lib.GetTenant()
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	servers := make([]*avimodels.Server, 0, len(ips))
	for i := range ips {
		ipType := "V4"
		servers = append(servers, &avimodels.Server{
			IP: &avimodels.IPAddr{Addr: proto.String(ips[i]), Type: proto.String(ipType)},
		})
	}
	// VRF is inherited from the VS/cloud context (set internally by pb-transform),
	// so it is intentionally not set here — matching a standalone pool create.
	pool := avimodels.Pool{
		Name:              proto.String(poolName),
		TenantRef:         proto.String("/api/tenant/?name=" + lib.GetEscapedValue(tenant)),
		CloudRef:          proto.String("/api/cloud/?name=" + utils.CloudName),
		DefaultServerPort: proto.Int32(port),
		Servers:           servers,
	}

	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID    string `json:"uuid"`
			Servers []struct {
				IP struct {
					Addr string `json:"addr"`
				} `json:"ip"`
			} `json:"servers"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/pool?name="+poolName, &check)
	if check.Count > 0 {
		// Avi blocks server modifications on a Pool bound to an OAuth Profile
		// ("Servers deleted for a Pool associated with an Oauth Profile"), so the
		// pool is treated as immutable here — left as-is once created. Return the
		// pool's OWN current server IP so the AuthProfile endpoint host always
		// matches a pool server (Avi rejects a mismatch). If the issuer endpoints
		// change, delete the AIGatewayAuthPolicy and re-apply to rebuild the pool.
		poolIP := ips[0]
		if len(check.Results[0].Servers) > 0 && check.Results[0].Servers[0].IP.Addr != "" {
			poolIP = check.Results[0].Servers[0].IP.Addr
		}
		utils.AviLog.Infof("key: %s, msg: issuer Pool %s already exists, using its server %s", key, poolName, poolIP)
		return poolName, poolIP, port, nil
	}
	var resp interface{}
	if err := lib.AviPost(client, "/api/pool", pool, &resp); err != nil {
		return "", "", 0, fmt.Errorf("issuer Pool POST %s: %w", poolName, err)
	}
	utils.AviLog.Infof("key: %s, msg: issuer Pool %s created (%d servers, port %d)", key, poolName, len(ips), port)
	return poolName, ips[0], port, nil
}

// ─── OAuth AuthProfile ───────────────────────────────────────────────────────

// EnsureOAuthAuthProfile creates/updates the AUTH_PROFILE_OAUTH that the SE uses
// to validate JWT access tokens. The OAuth endpoint hosts point at firstIP:port
// (so they match a pool server); the issuer stays the DNS issuer so it matches
// the token's `iss` claim.
func EnsureOAuthAuthProfile(key string, policy *AIGatewayAuthPolicy, poolName, firstIP string, port int32) (string, error) {
	name := oauthAuthProfileName(policy)
	tenant := lib.GetTenant()
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	ep := fmt.Sprintf("http://%s:%d", firstIP, port)
	profile := map[string]interface{}{
		"name":       name,
		"tenant_ref": "/api/tenant/?name=" + lib.GetEscapedValue(tenant),
		"type":       "AUTH_PROFILE_OAUTH",
		"oauth_profile": map[string]interface{}{
			"oauth_profile_type":     "CLIENT_OAUTH",
			"issuer":                 policy.Spec.JWT.Issuer,
			"jwks_uri":               ep + "/jwks",
			"authorization_endpoint": ep + "/authorize",
			"token_endpoint":         ep + "/token",
			"pool_ref":               "/api/pool/?name=" + poolName,
		},
	}

	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/authprofile?name="+name, &check)
	if check.Count > 0 {
		var resp interface{}
		if err := lib.AviPut(client, "/api/authprofile/"+check.Results[0].UUID, profile, &resp); err != nil {
			return "", fmt.Errorf("OAuth AuthProfile PUT %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: OAuth AuthProfile %s updated", key, name)
	} else {
		var resp interface{}
		if err := lib.AviPost(client, "/api/authprofile", profile, &resp); err != nil {
			return "", fmt.Errorf("OAuth AuthProfile POST %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: OAuth AuthProfile %s created", key, name)
	}
	return name, nil
}

// ─── OAuth SSO Policy ────────────────────────────────────────────────────────

// EnsureOAuthSSOPolicy creates/updates the SSO_TYPE_OAUTH policy referenced by
// the VS. Returns the policy name for use in SsoPolicyRef.
func EnsureOAuthSSOPolicy(key string, policy *AIGatewayAuthPolicy) (string, error) {
	name := oauthSSOPolicyName(policy)
	tenant := lib.GetTenant()
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	sso := avimodels.SSOPolicy{
		Name:      proto.String(name),
		TenantRef: proto.String("/api/tenant/?name=" + lib.GetEscapedValue(tenant)),
		Type:      proto.String("SSO_TYPE_OAUTH"),
		// Exempt the read-only dashboard counters endpoint from the OAuth
		// authorization-code flow. Without this, GET /v1/admin/counters would be
		// 302'd into the login redirect and never reach the DataScript that gates
		// it on X-Admin-Token. The path lives under the route's /v1 prefix so the
		// EVH parent still content-switches it to this child VS.
		AuthenticationPolicy: &avimodels.AuthenticationPolicy{
			AuthnRules: []*avimodels.AuthenticationRule{{
				Name:   proto.String("ai-admin-skip"),
				Index:  proto.Int32(1),
				Enable: proto.Bool(true),
				Action: &avimodels.AuthenticationAction{Type: proto.String("SKIP_AUTHENTICATION")},
				Match: &avimodels.AuthenticationMatch{
					Path: &avimodels.PathMatch{
						MatchCriteria: proto.String("BEGINS_WITH"),
						MatchStr:      []string{"/v1/admin/"},
					},
				},
			}},
		},
	}

	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/ssopolicy?name="+name, &check)
	if check.Count > 0 {
		var resp interface{}
		if err := lib.AviPut(client, "/api/ssopolicy/"+check.Results[0].UUID, sso, &resp); err != nil {
			return "", fmt.Errorf("OAuth SSOPolicy PUT %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: OAuth SSOPolicy %s updated", key, name)
	} else {
		var resp interface{}
		if err := lib.AviPost(client, "/api/ssopolicy", sso, &resp); err != nil {
			return "", fmt.Errorf("OAuth SSOPolicy POST %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: OAuth SSOPolicy %s created", key, name)
	}
	return name, nil
}

// ─── Deletion ────────────────────────────────────────────────────────────────

// DeleteOAuthObjects removes the AKO-managed OAuth object graph for the policy.
func DeleteOAuthObjects(key string, policy *AIGatewayAuthPolicy) {
	tenant := lib.GetTenant()
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
	// Order matters: SSO + AuthProfile reference the pool, so delete them first.
	delByName("/api/ssopolicy", oauthSSOPolicyName(policy))
	delByName("/api/authprofile", oauthAuthProfileName(policy))
	delByName("/api/pool", issuerPoolName(policy))
}
