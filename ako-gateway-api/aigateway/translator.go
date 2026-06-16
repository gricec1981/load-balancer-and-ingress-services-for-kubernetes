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

// ApplyAuthPolicy configures the Avi VS node model to enforce OAuth/OIDC
// authentication as described in the AIGatewayAuthPolicy spec.
//
// Avi's SSO_TYPE_JWT validation strips the Authorization header before any
// DataScript runs (and 31.2 offers no claim-to-header injection), so the
// AI-Gateway uses the OAuth resource-server flow instead: the SE runs the OIDC
// auth-code flow, validates the access token, and exposes its claims to the
// token-budget DataScript via avi.http.oauth_get_claim(). OAuth/OIDC requires
// the Gateway listener to terminate TLS (HTTPS).
//
// Avi object mapping (see oauth_rest.go):
//   - Pool "<ns>-<name>-oauth-pool": servers = the issuer Service's endpoints
//     (so the SE can fetch JWKS / exchange the auth code at runtime).
//   - AuthProfile (AUTH_PROFILE_OAUTH) "<ns>-<name>-oauth": issuer + jwks_uri,
//     pointing at the pool.
//   - SSOPolicy (SSO_TYPE_OAUTH) "<ns>-<name>-oauth-sso".
//   - The VS GeneratedFields get OauthVsConfig (client app_settings +
//     resource_server access_type JWT) and SsoPolicyRef, attached the same way
//     the SSORule CRD attaches OAuth to an EVH child.
func ApplyAuthPolicy(key string, policy *AIGatewayAuthPolicy, vsNode nodes.AviVsEvhSniModel, host, routePrefix string) {
	if policy == nil {
		return
	}
	spec := policy.Spec

	// jwtQuery mode validates a stateless bearer JWT presented as a query param
	// (machine clients) instead of running the OAuth browser flow. The claim
	// helper decodes the SE-validated token from the query string — see
	// applyJWTQueryAuth / jwtClaimHelper(ClaimModeJWTQuery).
	if spec.EffectiveAuthMode() == ClaimModeJWTQuery {
		applyJWTQueryAuth(key, policy, vsNode)
		return
	}

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
		// that prefix (derived from the route, not hardcoded) so it reaches the
		// child's OAuth module instead of 404ing — e.g. /v1/... for the LLM route,
		// /mcp/... for the MCP route.
		RedirectURI: proto.String(fmt.Sprintf("https://%s%s/oauth/callback", host, strings.TrimRight(routePrefix, "/"))),
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

// applyJWTQueryAuth wires the SSO_TYPE_JWT object graph and the VS jwt_config for
// stateless bearer auth via a query parameter (ClaimModeJWTQuery). The SE
// validates the JWT found at ?<jwt>=<token>; the claim helper in the AKO
// DataScripts base64url-decodes the same (validated) token to read claims —
// avi.http.oauth_get_claim is not available in this mode.
//
// Security note (token-in-URL): unlike the Authorization header, the query param
// is not stripped, which is what makes the claims readable — but it also means
// the token can land in access/proxy logs and is forwarded to the backend. Run
// this only over TLS, with short-lived tokens, SE query-param log redaction, and
// (where supported) a query-strip before the pool. See docs/gateway-api.
func applyJWTQueryAuth(key string, policy *AIGatewayAuthPolicy, vsNode nodes.AviVsEvhSniModel) {
	spec := policy.Spec

	serverProfileName, err := EnsureJWTServerProfile(key, policy)
	if err != nil {
		utils.AviLog.Errorf("key: %s, msg: AIGatewayAuthPolicy %s/%s: JWTServerProfile error: %v",
			key, policy.Namespace, policy.Name, err)
		return
	}
	authProfileName, err := EnsureJWTAuthProfile(key, policy, serverProfileName)
	if err != nil {
		utils.AviLog.Errorf("key: %s, msg: AIGatewayAuthPolicy %s/%s: JWT AuthProfile error: %v",
			key, policy.Namespace, policy.Name, err)
		return
	}
	ssoPolicyName, err := EnsureJWTSSOPolicy(key, policy, authProfileName)
	if err != nil {
		utils.AviLog.Errorf("key: %s, msg: AIGatewayAuthPolicy %s/%s: JWT SSOPolicy error: %v",
			key, policy.Namespace, policy.Name, err)
		return
	}

	audience := "*"
	if len(spec.JWT.Audiences) > 0 {
		audience = spec.JWT.Audiences[0]
	}

	gf := vsNode.GetGeneratedFields()
	// Mutually exclusive with the OAuth path; clear it so a mode switch on an
	// existing policy cleanly replaces the config.
	gf.OauthVsConfig = nil
	gf.SsoPolicyRef = proto.String(fmt.Sprintf("/api/ssopolicy?name=%s", ssoPolicyName))
	gf.JwtConfig = &avimodels.JWTValidationVsConfig{
		Audience:    proto.String(audience),
		JwtLocation: proto.String("JWT_LOCATION_QUERY_PARAM"),
		JwtName:     proto.String(JwtQueryParamName),
	}
	utils.AviLog.Infof("key: %s, msg: AIGatewayAuthPolicy %s/%s: set JWT-query auth (sso=%s, audience=%s, jwt_name=%s)",
		key, policy.Namespace, policy.Name, ssoPolicyName, audience, JwtQueryParamName)
}

// ApplyTokenRateLimitPolicy configures the Avi VS node model to enforce the
// token-budget and request-rate limits in the AITokenRateLimitPolicy spec.
//
// Avi object mapping:
//   - requestRateLimit: native Avi rate limiter. A models.RateLimiter
//     (count/period/burst) is published on the request-phase VSDataScriptSet
//     (rate_limiters) and the REQ DataScript calls avi.vs.ratelimit.exceed()
//     against it, keyed per-consumer. The SE owns the token bucket and keeps it
//     consistent across VS scale-out (distributed) — replacing the per-SE,
//     eventually-consistent table_insert/lookup soft bucket.
//   - token limits:     two AviHTTPDataScriptNode entries (HTTP_REQ enforcement
//   - HTTP_RESP accounting) are added to the VS's HTTPDSrefs slice. Token budgets
//     stay in DataScript (per-group/tier ceilings, post-response accounting,
//     fixed-window reset and the counters endpoint don't map onto the native
//     limiter). Names are scoped to the VS to avoid collisions across policies.
func ApplyTokenRateLimitPolicy(key string, policy *AITokenRateLimitPolicy, vsNode nodes.AviVsEvhSniModel, mode AuthClaimMode) {
	if policy == nil {
		return
	}
	spec := policy.Spec
	hasTokenLimits := len(spec.Limits) > 0
	hasRateLimit := spec.RequestRateLimit != nil

	if !hasTokenLimits && !hasRateLimit {
		return
	}

	scripts := GenerateTokenAccountingScripts(policy, mode)
	reqScript := scripts.ReqScript

	vsName := vsNode.GetName()
	tenant := vsNode.GetTenant()

	// Prepend RPS rate-limit logic to the REQ DataScript when configured, and
	// attach the matching native rate limiter to the same VSDataScriptSet. The SE
	// owns the (distributed, VS-scale-out-aware) token bucket; the script only
	// calls avi.vs.ratelimit.exceed() against it.
	var reqRateLimiters []*avimodels.RateLimiter
	if hasRateLimit {
		rlName := RPSRateLimiterName(vsName)
		rlScript := buildRequestRateLimitScript(spec, rlName)
		reqScript = rlScript + "\n\n" + reqScript
		reqRateLimiters = []*avimodels.RateLimiter{buildRequestRateLimiter(spec, rlName)}
	}

	if hasTokenLimits || hasRateLimit {
		addDataScriptNode(key, vsName, tenant, DSReqName(vsName), DSEvtHTTPReq, reqScript, reqRateLimiters, vsNode)
	}
	if hasTokenLimits {
		// HTTP_RESP enables response-body buffering; HTTP_RESP_DATA reads the
		// buffered body and does the token accounting (parses usage directly
		// from the JSON body, no backend token-header dependency).
		addDataScriptNode(key, vsName, tenant, DSRespName(vsName), DSEvtHTTPResp, scripts.RespScript, nil, vsNode)
		addDataScriptNode(key, vsName, tenant, DSRespDataName(vsName), DSEvtHTTPRespData, scripts.RespDataScript, nil, vsNode)
	}
	// Tier-dependent budgets enforce in HTTP_REQ_DATA so they can read the ai_tier
	// reqvar set by the AIModelRoutePolicy script. ApplyModelRoutePolicy is invoked
	// before this, so its DataScript carries a lower index and runs first (verified:
	// reqvars cross DataScriptSets and execution follows index order).
	if scripts.ReqDataEnforceScript != "" {
		addDataScriptNode(key, vsName, tenant, DSReqDataEnforceName(vsName), DSEvtHTTPReqData, scripts.ReqDataEnforceScript, nil, vsNode)
	}

	utils.AviLog.Infof("key: %s, msg: AITokenRateLimitPolicy %s/%s: registered token-accounting DataScripts on VS %s",
		key, policy.Namespace, policy.Name, vsName)
}

// ApplyGuardrailPolicy authors the Avi objects for the guardrail spec and
// attaches them to the VS. Two independent layers, both authored by AKO:
//
//   - Signature (WAF): a WafPolicy (DLP + known-phrase injection) set on the VS
//     via waf_policy_ref. Attachment could also be done with the L7Rule CRD, but
//     L7Rule only references a WafPolicy by name; nothing else creates it.
//   - Semantic (ICAP): a prompt-injection classifier the SE calls over ICAP
//     REQMOD, for the novel/paraphrased injection the signature layer misses.
//     This needs TWO Avi objects — the icapprofile AND an HTTPPolicySet whose
//     security rule action is REQUEST_CHECK_ICAP (attaching the profile alone is
//     a no-op; Spike-1) — set on the VS via icap_request_profile_refs and
//     http_policies respectively.
//
// The layers compose: on 32.1.1 WAF short-circuits before ICAP; on 31.2.1 the
// ICAP check runs first. Either way both block before the request is forwarded.
func ApplyGuardrailPolicy(key string, policy *AIGuardrailPolicy, vsNode nodes.AviVsEvhSniModel) {
	if policy == nil {
		return
	}

	// ── Signature layer (WafPolicy) — skipped for a semantic-only policy. ──
	if policy.Spec.Resolve().HasAny() {
		name, err := EnsureGuardrailWafPolicy(key, policy)
		if err != nil {
			utils.AviLog.Warnf("key: %s, msg: AIGuardrailPolicy %s/%s: %v", key, policy.Namespace, policy.Name, err)
		} else {
			vsNode.SetWafPolicyRef(proto.String("/api/wafpolicy?name=" + name))
			utils.AviLog.Infof("key: %s, msg: AIGuardrailPolicy %s/%s: attached WAF guardrail %s on VS %s",
				key, policy.Namespace, policy.Name, name, vsNode.GetName())
		}
	}

	// ── Semantic layer (ICAP classifier) — author the icapprofile AND the ──
	// REQUEST_CHECK_ICAP HTTPPolicySet, then set both on the VS.
	if policy.Spec.SemanticEnabled() {
		refs, err := EnsureGuardrailIcap(key, policy)
		if err != nil {
			utils.AviLog.Warnf("key: %s, msg: AIGuardrailPolicy %s/%s: semantic ICAP: %v", key, policy.Namespace, policy.Name, err)
			return
		}
		if refs != nil {
			vsNode.SetICAPProfileRefs([]string{refs.ICAPProfileRef})
			// Merge (not overwrite) the security HTTPPolicySet: L7Rule/HTTPRoute
			// filters may also program http_policies. De-dup by ref.
			existing := vsNode.GetHttpPolicySetRefs()
			found := false
			for _, r := range existing {
				if r == refs.SecurityHPSRef {
					found = true
					break
				}
			}
			if !found {
				existing = append(existing, refs.SecurityHPSRef)
			}
			vsNode.SetHttpPolicySetRefs(existing)
			utils.AviLog.Infof("key: %s, msg: AIGuardrailPolicy %s/%s: attached semantic ICAP (%s + %s) on VS %s",
				key, policy.Namespace, policy.Name, refs.ICAPProfileRef, refs.SecurityHPSRef, vsNode.GetName())
		}
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// requestRateKeyExpr returns the Lua expression that resolves the rate-limiter
// request_key (the per-consumer bucket selector). "consumer" prefers the resolved
// identity header (set by AIGatewayAuthPolicy) and falls back to the client IP;
// anything else keys on the client IP.
func requestRateKeyExpr(spec AITokenRateLimitPolicySpec) string {
	if spec.RequestRateLimit.Key == "consumer" {
		hdr := spec.EffectiveIdentityHeader()
		return fmt.Sprintf(`(avi.http.get_header(%q, avi.HTTP_REQUEST) or avi.vs.client_ip())`, hdr)
	}
	return "avi.vs.client_ip()"
}

// buildRequestRateLimitScript returns the Lua snippet for per-consumer RPS rate
// limiting using the native Avi rate limiter. The bucket math (count/period/burst)
// lives in the RateLimiter object on the VSDataScriptSet (see
// buildRequestRateLimiter); the script just consumes one token per request from
// the per-consumer bucket via avi.vs.ratelimit.exceed(name, request_key). The SE
// keeps the bucket consistent across VS scale-out, so this is exact across SEs
// (unlike the previous per-SE table_insert/lookup soft bucket).
func buildRequestRateLimitScript(spec AITokenRateLimitPolicySpec, rlName string) string {
	keyExpr := requestRateKeyExpr(spec)
	return fmt.Sprintf(`-- AKO AI Gateway: per-consumer RPS rate limiter (native Avi rate limiter)
-- Buckets per consumer via request_key against the %q rate limiter on this
-- DataScriptSet. The SE owns the token bucket and keeps it consistent across VS
-- scale-out, replacing the per-SE soft bucket (remove-then-insert).
do
  local rk = %s
  if avi.vs.ratelimit.exceed(%q, rk) then
    avi.http.response(429,
      {["Content-Type"] = "application/json", ["Retry-After"] = "1"},
      '{"error":"rate_limit_exceeded","limit":"requests_per_second","budget":%d}')
    return
  end
end`, rlName, keyExpr, rlName, spec.RequestRateLimit.RequestsPerSecond)
}

// buildRequestRateLimiter builds the native Avi RateLimiter object for the request
// rate limit. Count/Period express the sustained rate (requests per second) and
// BurstSz the allowed instantaneous overshoot (defaults to the sustained count).
// The Name must match the one used in buildRequestRateLimitScript's
// avi.vs.ratelimit.exceed() call.
func buildRequestRateLimiter(spec AITokenRateLimitPolicySpec, name string) *avimodels.RateLimiter {
	rl := spec.RequestRateLimit
	burst := rl.Burst
	if burst <= 0 {
		burst = rl.RequestsPerSecond
	}
	return &avimodels.RateLimiter{
		Name:    proto.String(name),
		Count:   proto.Uint32(uint32(rl.RequestsPerSecond)),
		Period:  proto.Uint32(1),
		BurstSz: proto.Uint32(uint32(burst)),
	}
}

// addDataScriptNode adds or replaces an AviHTTPDataScriptNode in the VS's
// HTTPDSrefs list (idempotent: replaces by name on reconcile). rateLimiters, when
// non-nil, are the native Avi rate limiters the script references via
// avi.vs.ratelimit.exceed() — published on the resulting VSDataScriptSet.
func addDataScriptNode(key, vsName, tenant, dsName, evt, script string, rateLimiters []*avimodels.RateLimiter, vsNode nodes.AviVsEvhSniModel) {
	ds := &nodes.AviHTTPDataScriptNode{
		Name:         dsName,
		Tenant:       tenant,
		RateLimiters: rateLimiters,
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
