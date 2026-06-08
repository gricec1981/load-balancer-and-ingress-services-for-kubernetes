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
	"io"
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"

	avimodels "github.com/vmware/alb-sdk/go/models"
	avicache "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/cache"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// JWT bearer auth via query param (ClaimModeJWTQuery)
// ──────────────────────────────────────────────────
// This is the stateless, machine-client counterpart to the OAuth flow in
// oauth_rest.go. The SE validates a JWT presented as a query parameter
// (jwt_location=JWT_LOCATION_QUERY_PARAM) — 200 on a valid token, 401 otherwise,
// with NO redirect — so SDK/agent clients authenticate per request without a
// browser. Unlike the Authorization header (which the SE strips before any
// DataScript runs), the query param survives, so the AKO claim helper
// base64url-decodes the SE-validated token to read claims (see jwtClaimHelper).
//
// Object graph (mirrors the JWT validation chain confirmed on Avi 32.1.1):
//   JWTServerProfile{jwt_profile_type: CLIENT_AUTH, issuer, jwks_keys}
//     → AuthProfile{type: AUTH_PROFILE_JWT, jwt_profile_ref}
//       → SSOPolicy{type: SSO_TYPE_JWT, authentication_policy.default_auth_profile_ref}
//         → VS.jwt_config{audience, jwt_location: QUERY_PARAM, jwt_name}
//
// The SE has no cluster DNS/egress, so the JWKS keyset cannot be fetched by the
// SE at runtime; AKO (in-cluster) fetches it from spec.jwt.jwksUri and embeds the
// keyset inline in jwks_keys. Re-apply the policy to refresh rotated keys.

// JwtQueryParamName is the query-param key the SE looks for the JWT under, and
// that the claim helper reads. Kept short and unsurprising for client ergonomics.
const JwtQueryParamName = "jwt"

// ─── Name derivation ─────────────────────────────────────────────────────────

func jwtServerProfileName(policy *AIGatewayAuthPolicy) string {
	return fmt.Sprintf("%s-%s-jwt-srv", policy.Namespace, policy.Name)
}
func jwtAuthProfileName(policy *AIGatewayAuthPolicy) string {
	return fmt.Sprintf("%s-%s-jwt-auth", policy.Namespace, policy.Name)
}
func jwtSSOPolicyName(policy *AIGatewayAuthPolicy) string {
	return fmt.Sprintf("%s-%s-jwt-sso", policy.Namespace, policy.Name)
}

// ─── JWKS fetch ──────────────────────────────────────────────────────────────

// fetchJWKS retrieves the JWKS document from the issuer. AKO runs in-cluster and
// can reach a ClusterIP/Service-DNS issuer that the SE cannot, so the keyset is
// fetched here and embedded in the JWTServerProfile.
func fetchJWKS(jwksURI string) (string, error) {
	if jwksURI == "" {
		return "", fmt.Errorf("spec.jwt.jwksUri is required for jwtQuery auth")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(jwksURI)
	if err != nil {
		return "", fmt.Errorf("GET JWKS %s: %w", jwksURI, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET JWKS %s: status %d", jwksURI, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return "", fmt.Errorf("read JWKS %s: %w", jwksURI, err)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("JWKS %s returned an empty body", jwksURI)
	}
	return string(body), nil
}

// ─── JWT Server Profile ──────────────────────────────────────────────────────

// EnsureJWTServerProfile creates/updates the CLIENT_AUTH JWTServerProfile that
// holds the issuer and the fetched JWKS keyset the SE validates tokens against.
func EnsureJWTServerProfile(key string, policy *AIGatewayAuthPolicy) (string, error) {
	jwks, err := fetchJWKS(policy.Spec.JWT.JwksUri)
	if err != nil {
		return "", err
	}
	name := jwtServerProfileName(policy)
	tenant := lib.GetTenant()
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	profile := avimodels.JWTServerProfile{
		Name:           proto.String(name),
		TenantRef:      proto.String("/api/tenant/?name=" + lib.GetEscapedValue(tenant)),
		JwtProfileType: proto.String("CLIENT_AUTH"),
		Issuer:         proto.String(policy.Spec.JWT.Issuer),
		JwksKeys:       proto.String(jwks),
	}

	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/jwtserverprofile?name="+name, &check)
	if check.Count > 0 {
		var resp interface{}
		if err := lib.AviPut(client, "/api/jwtserverprofile/"+check.Results[0].UUID, profile, &resp); err != nil {
			return "", fmt.Errorf("JWTServerProfile PUT %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: JWTServerProfile %s updated", key, name)
	} else {
		var resp interface{}
		if err := lib.AviPost(client, "/api/jwtserverprofile", profile, &resp); err != nil {
			return "", fmt.Errorf("JWTServerProfile POST %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: JWTServerProfile %s created", key, name)
	}
	return name, nil
}

// ─── JWT Auth Profile ────────────────────────────────────────────────────────

// EnsureJWTAuthProfile creates/updates the AUTH_PROFILE_JWT that references the
// JWTServerProfile.
func EnsureJWTAuthProfile(key string, policy *AIGatewayAuthPolicy, serverProfileName string) (string, error) {
	name := jwtAuthProfileName(policy)
	tenant := lib.GetTenant()
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	profile := avimodels.AuthProfile{
		Name:          proto.String(name),
		TenantRef:     proto.String("/api/tenant/?name=" + lib.GetEscapedValue(tenant)),
		Type:          proto.String("AUTH_PROFILE_JWT"),
		JwtProfileRef: proto.String("/api/jwtserverprofile/?name=" + serverProfileName),
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
			return "", fmt.Errorf("JWT AuthProfile PUT %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: JWT AuthProfile %s updated", key, name)
	} else {
		var resp interface{}
		if err := lib.AviPost(client, "/api/authprofile", profile, &resp); err != nil {
			return "", fmt.Errorf("JWT AuthProfile POST %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: JWT AuthProfile %s created", key, name)
	}
	return name, nil
}

// ─── JWT SSO Policy ──────────────────────────────────────────────────────────

// EnsureJWTSSOPolicy creates/updates the SSO_TYPE_JWT policy the VS references.
// The same admin-skip authn rule as the OAuth path keeps the read-only counters
// endpoint (GET /v1/admin/...) reachable without a token.
func EnsureJWTSSOPolicy(key string, policy *AIGatewayAuthPolicy, authProfileName string) (string, error) {
	name := jwtSSOPolicyName(policy)
	tenant := lib.GetTenant()
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	sso := avimodels.SSOPolicy{
		Name:      proto.String(name),
		TenantRef: proto.String("/api/tenant/?name=" + lib.GetEscapedValue(tenant)),
		Type:      proto.String("SSO_TYPE_JWT"),
		AuthenticationPolicy: &avimodels.AuthenticationPolicy{
			DefaultAuthProfileRef: proto.String("/api/authprofile/?name=" + authProfileName),
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
			return "", fmt.Errorf("JWT SSOPolicy PUT %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: JWT SSOPolicy %s updated", key, name)
	} else {
		var resp interface{}
		if err := lib.AviPost(client, "/api/ssopolicy", sso, &resp); err != nil {
			return "", fmt.Errorf("JWT SSOPolicy POST %s: %w", name, err)
		}
		utils.AviLog.Infof("key: %s, msg: JWT SSOPolicy %s created", key, name)
	}
	return name, nil
}

// ─── Deletion ────────────────────────────────────────────────────────────────

// DeleteJWTObjects removes the AKO-managed JWT object graph for the policy.
func DeleteJWTObjects(key string, policy *AIGatewayAuthPolicy) {
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
	// Order matters: SSO + AuthProfile reference the server profile.
	delByName("/api/ssopolicy", jwtSSOPolicyName(policy))
	delByName("/api/authprofile", jwtAuthProfileName(policy))
	delByName("/api/jwtserverprofile", jwtServerProfileName(policy))
}
