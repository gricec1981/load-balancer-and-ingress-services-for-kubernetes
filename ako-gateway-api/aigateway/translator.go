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

	"google.golang.org/protobuf/proto"

	avimodels "github.com/vmware/alb-sdk/go/models"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
	akov1alpha2 "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/apis/ako/v1alpha2"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// ApplyAuthPolicy configures the Avi VS node model to enforce JWT authentication
// as described in the AIGatewayAuthPolicy spec.
//
// Avi object mapping (Phase 1.5):
//   - JWTServerProfile: created/updated in Avi with issuer + JWKS keys fetched
//     from spec.jwt.jwksUri.  Name: "<ns>-<name>-jwt".
//   - AuthProfile (JWT): wraps the JWTServerProfile. Name: "<ns>-<name>-jwt-auth".
//   - SSOPolicy (SSO_TYPE_JWT): references the AuthProfile. Name: "<ns>-<name>-sso".
//     AKO sets VirtualService.SsoPolicyRef to this policy's Avi API path.
//   - Identity header: HTTP request policy rules copy the Avi-injected JWT claim
//     headers (X-AVI-JWT-<CLAIM>) into the user-configured header names so that
//     downstream policies (e.g. AITokenRateLimitPolicy) have stable names.
func ApplyAuthPolicy(key string, policy *AIGatewayAuthPolicy, vsNode nodes.AviVsEvhSniModel, host string) {
	if policy == nil {
		return
	}
	spec := policy.Spec

	// ── 1. Ensure issuer Pool + OAUTH AuthProfile + OAUTH SSOPolicy in Avi ────
	// SSO_TYPE_JWT validation strips the Authorization header before any
	// DataScript runs, so a token-rate-limit DataScript cannot read JWT claims.
	// We use the OAuth resource-server flow instead: the SE validates the bearer
	// token as an OAuth JWT access token and the DataScript reads claims via
	// avi.http.oauth_get_claim(). See oauth_rest.go for the object graph.
	poolName, firstIP, port, err := EnsureIssuerPool(key, policy)
	if err != nil {
		utils.AviLog.Errorf("key: %s, msg: AIGatewayAuthPolicy %s/%s: issuer Pool error: %v",
			key, policy.Namespace, policy.Name, err)
		return
	}
	authProfileName, err := EnsureOAuthAuthProfile(key, policy, poolName, firstIP, port)
	if err != nil {
		utils.AviLog.Errorf("key: %s, msg: AIGatewayAuthPolicy %s/%s: OAuth AuthProfile error: %v",
			key, policy.Namespace, policy.Name, err)
		return
	}
	ssoPolicyName, err := EnsureOAuthSSOPolicy(key, policy)
	if err != nil {
		utils.AviLog.Errorf("key: %s, msg: AIGatewayAuthPolicy %s/%s: OAuth SSOPolicy error: %v",
			key, policy.Namespace, policy.Name, err)
		return
	}

	audience := "*"
	if len(spec.JWT.Audiences) > 0 {
		audience = spec.JWT.Audiences[0]
	}

	// ── 2. Attach OauthVsConfig + SsoPolicyRef to the EVH child VS ────────────
	// Same mechanism the SSORule CRD uses (internal/nodes.BuildL7SSORule): set
	// the VS GeneratedFields and let AKO publish the VS holistically. OAuth/OIDC
	// requires the VS to terminate TLS, so the Gateway listener must be HTTPS.
	if host == "" {
		if names := vsNode.GetVHDomainNames(); len(names) > 0 {
			host = names[0]
		} else {
			host = "ai-gateway.local"
		}
	}
	gf := vsNode.GetGeneratedFields()
	gf.SsoPolicyRef = proto.String(fmt.Sprintf("/api/ssopolicy?name=%s", ssoPolicyName))
	gf.JwtConfig = nil // not used in OAuth mode
	gf.OauthVsConfig = &akov1alpha2.OAuthVSConfig{
		CookieName:    proto.String("AVI-OAUTH-SESSION"),
		CookieTimeout: proto.Int32(60),
		// The OAuth callback must land on this EVH child VS, which the parent only
		// content-switches to for the route's path prefix. Put the callback under
		// that prefix so it reaches the child's OAuth module instead of 404ing.
		RedirectURI: proto.String(fmt.Sprintf("https://%s/v1/oauth/callback", host)),
		OauthSettings: []*akov1alpha2.OAuthSettings{{
			AuthProfileRef: proto.String(fmt.Sprintf("/api/authprofile?name=%s", authProfileName)),
			// app_settings are mandatory on oauth_vs_config even in resource-server
			// mode; OIDC interactive login is disabled so only bearer validation runs.
			AppSettings: &akov1alpha2.OAuthAppSettings{
				ClientID:     proto.String(audience),
				ClientSecret: proto.String("unused-resource-server"),
				OidcConfig: &akov1alpha2.OIDCConfig{
					OidcEnable: proto.Bool(false),
					Profile:    proto.Bool(false),
					Userinfo:   proto.Bool(false),
				},
			},
			ResourceServer: &akov1alpha2.OAuthResourceServer{
				AccessType: proto.String("ACCESS_TOKEN_TYPE_JWT"),
				JwtParams:  &akov1alpha2.JWTValidationParams{Audience: proto.String(audience)},
			},
		}},
	}
	utils.AviLog.Infof("key: %s, msg: AIGatewayAuthPolicy %s/%s: set OAuth SsoPolicyRef → %s (audience=%s, host=%s)",
		key, policy.Namespace, policy.Name, ssoPolicyName, audience, host)
}

// ApplyTokenRateLimitPolicy configures the Avi VS node model to enforce the
// token-budget and request-rate limits in the AITokenRateLimitPolicy spec.
//
// Avi object mapping (Phase 1):
//   - requestRateLimit: soft per-consumer token-bucket rate limiter implemented
//     in the DataScript REQ phase via avi.vs.table_insert/lookup.
//     Phase 1.5 upgrade path: extend AviHTTPDataScriptNode with
//     RateLimiters []*models.RateLimiter and call avi.vs.rate_limiter() for
//     the native distributed rate limiter.
//   - token limits:     two AviHTTPDataScriptNode entries (HTTP_REQ enforcement
//     + HTTP_RESP accounting) are added to the VS's HTTPDSrefs slice.
//     Names are scoped to the VS to avoid collisions across policies.
func ApplyTokenRateLimitPolicy(key string, policy *AITokenRateLimitPolicy, vsNode nodes.AviVsEvhSniModel) {
	if policy == nil {
		return
	}
	spec := policy.Spec
	hasTokenLimits := len(spec.Limits) > 0
	hasRateLimit := spec.RequestRateLimit != nil

	if !hasTokenLimits && !hasRateLimit {
		return
	}

	scripts := GenerateTokenAccountingScripts(policy)
	reqScript := scripts.ReqScript
	respScript := scripts.RespScript

	// Prepend RPS rate-limit logic to the REQ DataScript when configured.
	if hasRateLimit {
		rlScript := buildRequestRateLimitScript(spec)
		reqScript = rlScript + "\n\n" + reqScript
	}

	vsName := vsNode.GetName()
	tenant := vsNode.GetTenant()

	if hasTokenLimits || hasRateLimit {
		addDataScriptNode(key, vsName, tenant, DSReqName(vsName), DSEvtHTTPReq, reqScript, vsNode)
	}
	if hasTokenLimits {
		addDataScriptNode(key, vsName, tenant, DSRespName(vsName), DSEvtHTTPResp, respScript, vsNode)
	}

	utils.AviLog.Infof("key: %s, msg: AITokenRateLimitPolicy %s/%s: registered token-accounting DataScripts on VS %s",
		key, policy.Namespace, policy.Name, vsName)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// derivedSSOPolicyName generates the expected Avi SSO Policy name for a policy.
func derivedSSOPolicyName(policy *AIGatewayAuthPolicy) string {
	return fmt.Sprintf("%s-%s-sso", policy.Namespace, policy.Name)
}

// aviJWTClaimHeader returns the header name Avi's SSO policy injects for a given JWT claim.
func aviJWTClaimHeader(claim string) string {
	return "X-AVI-JWT-" + strings.ToUpper(claim)
}

// buildClaimForwardRules returns HTTPRequestRules that copy Avi-injected JWT
// claim headers into user-configured header names.
func buildClaimForwardRules(srcIdentityHeader, dstIdentityHeader string, forwardClaims []string, startIndex int32) []*avimodels.HTTPRequestRule {
	var rules []*avimodels.HTTPRequestRule
	idx := startIndex

	rules = append(rules, buildHeaderCopyRule(
		fmt.Sprintf("ai-auth-identity-%d", idx),
		idx,
		srcIdentityHeader,
		dstIdentityHeader,
	))
	idx++

	for _, claim := range forwardClaims {
		src := aviJWTClaimHeader(claim)
		dst := strings.ToLower(claim)
		rules = append(rules, buildHeaderCopyRule(
			fmt.Sprintf("ai-auth-fwd-%s-%d", dst, idx),
			idx,
			src,
			dst,
		))
		idx++
	}
	return rules
}

// buildHeaderCopyRule constructs an HTTPRequestRule that copies srcHeader → dstHeader.
// Avi HTTP policy variables for request headers use the form "$http_<name>" where
// the header name has dashes replaced with underscores and is lowercased.
// The rule adds dstHeader carrying the value of srcHeader.
func buildHeaderCopyRule(ruleName string, index int32, srcHeader, dstHeader string) *avimodels.HTTPRequestRule {
	// Convert "X-AVI-JWT-SUB" → "$http_x_avi_jwt_sub" per Avi variable naming.
	srcVar := "$http_" + strings.ToLower(strings.ReplaceAll(srcHeader, "-", "_"))
	return &avimodels.HTTPRequestRule{
		Name:   proto.String(ruleName),
		Enable: proto.Bool(true),
		Index:  proto.Int32(index),
		HdrAction: []*avimodels.HTTPHdrAction{
			{
				Action: proto.String("HTTP_ADD_HDR"),
				Hdr: &avimodels.HTTPHdrData{
					Name: proto.String(dstHeader),
					Value: &avimodels.HTTPHdrValue{
						Var: proto.String("HTTP_POLICY_VAR_HTTP_HDR"),
						Val: proto.String(srcVar),
					},
				},
			},
		},
	}
}

// buildRequestRateLimitScript returns the Lua snippet for per-consumer RPS rate
// limiting using a per-SE soft token bucket.
//
// Phase 1.5 upgrade path: use avi.vs.rate_limiter() (native distributed limiter)
// by extending AviHTTPDataScriptNode with RateLimiters []*models.RateLimiter.
func buildRequestRateLimitScript(spec AITokenRateLimitPolicySpec) string {
	rl := spec.RequestRateLimit
	burst := rl.Burst
	if burst <= 0 {
		burst = rl.RequestsPerSecond
	}

	var keyExpr string
	switch rl.Key {
	case "consumer":
		hdr := spec.EffectiveIdentityHeader()
		keyExpr = fmt.Sprintf(`(avi.http.get_header(%q, avi.HTTP_REQUEST) or avi.vs.client_ip())`, hdr)
	default:
		keyExpr = "avi.vs.client_ip()"
	}

	// avi.vs.table_* take (key[, value[, ttl]]) - no table-name argument - and
	// table_insert does not overwrite, so remove-then-insert to update the count.
	return fmt.Sprintf(`-- AKO AI Gateway: per-consumer RPS soft rate limiter
-- Phase 1.5 upgrade: replace with native avi.vs.rate_limiter() for cross-SE consistency.
do
  local rk = "rps:"..%s
  local win_key = rk..":"..math.floor(os.time())
  local cur = tonumber(avi.vs.table_lookup(win_key) or 0)
  if cur >= %d then
    avi.http.response(429,
      {["Content-Type"] = "application/json", ["Retry-After"] = "1"},
      '{"error":"rate_limit_exceeded","limit":"requests_per_second","budget":%d}')
    return
  end
  avi.vs.table_remove(win_key)
  avi.vs.table_insert(win_key, tostring(cur + 1), 2)
end`, keyExpr, burst, rl.RequestsPerSecond)
}

// findOrCreateHTTPPolicySet finds an existing HTTPPolicySetNode on the VS by
// name, or creates and appends a new one.
func findOrCreateHTTPPolicySet(name string, vsNode nodes.AviVsEvhSniModel) *nodes.AviHttpPolicySetNode {
	for _, ps := range vsNode.GetHttpPolicyRefs() {
		if ps.Name == name {
			return ps
		}
	}
	ps := &nodes.AviHttpPolicySetNode{
		Name:   name,
		Tenant: vsNode.GetTenant(),
	}
	vsNode.SetHttpPolicyRefs(append(vsNode.GetHttpPolicyRefs(), ps))
	return ps
}

// addDataScriptNode adds or replaces an AviHTTPDataScriptNode in the VS's
// HTTPDSrefs list (idempotent: replaces by name on reconcile).
func addDataScriptNode(key, vsName, tenant, dsName, evt, script string, vsNode nodes.AviVsEvhSniModel) {
	ds := &nodes.AviHTTPDataScriptNode{
		Name:   dsName,
		Tenant: tenant,
		DataScript: &nodes.DataScript{
			Evt:    evt,
			Script: script,
		},
	}

	existing := vsNode.GetHTTPDSrefs()
	for i, e := range existing {
		if e.Name == dsName {
			existing[i] = ds
			vsNode.SetHTTPDSrefs(existing)
			utils.AviLog.Debugf("key: %s, msg: replaced DataScript %s on VS %s", key, dsName, vsName)
			return
		}
	}
	vsNode.SetHTTPDSrefs(append(existing, ds))
	utils.AviLog.Debugf("key: %s, msg: added DataScript %s on VS %s", key, dsName, vsName)
}
