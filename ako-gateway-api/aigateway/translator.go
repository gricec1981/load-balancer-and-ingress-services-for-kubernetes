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
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

// ApplyAuthPolicy configures the Avi VS node model to enforce JWT authentication
// as described in the AIGatewayAuthPolicy spec.
//
// Avi object mapping (Phase 1):
//   - JWT validation:  references a pre-existing Avi SSO Policy by name.
//     The SSO Policy encapsulates the JWTServerProfile (issuer, JWKS) and the
//     HTTP security action that validates the bearer token.  The SSO policy name
//     is derived from the policy namespace/name: "<ns>-<name>-sso".
//     Full AKO-managed JWTServerProfile/SSO-Policy lifecycle is Phase 1.5.
//   - Identity header: HTTP request policy rules copy the claim headers that
//     the Avi SSO policy inserts (X-AVI-JWT-<CLAIM>) into the user-configured
//     identityHeader and any forwardClaims headers.
func ApplyAuthPolicy(key string, policy *AIGatewayAuthPolicy, vsNode nodes.AviVsEvhSniModel) {
	if policy == nil {
		return
	}
	spec := policy.Spec

	// ── 1. Reference the Avi SSO Policy for JWT signature validation ─────────
	// The SSO Policy must be pre-created in Avi with the correct JWTServerProfile
	// (issuer + JWKS).  Full lifecycle management will be added in Phase 1.5.
	ssoPolicyName := derivedSSOPolicyName(policy)
	ssoPolicyRef := fmt.Sprintf("/api/ssopolicy?name=%s", ssoPolicyName)
	vsNode.GetGeneratedFields().SsoPolicyRef = proto.String(ssoPolicyRef)
	utils.AviLog.Infof("key: %s, msg: AIGatewayAuthPolicy %s/%s: set SsoPolicyRef → %s",
		key, policy.Namespace, policy.Name, ssoPolicyName)

	// ── 2. Identity-header + forward-claim injection via HTTP request policy ──
	// The Avi SSO policy copies validated JWT claims into headers of the form
	// "X-AVI-JWT-<UPPER_CLAIM>".  We copy those into the user-configured header
	// names so downstream policies have stable names to key off.
	identityClaim := spec.JWT.EffectiveIdentityClaim()
	identityHdr := spec.EffectiveIdentityHeader()
	claimSrcHeader := aviJWTClaimHeader(identityClaim)

	rules := buildClaimForwardRules(claimSrcHeader, identityHdr, spec.JWT.ForwardClaims, 100)
	if len(rules) == 0 {
		return
	}

	// Find or create a dedicated AI-auth HTTP policy set on this VS.
	policySetName := fmt.Sprintf("%s-ai-auth", vsNode.GetName())
	policyNode := findOrCreateHTTPPolicySet(policySetName, vsNode)
	// Prepend rules so they run before any existing request rules.
	policyNode.RequestRules = append(rules, policyNode.RequestRules...)
	utils.AviLog.Infof("key: %s, msg: AIGatewayAuthPolicy %s/%s: added %d claim-forward rule(s) to %s",
		key, policy.Namespace, policy.Name, len(rules), policySetName)
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

// buildHeaderCopyRule constructs an HTTPRequestRule that copies srcHeader → dstHeader
// using the HTTP_POLICY_VAR_HTTP_HDR variable.
func buildHeaderCopyRule(ruleName string, index int32, srcHeader, dstHeader string) *avimodels.HTTPRequestRule {
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
						Val: proto.String(srcHeader),
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
		keyExpr = fmt.Sprintf(`(avi.http.get_header(%q) or avi.vs.client_ip())`, hdr)
	default:
		keyExpr = "avi.vs.client_ip()"
	}

	return fmt.Sprintf(`-- AKO AI Gateway: per-consumer RPS soft rate limiter
-- Phase 1.5 upgrade: replace with native avi.vs.rate_limiter() for cross-SE consistency.
do
  local rk = "rps:"..%s
  local win_key = rk..":"..math.floor(os.time())
  local cur = tonumber(avi.vs.table_lookup("ai_rps", win_key) or 0)
  if cur >= %d then
    avi.http.response(429,
      {["Content-Type"] = "application/json", ["Retry-After"] = "1"},
      '{"error":"rate_limit_exceeded","limit":"requests_per_second","budget":%d}')
    return
  end
  avi.vs.table_insert("ai_rps", win_key, tostring(cur + 1), 2)
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
