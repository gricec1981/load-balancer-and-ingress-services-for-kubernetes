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
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─── AIModelRoutePolicy ──────────────────────────────────────────────────────

// AIModelRoutePolicy routes inference requests to different backends based on the
// requested model, organised into quality/cost tiers. The model is read from the
// request body by an SE DataScript (HTTP_REQ buffering + HTTP_REQ_DATA read),
// mapped to a tier, optionally gated by the verified group claim from an
// AIGatewayAuthPolicy, and the request is routed to that tier's Avi Pool Group
// via avi.poolgroup.select(). See docs/gateway-api/model-routing.md.
type AIModelRoutePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIModelRoutePolicySpec   `json:"spec"`
	Status AIModelRoutePolicyStatus `json:"status,omitempty"`
}

// AIModelRoutePolicySpec is the desired state of an AIModelRoutePolicy.
type AIModelRoutePolicySpec struct {
	// TargetRef identifies the HTTPRoute or Gateway this policy applies to.
	TargetRef PolicyTargetRef `json:"targetRef"`

	// ModelField is the JSON body key to route on. Defaults to "model".
	ModelField string `json:"modelField,omitempty"`

	// Tiers lists the routing destinations, most-preferred first. Order encodes
	// preference and drives downgrade selection.
	Tiers []ModelTier `json:"tiers"`

	// ModelTiers maps a requested model name to a tier name. Keys may be an exact
	// model name or a single trailing "*" prefix glob (e.g. "mistral-7b*").
	ModelTiers map[string]string `json:"modelTiers"`

	// DefaultTier is the tier used for a requested model not matched by ModelTiers.
	DefaultTier string `json:"defaultTier"`

	// Entitlements, when set, restricts which auth groups may reach which tiers.
	// +optional
	Entitlements *ModelEntitlements `json:"entitlements,omitempty"`

	// OnUnentitled controls behaviour when a caller requests a tier they are not
	// entitled to (or an unknown model maps to a disallowed default).
	// +optional
	OnUnentitled *UnentitledAction `json:"onUnentitled,omitempty"`

	// Discovery, when enabled, extends ModelTiers with aliases discovered from
	// running pods: any pod (in an allowed namespace) carrying the alias
	// annotation and a tier label naming a declared tier is merged into the
	// model→tier table at translation time. Static ModelTiers entries always win
	// over discovered ones. Deploying a labeled model server and publishing it
	// through the gateway thereby become the same action; deleting the pods
	// un-registers the alias (requests fall back to DefaultTier).
	// +optional
	Discovery *ModelDiscovery `json:"discovery,omitempty"`
}

// ModelDiscovery configures pod-label based model alias discovery. It is
// deliberately engine-agnostic: any workload that produces pods with the
// annotation/label pair participates (KServe predictors, plain Deployments,
// anything) — and with no such pods present it discovers nothing.
type ModelDiscovery struct {
	// Enabled turns discovery on. Off (or Discovery absent) leaves behaviour
	// byte-identical to a static-only policy.
	Enabled bool `json:"enabled"`

	// AliasAnnotation names the pod annotation carrying the model alias to
	// publish. Defaults to "ai.ako.vmware.com/model-alias".
	// +optional
	AliasAnnotation string `json:"aliasAnnotation,omitempty"`

	// TierLabel names the pod label carrying the tier. Its value must name a
	// declared tier or the pod is ignored (with a warning). Defaults to
	// "ai.ako.vmware.com/tier".
	// +optional
	TierLabel string `json:"tierLabel,omitempty"`

	// Namespaces scopes discovery: only pods in these namespaces may register
	// aliases (the governance gate). Defaults to the policy's own namespace.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
}

// ModelTier is one routing destination.
type ModelTier struct {
	// Name is the tier identifier; becomes part of the Pool Group name and the
	// ai_tier reqvar value.
	Name string `json:"name"`

	// BackendRef is the backend that serves this tier (an InferencePool to keep
	// per-pod metric weighting, or a core Service). Omitted when Provider is set.
	// +optional
	BackendRef ModelBackendRef `json:"backendRef,omitempty"`

	// Provider routes this tier to an external OpenAI-compatible provider (e.g.
	// Gemini) over the Service Engine's egress. AKO authors an FQDN pool with
	// backend TLS/SNI, and the model-route DataScript rewrites the path + Host and
	// injects the provider's API key (read from a Secret) before forwarding. The
	// SE never proxies for it — it forwards the OpenAI-shaped request unchanged.
	// +optional
	Provider *ModelProvider `json:"provider,omitempty"`
}

// ModelProvider describes an external OpenAI-compatible provider endpoint.
type ModelProvider struct {
	// Host is the provider's FQDN (e.g. generativelanguage.googleapis.com). AKO
	// builds an FQDN pool whose server the SE resolves by DNS and reaches over TLS.
	Host string `json:"host"`
	// Port defaults to 443.
	// +optional
	Port int32 `json:"port,omitempty"`
	// TLS enables backend TLS + SNI to Host. Defaults to true.
	// +optional
	TLS *bool `json:"tls,omitempty"`
	// Path is the provider's chat-completions path the request is rewritten to
	// (e.g. /v1beta/openai/chat/completions for Gemini).
	Path string `json:"path"`
	// Auth injects the provider API key as a request header.
	// +optional
	Auth *ProviderAuth `json:"auth,omitempty"`
}

// ProviderAuth injects a provider credential (from a Secret) as a request header.
type ProviderAuth struct {
	// SecretRef names the Secret (in the policy namespace) holding the API key.
	SecretRef ProviderSecretRef `json:"secretRef"`
	// Header is the header to set. Defaults to "Authorization".
	// +optional
	Header string `json:"header,omitempty"`
	// Scheme prefixes the key value. Defaults to "Bearer". Set "" for a raw key
	// (e.g. an "x-goog-api-key"-style header).
	// +optional
	Scheme *string `json:"scheme,omitempty"`
}

// ProviderSecretRef references a key inside a Secret.
type ProviderSecretRef struct {
	Name string `json:"name"`
	// Key is the Secret data key. Defaults to the Secret's single key if omitted.
	// +optional
	Key string `json:"key,omitempty"`
}

// IsProvider reports whether this tier routes to an external provider.
func (t *ModelTier) IsProvider() bool { return t.Provider != nil }

// EffectivePort returns the provider port (default 443).
func (p *ModelProvider) EffectivePort() int32 {
	if p.Port > 0 {
		return p.Port
	}
	return 443
}

// EffectiveTLS reports whether backend TLS is on (default true).
func (p *ModelProvider) EffectiveTLS() bool { return p.TLS == nil || *p.TLS }

// EffectiveHeader returns the auth header name (default Authorization).
func (a *ProviderAuth) EffectiveHeader() string {
	if a != nil && a.Header != "" {
		return a.Header
	}
	return "Authorization"
}

// EffectiveScheme returns the auth scheme prefix (default "Bearer").
func (a *ProviderAuth) EffectiveScheme() string {
	if a != nil && a.Scheme != nil {
		return *a.Scheme
	}
	return "Bearer"
}

// ModelBackendRef references the backend that serves a tier.
type ModelBackendRef struct {
	// Group is the API group of the backend ("" for a core Service,
	// "gateway.inference.x-k8s.io" for an InferencePool).
	Group string `json:"group,omitempty"`

	// Kind is "InferencePool" or "Service".
	Kind string `json:"kind"`

	// Name is the backend name in the policy namespace.
	Name string `json:"name"`
}

// ModelEntitlements restricts tier access by the caller's verified group.
type ModelEntitlements struct {
	// GroupClaim names the verified JWT claim (or forwarded header) that carries
	// the caller's group. Defaults to "group".
	GroupClaim string `json:"groupClaim,omitempty"`

	// Rules lists, per group, the tiers that group may reach.
	Rules []EntitlementRule `json:"rules,omitempty"`
}

// EntitlementRule allows a group to reach a set of tiers.
type EntitlementRule struct {
	// Group is the group-claim value this rule applies to.
	Group string `json:"group"`

	// Allow lists the tier names this group may be routed to.
	Allow []string `json:"allow"`
}

// UnentitledAction controls what happens when a caller is not entitled to the
// resolved tier.
type UnentitledAction struct {
	// Type is "Downgrade" (default) or "Reject".
	Type string `json:"type,omitempty"`

	// StatusCode is the HTTP status returned when Type is Reject (or a Downgrade
	// finds no allowed tier). Defaults to 403.
	StatusCode int `json:"statusCode,omitempty"`
}

// AIModelRoutePolicyStatus is the observed state.
type AIModelRoutePolicyStatus struct {
	// Conditions holds standard condition types.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ─── Defaults ────────────────────────────────────────────────────────────────

// EffectiveModelField returns the body key to route on, defaulting to "model".
func (s *AIModelRoutePolicySpec) EffectiveModelField() string {
	if s.ModelField != "" {
		return s.ModelField
	}
	return "model"
}

// Default discovery convention keys.
const (
	DefaultAliasAnnotation = "ai.ako.vmware.com/model-alias"
	DefaultTierLabel       = "ai.ako.vmware.com/tier"
)

// IsEnabled reports whether pod-based model discovery is turned on.
func (d *ModelDiscovery) IsEnabled() bool {
	return d != nil && d.Enabled
}

// EffectiveAliasAnnotation returns the alias annotation key, defaulted.
func (d *ModelDiscovery) EffectiveAliasAnnotation() string {
	if d != nil && d.AliasAnnotation != "" {
		return d.AliasAnnotation
	}
	return DefaultAliasAnnotation
}

// EffectiveTierLabel returns the tier label key, defaulted.
func (d *ModelDiscovery) EffectiveTierLabel() string {
	if d != nil && d.TierLabel != "" {
		return d.TierLabel
	}
	return DefaultTierLabel
}

// EffectiveNamespaces returns the namespaces discovery may read from,
// defaulting to the policy's own namespace.
func (d *ModelDiscovery) EffectiveNamespaces(policyNamespace string) []string {
	if d != nil && len(d.Namespaces) > 0 {
		return d.Namespaces
	}
	return []string{policyNamespace}
}

// AllowsNamespace reports whether discovery may read pods in ns.
func (d *ModelDiscovery) AllowsNamespace(policyNamespace, ns string) bool {
	for _, allowed := range d.EffectiveNamespaces(policyNamespace) {
		if allowed == ns {
			return true
		}
	}
	return false
}

// EffectiveGroupClaim returns the entitlement group claim, defaulting to "group".
func (e *ModelEntitlements) EffectiveGroupClaim() string {
	if e != nil && e.GroupClaim != "" {
		return e.GroupClaim
	}
	return "group"
}

// EffectiveType returns the unentitled action type, defaulting to "Downgrade".
func (a *UnentitledAction) EffectiveType() string {
	if a != nil && a.Type != "" {
		return a.Type
	}
	return "Downgrade"
}

// EffectiveStatusCode returns the unentitled reject status, defaulting to 403.
func (a *UnentitledAction) EffectiveStatusCode() int {
	if a != nil && a.StatusCode >= 400 {
		return a.StatusCode
	}
	return 403
}

// ─── Tier resolution / entitlement (pure Go, mirrors the generated Lua) ───────

// prefixEntry is one trailing-"*" glob rule from ModelTiers.
type prefixEntry struct {
	Prefix string // the part before "*"
	Tier   string
}

// prefixEntries returns the ModelTiers glob rules sorted by descending prefix
// length, so the longest (most specific) prefix wins — the same order baked into
// the generated Lua.
func (s *AIModelRoutePolicySpec) prefixEntries() []prefixEntry {
	var out []prefixEntry
	for k, v := range s.ModelTiers {
		if strings.HasSuffix(k, "*") {
			out = append(out, prefixEntry{Prefix: strings.TrimSuffix(k, "*"), Tier: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Prefix) != len(out[j].Prefix) {
			return len(out[i].Prefix) > len(out[j].Prefix)
		}
		return out[i].Prefix < out[j].Prefix // stable tiebreak
	})
	return out
}

// ResolveTier returns the tier name for a requested model: exact match first,
// then longest matching prefix glob, then DefaultTier. (A real model name never
// ends in "*", so an exact hit can only be a non-glob key — matching the
// exact-table baked into the generated Lua.)
func (s *AIModelRoutePolicySpec) ResolveTier(model string) string {
	if t, ok := s.ModelTiers[model]; ok && !strings.HasSuffix(model, "*") {
		return t
	}
	for _, pe := range s.prefixEntries() {
		if strings.HasPrefix(model, pe.Prefix) {
			return pe.Tier
		}
	}
	return s.DefaultTier
}

// tierNames returns the tier names in preference (spec) order.
func (s *AIModelRoutePolicySpec) tierNames() []string {
	out := make([]string, 0, len(s.Tiers))
	for _, t := range s.Tiers {
		out = append(out, t.Name)
	}
	return out
}

// allowedTiers returns the set of tiers a group may reach. When no entitlements
// are configured every tier is allowed.
func (s *AIModelRoutePolicySpec) allowedTiers(group string) map[string]bool {
	if s.Entitlements == nil || len(s.Entitlements.Rules) == 0 {
		all := make(map[string]bool, len(s.Tiers))
		for _, t := range s.Tiers {
			all[t.Name] = true
		}
		return all
	}
	for _, r := range s.Entitlements.Rules {
		if r.Group == group {
			set := make(map[string]bool, len(r.Allow))
			for _, t := range r.Allow {
				set[t] = true
			}
			return set
		}
	}
	return map[string]bool{}
}

// IsAllowed reports whether group may reach tier.
func (s *AIModelRoutePolicySpec) IsAllowed(group, tier string) bool {
	return s.allowedTiers(group)[tier]
}

// BestAllowedTier returns the highest-preference tier the group may reach, or ""
// if none.
func (s *AIModelRoutePolicySpec) BestAllowedTier(group string) string {
	allowed := s.allowedTiers(group)
	for _, name := range s.tierNames() { // spec order = preference
		if allowed[name] {
			return name
		}
	}
	return ""
}

// ─── Validation ──────────────────────────────────────────────────────────────

// Validate checks cross-field referential integrity that OpenAPI cannot express:
// every ModelTiers value and DefaultTier must name a declared tier, and every
// entitlement Allow entry must reference a declared tier.
func (s *AIModelRoutePolicySpec) Validate() error {
	known := make(map[string]bool, len(s.Tiers))
	for _, t := range s.Tiers {
		if t.Name == "" {
			return fmt.Errorf("tier with empty name")
		}
		if known[t.Name] {
			return fmt.Errorf("duplicate tier name %q", t.Name)
		}
		if t.IsProvider() {
			if t.Provider.Host == "" || t.Provider.Path == "" {
				return fmt.Errorf("tier %q: provider requires host and path", t.Name)
			}
			if t.Provider.Auth != nil && t.Provider.Auth.SecretRef.Name == "" {
				return fmt.Errorf("tier %q: provider.auth requires secretRef.name", t.Name)
			}
		} else if t.BackendRef.Name == "" {
			return fmt.Errorf("tier %q: backendRef.name or provider is required", t.Name)
		}
		known[t.Name] = true
	}
	if len(known) == 0 {
		return fmt.Errorf("at least one tier is required")
	}
	if s.DefaultTier == "" || !known[s.DefaultTier] {
		return fmt.Errorf("defaultTier %q is not a declared tier", s.DefaultTier)
	}
	for model, tier := range s.ModelTiers {
		if !known[tier] {
			return fmt.Errorf("modelTiers[%q] references unknown tier %q", model, tier)
		}
	}
	if s.Entitlements != nil {
		for _, r := range s.Entitlements.Rules {
			for _, t := range r.Allow {
				if !known[t] {
					return fmt.Errorf("entitlements group %q allows unknown tier %q", r.Group, t)
				}
			}
		}
	}
	return nil
}
