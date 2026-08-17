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

// Package aigateway implements Phase 1 of the Avi AI Gateway: JWT authentication
// (AIGatewayAuthPolicy) and token-based rate limiting (AITokenRateLimitPolicy).
// Both policies are expressed as Kubernetes CRDs that attach to HTTPRoutes via a
// targetRef and are reconciled into existing Avi Service Engine capabilities by AKO.
//
// Design reference: docs/ai-gateway/phase1-design.md
package aigateway

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PolicyGroup is the API group for AI Gateway policy CRDs.
const PolicyGroup = "ai.ako.vmware.com"

// PolicyVersion is the API version for AI Gateway policy CRDs.
const PolicyVersion = "v1alpha1"

// ─── AIGatewayAuthPolicy ─────────────────────────────────────────────────────

// AIGatewayAuthPolicy authenticates API consumers via JWT and resolves a stable
// consumer identity that downstream policies key off. Phase 1 supports JWT only.
type AIGatewayAuthPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIGatewayAuthPolicySpec   `json:"spec"`
	Status AIGatewayAuthPolicyStatus `json:"status,omitempty"`

	// AdminSkipPath is read from AdminSkipPathAnnotation and controls the
	// SKIP_AUTHENTICATION authn rule that keeps the read-only counters endpoint
	// reachable by a client that cannot complete the IdP flow. Empty (the
	// default) preserves the historical "/v1/admin/" prefix; a narrower prefix
	// such as "/v1/admin/counters" shrinks what bypasses authentication; the
	// literal "none" makes the SE validate the JWT before the DataScript runs
	// (requires the counters endpoint to accept a claim — see
	// AdminClaimAnnotation — and the caller to present a token).
	//
	// "none" flips the rule's action rather than removing it; see adminAuthnRules
	// for why an empty rule list takes the whole gateway down.
	AdminSkipPath string `json:"-"`
}

// AdminSkipPathAnnotation, when set on an AIGatewayAuthPolicy, overrides the
// path prefix exempted from authentication for the read-only admin endpoints.
// Absent = "/v1/admin/" (historical behaviour). "none" = emit no skip rule.
//
// Rollback is an annotation edit: removing it restores the previous rule on the
// next reconcile, with no image change.
const AdminSkipPathAnnotation = "ai.ako.vmware.com/admin-skip-path"

// DefaultAdminSkipPath is the historical exemption prefix. It is deliberately
// broader than the single endpoint the DataScript implements, which is why it
// is worth narrowing: any other path under it reaches the enforcement block
// with no validated JWT (today it is stopped only by the unknown-group
// fail-closed, i.e. by a limit whose Budget is 0).
const DefaultAdminSkipPath = "/v1/admin/"

// EffectiveAdminSkipPath returns the admin path prefix the authn rule matches,
// and whether that rule should SKIP authentication for it. A false second value
// means "match the same path, but authenticate it normally" — the rule is still
// emitted (see adminAuthnRules).
func (p *AIGatewayAuthPolicy) EffectiveAdminSkipPath() (string, bool) {
	switch p.AdminSkipPath {
	case "":
		return DefaultAdminSkipPath, true
	case "none":
		return DefaultAdminSkipPath, false
	default:
		return p.AdminSkipPath, true
	}
}

// AuthClaimMode selects how the Avi SE validates the caller's JWT and how the
// AKO DataScripts read its (validated) claims. The two modes are mutually
// exclusive on the current Avi build — see docs/gateway-api/ai-gateway-auth.md.
//
// The zero value is ClaimModeOAuth, so any path that does not explicitly thread a
// mode falls back to the OAuth flow and never decode-and-trusts a query token.
type AuthClaimMode int

const (
	// ClaimModeOAuth is the OAuth/OIDC resource-server flow: the SE runs the
	// auth-code/session-cookie flow and exposes validated claims to DataScripts
	// via avi.http.oauth_get_claim(). Browser clients only — a bearer token in
	// the Authorization header is ignored (302 to /authorize).
	ClaimModeOAuth AuthClaimMode = iota

	// ClaimModeJWTQuery is the stateless bearer-style flow: the SE validates a
	// JWT presented as a query parameter (jwt_location=JWT_LOCATION_QUERY_PARAM),
	// returning 200/401 with no redirect. The query param is NOT stripped before
	// DataScripts run, so the AKO claim helper base64url-decodes the SE-validated
	// token to read claims. Works for machine clients (SDKs/agents) that append
	// the token to the request URL. Cost: token-in-URL (mitigate with TLS,
	// short-lived tokens, SE query-log redaction, and strip-before-backend).
	ClaimModeJWTQuery
)

// AIGatewayAuthPolicySpec is the desired state of an AIGatewayAuthPolicy.
type AIGatewayAuthPolicySpec struct {
	// TargetRef identifies the HTTPRoute or Gateway this policy applies to.
	TargetRef PolicyTargetRef `json:"targetRef"`

	// AuthMode selects the SE validation + claim-access model: "oauthBrowser"
	// (default — OAuth session, browser clients) or "jwtQuery" (stateless bearer
	// JWT in a query param, machine clients). See AuthClaimMode.
	// +optional
	AuthMode string `json:"authMode,omitempty"`

	// JWT configures JWT validation for the targeted route.
	JWT JWTConfig `json:"jwt"`

	// IdentityHeader is the request header that receives the resolved consumer
	// identity after successful JWT validation. Defaults to "x-ai-consumer".
	IdentityHeader string `json:"identityHeader,omitempty"`

	// OnFailure controls the response when JWT validation fails.
	OnFailure *AuthFailureAction `json:"onFailure,omitempty"`
}

// JWTConfig carries the JWT validation parameters.
type JWTConfig struct {
	// Issuer is the expected `iss` claim value.
	Issuer string `json:"issuer"`

	// JwksUri is the URL of the JWKS endpoint. AKO fetches and caches
	// the public key set for offline signature verification.
	// +optional
	JwksUri string `json:"jwksUri,omitempty"`

	// Audiences lists acceptable `aud` claim values. At least one must match.
	// If empty, audience validation is skipped.
	// +optional
	Audiences []string `json:"audiences,omitempty"`

	// IdentityClaim is the JWT claim whose value becomes the consumer identity
	// written into IdentityHeader. Defaults to "sub".
	IdentityClaim string `json:"identityClaim,omitempty"`

	// ForwardClaims is the list of additional JWT claims to inject as
	// request headers (header name = lower-cased claim name).
	// +optional
	ForwardClaims []string `json:"forwardClaims,omitempty"`
}

// AuthFailureAction controls the rejection response when JWT validation fails.
type AuthFailureAction struct {
	// StatusCode is the HTTP status to return. Defaults to 401.
	StatusCode int `json:"statusCode,omitempty"`
}

// AIGatewayAuthPolicyStatus is the observed state of an AIGatewayAuthPolicy.
type AIGatewayAuthPolicyStatus struct {
	// Conditions holds standard condition types.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AuthModeJWTQuery is the spec.authMode string that selects ClaimModeJWTQuery.
const AuthModeJWTQuery = "jwtQuery"

// EffectiveAuthMode maps the spec.authMode string to an AuthClaimMode. Anything
// other than "jwtQuery" (including empty) is the default OAuth browser flow.
func (s AIGatewayAuthPolicySpec) EffectiveAuthMode() AuthClaimMode {
	if s.AuthMode == AuthModeJWTQuery {
		return ClaimModeJWTQuery
	}
	return ClaimModeOAuth
}

// ─── AITokenRateLimitPolicy ──────────────────────────────────────────────────

// AITokenRateLimitPolicy enforces token-based budgets and classic request-rate
// limits on an HTTPRoute. Token accounting is reactive (post-response) and
// eventually-consistent across scaled-out Service Engines (see design doc §5).
type AITokenRateLimitPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AITokenRateLimitPolicySpec   `json:"spec"`
	Status AITokenRateLimitPolicyStatus `json:"status,omitempty"`

	// CounterEpoch is read from the CounterEpochAnnotation. It is folded into the
	// SE counter key, so changing it (e.g. `kubectl annotate ...
	// ai.ako.vmware.com/counter-epoch=2`) moves every limit to a fresh keyspace —
	// effectively resetting the running token counters without touching the
	// budgets or any other config. Empty means no prefix (back-compatible).
	CounterEpoch string `json:"-"`

	// AdminToken is the resolved value of the Secret named by
	// AdminTokenSecretAnnotation (key "token"). When non-empty, AKO emits a
	// read-only GET /v1/admin/counters endpoint in the request DataScript that the
	// dashboard UI polls for per-user token usage, gated by this token in the
	// X-Admin-Token header. Empty means the endpoint is not generated.
	AdminToken string `json:"-"`

	// AdminClaimName/AdminClaimValue are read from AdminClaimAnnotation. When
	// set, the counters endpoint ALSO accepts a caller whose SE-validated JWT
	// carries that claim value — so the dashboard can authenticate as itself
	// (short-lived, revocable, attributable) instead of presenting a shared
	// secret that is baked verbatim into the generated DataScript.
	//
	// This is deliberately additive: the X-Admin-Token header keeps working
	// while the annotation is set, so callers migrate one at a time and
	// removing the annotation reverts to token-only with no image change.
	AdminClaimName  string `json:"-"`
	AdminClaimValue string `json:"-"`
}

// AdminClaimAnnotation, when set on an AITokenRateLimitPolicy as
// "<claim>=<value>" (e.g. "scope=counters:read"), lets the read-only counters
// endpoint accept a validated JWT carrying that claim, in addition to the
// X-Admin-Token header. Absent = token only (historical behaviour).
const AdminClaimAnnotation = "ai.ako.vmware.com/admin-claim"

// CounterEpochAnnotation, when set on an AITokenRateLimitPolicy, prefixes the SE
// token-counter keys. Bumping it clears all of that policy's running counters.
const CounterEpochAnnotation = "ai.ako.vmware.com/counter-epoch"

// AdminTokenSecretAnnotation names a Secret (in the policy's namespace, key
// "token") whose value gates the read-only GET /v1/admin/counters endpoint used
// by the dashboard UI. Absent annotation = no counters endpoint is generated.
const AdminTokenSecretAnnotation = "ai.ako.vmware.com/admin-token-secret"

// AITokenRateLimitPolicySpec is the desired state of an AITokenRateLimitPolicy.
type AITokenRateLimitPolicySpec struct {
	// TargetRef identifies the HTTPRoute or Gateway this policy applies to.
	TargetRef PolicyTargetRef `json:"targetRef"`

	// IdentitySource controls how the consumer identity is resolved.
	IdentitySource *IdentitySource `json:"identitySource,omitempty"`

	// Limits defines token-budget limits. Each entry generates a counter keyed
	// in SE shared state. ALL limits are checked per request; the request is
	// rejected if any limit is already at or over budget.
	Limits []TokenLimit `json:"limits,omitempty"`

	// RequestRateLimit applies a classic RPS rate limiter using Avi's native
	// distributed rate limiter. This is exact and consistent across all SEs.
	RequestRateLimit *RequestRateLimit `json:"requestRateLimit,omitempty"`
}

// IdentitySource controls how the consumer identity is resolved for rate limiting.
type IdentitySource struct {
	// Header is the request header carrying the consumer identity.
	// Defaults to "x-ai-consumer" (set by AIGatewayAuthPolicy).
	Header string `json:"header,omitempty"`

	// Fallback controls the identity used when the header is absent.
	// "clientIP" uses the source IP; "reject" returns 401.
	Fallback string `json:"fallback,omitempty"`
}

// TokenLimit defines a single token-budget limit.
type TokenLimit struct {
	// Name is a unique identifier for this limit (used as counter-key prefix).
	Name string `json:"name"`

	// Key is the dimension to key the counter on:
	//   "consumer"         – resolved consumer identity (from IdentitySource)
	//   "header:<name>"    – value of a specific request header
	//   "clientIP"         – source IP address
	Key string `json:"key"`

	// Tokens selects which token dimension counts against this budget:
	//   "prompt"     – prompt tokens only
	//   "completion" – completion tokens only
	//   "total"      – prompt + completion (default)
	Tokens string `json:"tokens,omitempty"`

	// Budget is the maximum token count allowed within the window.
	// When GroupBudgets is set this acts as the fallback for unrecognised groups
	// (0 = reject requests from unrecognised groups).
	Budget int64 `json:"budget"`

	// Window is the time window for the budget (e.g. "1m", "1h", "24h").
	// Fixed-window counters reset at the boundary.
	Window string `json:"window"`

	// GroupHeader, when set, enables per-group budget enforcement.
	// The header value (forwarded from a JWT claim by AIGatewayAuthPolicy) is
	// looked up in GroupBudgets to determine the per-user budget ceiling.
	// The counter is still keyed on the consumer identity (Key field), so each
	// user gets their own counter; the *budget* they are checked against depends
	// on which group they belong to.
	// +optional
	GroupHeader string `json:"groupHeader,omitempty"`

	// GroupBudgets maps group header values to their token budgets.
	// Only used when GroupHeader is set.
	// Example: {"group1": 500, "group2": 1000}
	// +optional
	GroupBudgets map[string]int64 `json:"groupBudgets,omitempty"`

	// Action controls what happens when the budget is exceeded.
	Action *LimitAction `json:"action,omitempty"`
}

// LimitAction controls the response when a token limit is exceeded.
type LimitAction struct {
	// Type is "Reject" (default) or "Log" (count but allow).
	Type string `json:"type,omitempty"`

	// StatusCode is the HTTP status code returned on rejection. Defaults to 429.
	StatusCode int `json:"statusCode,omitempty"`

	// RetryAfter, if true, adds a Retry-After header pointing to the window boundary.
	RetryAfter bool `json:"retryAfter,omitempty"`
}

// RequestRateLimit applies a classic requests-per-second rate limiter.
type RequestRateLimit struct {
	// RequestsPerSecond is the sustained request rate limit.
	RequestsPerSecond int `json:"requestsPerSecond"`

	// Burst is the maximum number of requests above the sustained rate in one
	// burst. Defaults to RequestsPerSecond if unset.
	Burst int `json:"burst,omitempty"`

	// Key is the dimension for the rate limiter: "consumer" or "clientIP".
	Key string `json:"key,omitempty"`
}

// AITokenRateLimitPolicyStatus is the observed state.
type AITokenRateLimitPolicyStatus struct {
	// Conditions holds standard condition types.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ─── Shared types ────────────────────────────────────────────────────────────

// PolicyTargetRef identifies the Kubernetes resource a policy applies to.
// Follows the Gateway API policy-attachment convention.
type PolicyTargetRef struct {
	// Group is the API group of the referent (e.g. "gateway.networking.k8s.io").
	Group string `json:"group"`

	// Kind is the Kind of the referent ("HTTPRoute" or "Gateway").
	Kind string `json:"kind"`

	// Name is the name of the referent in the same namespace as the policy.
	Name string `json:"name"`
}

// IdentityHeader returns the configured identity header, falling back to the
// default "x-ai-consumer" if the spec field is empty.
func (s *AIGatewayAuthPolicySpec) EffectiveIdentityHeader() string {
	if s.IdentityHeader != "" {
		return s.IdentityHeader
	}
	return "x-ai-consumer"
}

// EffectiveIdentityClaim returns the configured identity claim, defaulting to "sub".
func (j *JWTConfig) EffectiveIdentityClaim() string {
	if j.IdentityClaim != "" {
		return j.IdentityClaim
	}
	return "sub"
}

// EffectiveFailureStatus returns the configured HTTP status on auth failure, defaulting to 401.
func (s *AIGatewayAuthPolicySpec) EffectiveFailureStatus() int {
	if s.OnFailure != nil && s.OnFailure.StatusCode >= 400 {
		return s.OnFailure.StatusCode
	}
	return 401
}

// IdentityHeader returns the header the policy reads for consumer identity,
// defaulting to "x-ai-consumer".
func (s *AITokenRateLimitPolicySpec) EffectiveIdentityHeader() string {
	if s.IdentitySource != nil && s.IdentitySource.Header != "" {
		return s.IdentitySource.Header
	}
	return "x-ai-consumer"
}
