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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
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
// keyset inline in jwks_keys on every rebuild of the route. Rotated keys are
// picked up by the periodic JWKS refresh (jwks_refresh.go), which re-enqueues
// every route realizing a JWT-mode policy.

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

// jwtBackend is everything the JWT auth path needs from outside AKO: the IdP's
// JWKS endpoint and the Avi controller. It is the seam that lets the
// fail-closed / last-known-good decisions in realizeJWTAuth be unit-tested
// without either; production uses aviJWTBackend.
type jwtBackend interface {
	FetchJWKS(jwksURI string) (string, error)
	// LastKnownGood reports what Avi already holds for this policy from an
	// earlier realization: whether its JWTServerProfile exists with a keyset,
	// the issuer it was built for, and where and when that keyset was fetched.
	// It decides whether the SE may keep validating tokens against it while the
	// keyset cannot be refreshed.
	LastKnownGood(policy *AIGatewayAuthPolicy) (jwtLastKnownGood, error)
	UpsertServerProfile(key string, policy *AIGatewayAuthPolicy, jwks string) (string, error)
	// EnsureAuthProfile writes the AuthProfile; prov is the provenance of the
	// keyset in the server profile it references, recorded for LastKnownGood.
	EnsureAuthProfile(key string, policy *AIGatewayAuthPolicy, serverProfileName string, prov jwksProvenance) (string, error)
	EnsureSSOPolicy(key string, policy *AIGatewayAuthPolicy, authProfileName string) (string, error)
}

type aviJWTBackend struct{}

func (aviJWTBackend) FetchJWKS(jwksURI string) (string, error) { return fetchJWKS(jwksURI) }
func (aviJWTBackend) LastKnownGood(policy *AIGatewayAuthPolicy) (jwtLastKnownGood, error) {
	return jwtLastKnownGoodFromAvi(policy)
}
func (aviJWTBackend) UpsertServerProfile(key string, policy *AIGatewayAuthPolicy, jwks string) (string, error) {
	return upsertJWTServerProfile(key, policy, jwks)
}
func (aviJWTBackend) EnsureAuthProfile(key string, policy *AIGatewayAuthPolicy, serverProfileName string, prov jwksProvenance) (string, error) {
	return EnsureJWTAuthProfile(key, policy, serverProfileName, prov)
}
func (aviJWTBackend) EnsureSSOPolicy(key string, policy *AIGatewayAuthPolicy, authProfileName string) (string, error) {
	return EnsureJWTSSOPolicy(key, policy, authProfileName)
}

// jwtAvi is the backend applyJWTAuth uses. Tests swap it for a fake.
var jwtAvi jwtBackend = aviJWTBackend{}

// jwtRealization is the outcome of realizing a JWT auth policy in Avi.
type jwtRealization struct {
	SSOPolicyName string
	// StaleKeys is true when the JWKS could not be refreshed and the existing
	// JWTServerProfile (last-known-good keyset) was kept.
	StaleKeys bool
}

// realizeJWTAuth ensures the JWTServerProfile → AuthProfile → SSOPolicy chain.
// A non-nil error means there is nothing usable in Avi for this policy and the
// caller must fail closed.
func realizeJWTAuth(b jwtBackend, key string, policy *AIGatewayAuthPolicy) (jwtRealization, error) {
	serverProfileName, prov, stale, err := ensureJWTServerProfile(b, key, policy)
	if err != nil {
		return jwtRealization{}, fmt.Errorf("JWTServerProfile: %w", err)
	}
	authProfileName, err := b.EnsureAuthProfile(key, policy, serverProfileName, prov)
	if err != nil {
		return jwtRealization{}, fmt.Errorf("JWT AuthProfile: %w", err)
	}
	ssoPolicyName, err := b.EnsureSSOPolicy(key, policy, authProfileName)
	if err != nil {
		return jwtRealization{}, fmt.Errorf("JWT SSOPolicy: %w", err)
	}
	return jwtRealization{SSOPolicyName: ssoPolicyName, StaleKeys: stale}, nil
}

// ─── Last-known-good keyset ──────────────────────────────────────────────────

// JWKSMaxStale bounds how long a route may keep validating against a
// last-known-good keyset while the JWKS cannot be refreshed. Past it the route
// fails closed: keys the IdP has since revoked (a compromised key removed from
// the JWKS) must not stay trusted indefinitely just because AKO cannot see the
// removal.
const JWKSMaxStale = 24 * time.Hour

// jwtNow is the clock the staleness decision reads. Tests swap it.
var jwtNow = time.Now

// jwksProvenance records where and when the keyset in a JWTServerProfile was
// fetched. JWTServerProfile has no description or markers field, so it is kept
// in the description of the AuthProfile that references that server profile.
type jwksProvenance struct {
	// Source identifies the issuer + jwksUri the keyset was fetched for
	// (jwksSourceID).
	Source string
	// Keys identifies the keyset itself (jwksKeysID); empty in a provenance
	// written before it was recorded.
	Keys string
	// Refreshed is a time at which this keyset was fetched successfully. It may
	// lag the latest fetch by up to jwksProvenanceRewriteAfter: an unchanged
	// keyset keeps its recorded time so the AuthProfile is not rewritten on every
	// reconcile (see authProfileDescription). Lagging only shortens the
	// last-known-good window, never extends it.
	Refreshed time.Time
}

// jwksProvenanceRewriteAfter is how old a recorded refresh time may get before
// a successful fetch rewrites it even though nothing else changed. While the
// IdP is reachable the recorded time is at most this plus one refresh interval
// old, so a later outage still has most of JWKSMaxStale on the last-known-good
// keyset.
const jwksProvenanceRewriteAfter = JWKSMaxStale / 4

// jwksMaxClockSkew is how far in the future a recorded refresh time may be and
// still be believed. Anything later was not written by a sane clock (or was
// edited by hand), so it cannot vouch for the keyset's age.
const jwksMaxClockSkew = 5 * time.Minute

const jwksProvenancePrefix = "ako-ai-gateway jwks:"

// description renders the provenance for the AuthProfile description.
func (p jwksProvenance) description() string {
	keys := ""
	if p.Keys != "" {
		keys = " keys=" + p.Keys
	}
	return fmt.Sprintf("%s source=%s%s refreshed=%s", jwksProvenancePrefix, p.Source, keys, p.Refreshed.UTC().Format(time.RFC3339))
}

// authProfileDescription returns the AuthProfile description to write for
// prov, given the one Avi holds now (stored). The recorded provenance is
// rewritten only when it has to be: the keyset's source or content changed, the
// recorded time is older than jwksProvenanceRewriteAfter (or implausibly in the
// future), or what is stored is not a provenance at all. Otherwise stored is
// returned byte-for-byte, so an unchanged keyset does not turn every route
// event into an AuthProfile update on the controller.
func authProfileDescription(stored string, prov jwksProvenance, now time.Time) string {
	want := prov.description()
	if stored == want {
		return stored
	}
	old, ok := parseJWKSProvenance(stored)
	switch {
	case !ok,
		old.Source != prov.Source,
		old.Keys != prov.Keys,
		old.Refreshed.After(prov.Refreshed), // never record a newer time than known
		old.Refreshed.After(now.Add(jwksMaxClockSkew)),
		now.Sub(old.Refreshed) >= jwksProvenanceRewriteAfter:
		return want
	}
	return stored
}

// parseJWKSProvenance reads a provenance written by description. ok is false
// for anything else (no description, one written before provenance tracking,
// or one edited outside AKO).
func parseJWKSProvenance(desc string) (p jwksProvenance, ok bool) {
	rest, found := strings.CutPrefix(desc, jwksProvenancePrefix)
	if !found {
		return jwksProvenance{}, false
	}
	for _, field := range strings.Fields(rest) {
		k, v, _ := strings.Cut(field, "=")
		switch k {
		case "source":
			p.Source = v
		case "keys":
			p.Keys = v
		case "refreshed":
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return jwksProvenance{}, false
			}
			p.Refreshed = t
		}
	}
	if p.Source == "" || p.Refreshed.IsZero() {
		return jwksProvenance{}, false
	}
	return p, true
}

// jwksSourceID identifies the keyset source a policy names: its issuer and
// jwksUri. A change to either is a change of trust anchor, so a keyset fetched
// under the old pair must never stand in for the new one.
func jwksSourceID(policy *AIGatewayAuthPolicy) string {
	sum := sha256.Sum256([]byte(policy.Spec.JWT.Issuer + "\n" + policy.Spec.JWT.JwksUri))
	return hex.EncodeToString(sum[:16])
}

// jwksKeysID identifies a fetched JWKS document, so a rotated keyset is told
// apart from an unchanged one.
func jwksKeysID(jwks string) string {
	sum := sha256.Sum256([]byte(jwks))
	return hex.EncodeToString(sum[:16])
}

// jwtLastKnownGood is what Avi holds for a policy from an earlier realization.
type jwtLastKnownGood struct {
	// HasKeys: the policy's JWTServerProfile exists and holds a keyset.
	HasKeys bool
	// Issuer is the issuer that JWTServerProfile validates.
	Issuer string
	// Provenance of its keyset, from the AuthProfile; zero when unrecorded.
	Provenance jwksProvenance
}

// unusableReason says why the last-known-good keyset must not stand in for the
// policy's current one, or "" when it may. It may only when it was fetched for
// the same issuer and jwksUri the policy names now, recently enough.
func (l jwtLastKnownGood) unusableReason(policy *AIGatewayAuthPolicy, now time.Time) string {
	switch {
	case !l.HasKeys:
		return "no existing JWTServerProfile with a keyset"
	case l.Issuer != policy.Spec.JWT.Issuer:
		return fmt.Sprintf("it validates issuer %q but the policy now names %q", l.Issuer, policy.Spec.JWT.Issuer)
	case l.Provenance.Source == "":
		return "its keyset has no recorded source, so it cannot be tied to the policy's jwksUri"
	case l.Provenance.Source != jwksSourceID(policy):
		return "its keyset was fetched for a different issuer/jwksUri than the policy now names"
	case l.Provenance.Refreshed.After(now.Add(jwksMaxClockSkew)):
		return fmt.Sprintf("its recorded refresh time %s is in the future, so its age is unknown",
			l.Provenance.Refreshed.UTC().Format(time.RFC3339))
	case now.Sub(l.Provenance.Refreshed) > JWKSMaxStale:
		return fmt.Sprintf("its keyset was last refreshed at %s, more than %s ago",
			l.Provenance.Refreshed.UTC().Format(time.RFC3339), JWKSMaxStale)
	}
	return ""
}

// ensureJWTServerProfile creates/updates the CLIENT_AUTH JWTServerProfile that
// holds the issuer and the fetched JWKS keyset the SE validates tokens against,
// and returns the provenance to record for that keyset.
//
// If the JWKS cannot be fetched, the existing (last-known-good) keyset is kept
// and its name returned with stale=true, but only when it was fetched for the
// issuer and jwksUri the policy names now and refreshed within JWKSMaxStale: a
// transient IdP outage must not take down traffic whose keys are still valid,
// and it must not keep an IdP the policy no longer trusts either. Otherwise the
// error is returned and the caller fails closed.
func ensureJWTServerProfile(b jwtBackend, key string, policy *AIGatewayAuthPolicy) (name string, prov jwksProvenance, stale bool, err error) {
	name = jwtServerProfileName(policy)
	jwks, fetchErr := b.FetchJWKS(policy.Spec.JWT.JwksUri)
	if fetchErr == nil {
		name, err = b.UpsertServerProfile(key, policy, jwks)
		return name, jwksProvenance{Source: jwksSourceID(policy), Keys: jwksKeysID(jwks), Refreshed: jwtNow().UTC()}, false, err
	}
	lkg, lookupErr := b.LastKnownGood(policy)
	if lookupErr != nil {
		return "", jwksProvenance{}, false, fmt.Errorf("%w (and looking up existing JWTServerProfile %s failed: %v)", fetchErr, name, lookupErr)
	}
	if reason := lkg.unusableReason(policy, jwtNow()); reason != "" {
		return "", jwksProvenance{}, false, fmt.Errorf("%w (cannot fall back on JWTServerProfile %s: %s)", fetchErr, name, reason)
	}
	utils.AviLog.Warnf("key: %s, msg: AIGatewayAuthPolicy %s/%s: JWKS refresh failed, keeping last-known-good JWTServerProfile %s (refreshed %s): %v",
		key, policy.Namespace, policy.Name, name, lkg.Provenance.Refreshed.UTC().Format(time.RFC3339), fetchErr)
	return name, lkg.Provenance, true, nil
}

// jwtLastKnownGoodFromAvi reads the policy's JWTServerProfile and AuthProfile
// from the controller. A profile whose jwks_keys comes back explicitly empty has
// no keyset; one whose jwks_keys is omitted from the response (a controller that
// treats it as sensitive) has, since AKO never writes a profile without keys.
func jwtLastKnownGoodFromAvi(policy *AIGatewayAuthPolicy) (jwtLastKnownGood, error) {
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]
	var srv struct {
		Results []struct {
			Issuer   *string `json:"issuer"`
			JwksKeys *string `json:"jwks_keys"`
		} `json:"results"`
	}
	if err := lib.AviGet(client, "/api/jwtserverprofile?name="+jwtServerProfileName(policy), &srv); err != nil {
		return jwtLastKnownGood{}, err
	}
	var lkg jwtLastKnownGood
	for _, r := range srv.Results {
		if r.JwksKeys == nil || *r.JwksKeys != "" {
			lkg.HasKeys = true
			if r.Issuer != nil {
				lkg.Issuer = *r.Issuer
			}
			break
		}
	}
	if !lkg.HasKeys {
		return lkg, nil
	}
	var auth struct {
		Results []struct {
			Description *string `json:"description"`
		} `json:"results"`
	}
	if err := lib.AviGet(client, "/api/authprofile?name="+jwtAuthProfileName(policy), &auth); err != nil {
		return jwtLastKnownGood{}, err
	}
	for _, r := range auth.Results {
		if r.Description == nil {
			continue
		}
		if p, ok := parseJWKSProvenance(*r.Description); ok {
			lkg.Provenance = p
			break
		}
	}
	return lkg, nil
}

// upsertJWTServerProfile writes the JWTServerProfile with the given keyset.
func upsertJWTServerProfile(key string, policy *AIGatewayAuthPolicy, jwks string) (string, error) {
	name := jwtServerProfileName(policy)
	tenant := lib.GetTenantInNamespace(policy.Namespace)
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
// JWTServerProfile. Its description carries prov, the provenance of that
// server profile's keyset (see jwksProvenance); an existing description that
// still holds for prov is kept as it is (see authProfileDescription).
func EnsureJWTAuthProfile(key string, policy *AIGatewayAuthPolicy, serverProfileName string, prov jwksProvenance) (string, error) {
	name := jwtAuthProfileName(policy)
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	var check struct {
		Count   int `json:"count"`
		Results []struct {
			UUID        string  `json:"uuid"`
			Description *string `json:"description"`
		} `json:"results"`
	}
	_ = lib.AviGet(client, "/api/authprofile?name="+name, &check)
	stored := ""
	if check.Count > 0 && len(check.Results) > 0 && check.Results[0].Description != nil {
		stored = *check.Results[0].Description
	}

	profile := avimodels.AuthProfile{
		Name:          proto.String(name),
		TenantRef:     proto.String("/api/tenant/?name=" + lib.GetEscapedValue(tenant)),
		Type:          proto.String("AUTH_PROFILE_JWT"),
		JwtProfileRef: proto.String("/api/jwtserverprofile/?name=" + serverProfileName),
		Description:   proto.String(authProfileDescription(stored, prov, jwtNow())),
	}

	if check.Count > 0 && len(check.Results) > 0 {
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

// Avi authentication actions (AuthenticationAction.Type). The SDK documents the
// enum as: SKIP_AUTHENTICATION, USE_DEFAULT_AUTHENTICATION.
const (
	authnActionSkip    = "SKIP_AUTHENTICATION"
	authnActionDefault = "USE_DEFAULT_AUTHENTICATION"
)

// adminAuthnRules builds the authn rules for the admin path, shared by the JWT
// and OAuth SSO policies.
//
// It ALWAYS returns exactly one rule. "Do not exempt this path" is expressed by
// flipping the rule's action to USE_DEFAULT_AUTHENTICATION, never by emitting an
// empty rule list: an SSO policy with no authn rules is accepted by the
// controller on its own, but the VS that references it then fails to PUT, and
// AKO abandons the whole EVH child chain on that failure ("Failure in processing
// EVH node ... Not processing other child nodes"). The practical effect is that
// every route on the gateway stops being programmed and the VIP answers nothing
// — observed live 2026-08-16 on the LLM front door, recovered by reverting the
// annotation.
func adminAuthnRules(policy *AIGatewayAuthPolicy) []*avimodels.AuthenticationRule {
	paths, skip := policy.EffectiveAdminSkipPaths()
	action := authnActionSkip
	if !skip {
		action = authnActionDefault
	}
	// The rule name is stable across both actions so the policy carries one rule
	// that changes meaning, rather than accumulating orphans.
	return []*avimodels.AuthenticationRule{{
		Name:   proto.String("ai-admin-skip"),
		Index:  proto.Int32(1),
		Enable: proto.Bool(true),
		Action: &avimodels.AuthenticationAction{Type: proto.String(action)},
		Match: &avimodels.AuthenticationMatch{
			Path: &avimodels.PathMatch{
				MatchCriteria: proto.String("BEGINS_WITH"),
				MatchStr:      paths,
			},
		},
	}}
}

// EnsureJWTSSOPolicy creates/updates the SSO_TYPE_JWT policy the VS references.
// The same admin-skip authn rule as the OAuth path keeps the read-only counters
// endpoint (GET /v1/admin/...) reachable without a token.
func EnsureJWTSSOPolicy(key string, policy *AIGatewayAuthPolicy, authProfileName string) (string, error) {
	name := jwtSSOPolicyName(policy)
	tenant := lib.GetTenantInNamespace(policy.Namespace)
	client := avicache.SharedAVIClients(tenant).AviClient[0]

	authnRules := adminAuthnRules(policy)

	sso := avimodels.SSOPolicy{
		Name:      proto.String(name),
		TenantRef: proto.String("/api/tenant/?name=" + lib.GetEscapedValue(tenant)),
		Type:      proto.String("SSO_TYPE_JWT"),
		AuthenticationPolicy: &avimodels.AuthenticationPolicy{
			DefaultAuthProfileRef: proto.String("/api/authprofile/?name=" + authProfileName),
			AuthnRules:            authnRules,
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
	// Order matters: SSO + AuthProfile reference the server profile.
	delByName("/api/ssopolicy", jwtSSOPolicyName(policy))
	delByName("/api/authprofile", jwtAuthProfileName(policy))
	delByName("/api/jwtserverprofile", jwtServerProfileName(policy))
}
