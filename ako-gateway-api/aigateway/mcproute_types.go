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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ─── AIMCPRoutePolicy ─────────────────────────────────────────────────────────

// AIMCPRoutePolicyGVR is the GroupVersionResource for AIMCPRoutePolicy.
var AIMCPRoutePolicyGVR = schema.GroupVersionResource{
	Group:    PolicyGroup,
	Version:  PolicyVersion,
	Resource: "aimcproutepolicies",
}

// Built-in Avi 32.1.1 MCP objects the translator reuses (verified live — see
// docs/gateway-api/ai-gateway-mcp.md §2). AKO references these system objects on
// the MCP VS rather than generating its own session logic.
const (
	// MCPApplicationProfile is the system HTTP application profile with
	// app_service_type APP_SERVICE_TYPE_HTTP_MCP (websockets + HTTP/2).
	MCPApplicationProfile = "System-Secure-HTTP-MCP"

	// MCPSessionDataScript is the system DataScriptSet that pins MCP sessions to
	// the same pool+server via VS persistence tables keyed on Mcp-Session-Id
	// (create-on-response, delete-on-DELETE).
	MCPSessionDataScript = "System-Standard-MCP"
)

// WatchedAIGatewayPolicyGVRs lists every AI Gateway policy CRD the controller
// registers an informer for. It is the single source of truth shared by the
// informer wiring and the RBAC guardrail test (see rbac_clusterrole_test.go), so
// adding a new policy CRD here forces a matching ClusterRole grant — converting
// the "informer is RBAC-forbidden" runtime failure into a build-time test failure.
func WatchedAIGatewayPolicyGVRs() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		AIGatewayAuthPolicyGVR,
		AITokenRateLimitPolicyGVR,
		AIModelRoutePolicyGVR,
		AIMCPRoutePolicyGVR,
		AIA2ARoutePolicyGVR,
	}
}

// AIMCPRoutePolicy governs Model Context Protocol (agent↔tool) traffic on an
// HTTPRoute attached to an MCP Gateway. It ties enforcement to the same identity
// provider as the LLM gateway (authRef → an AIGatewayAuthPolicy whose issuer Pool
// and OAuth AuthProfile are reused), keeps stateful MCP sessions pinned via
// header persistence (session.header, default Mcp-Session-Id), and authorizes
// individual tool calls by the caller's verified role (toolAccess). The per-tool
// decision is enforced by an SE DataScript that reads the JSON-RPC body
// (method / params.name) — see docs/gateway-api/ai-gateway-mcp.md.
type AIMCPRoutePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIMCPRoutePolicySpec   `json:"spec"`
	Status AIMCPRoutePolicyStatus `json:"status,omitempty"`
}

// AIMCPRoutePolicySpec is the desired state of an AIMCPRoutePolicy.
type AIMCPRoutePolicySpec struct {
	// TargetRef identifies the MCP HTTPRoute (or Gateway) this policy applies to.
	TargetRef PolicyTargetRef `json:"targetRef"`

	// AuthRef references an AIGatewayAuthPolicy in the same namespace whose
	// identity provider this MCP route shares. AKO reuses that policy's issuer
	// Pool + OAuth AuthProfile (same IdP / login) and emits a separate SSO policy
	// for the MCP VS. Omit to run the MCP route unauthenticated (not recommended).
	// +optional
	AuthRef *AuthPolicyRef `json:"authRef,omitempty"`

	// Session configures MCP session persistence.
	// +optional
	Session *MCPSession `json:"session,omitempty"`

	// ToolAccess configures per-role authorization of MCP tool calls.
	// +optional
	ToolAccess *MCPToolAccess `json:"toolAccess,omitempty"`

	// OnUnauthorized controls behaviour when a caller invokes a tool their role
	// may not use.
	// +optional
	OnUnauthorized *UnauthorizedAction `json:"onUnauthorized,omitempty"`
}

// AuthPolicyRef references an AIGatewayAuthPolicy by name in the policy namespace.
type AuthPolicyRef struct {
	// Name is the AIGatewayAuthPolicy name whose IdP this MCP route shares.
	Name string `json:"name"`
}

// MCPSession configures session persistence for stateful MCP sessions.
type MCPSession struct {
	// Header is the request header carrying the MCP session id. Defaults to
	// "Mcp-Session-Id".
	Header string `json:"header,omitempty"`

	// Timeout is the idle session lifetime (e.g. "30m"). Defaults to "30m".
	Timeout string `json:"timeout,omitempty"`
}

// MCPToolAccess restricts which MCP tools a caller's role may invoke.
type MCPToolAccess struct {
	// RoleClaim names the verified JWT claim carrying the caller's role.
	// Defaults to "role".
	RoleClaim string `json:"roleClaim,omitempty"`

	// Rules lists, per role, the tool names that role may call.
	Rules []ToolAccessRule `json:"rules,omitempty"`
}

// ToolAccessRule allows a role to invoke a set of MCP tools.
type ToolAccessRule struct {
	// Role is the role-claim value this rule applies to.
	Role string `json:"role"`

	// Allow lists the tool names (or JSON-RPC methods) this role may invoke. A
	// single "*" allows all tools; a trailing "*" is a prefix glob (e.g. "docs.*").
	Allow []string `json:"allow"`
}

// UnauthorizedAction controls the response when a caller is not authorized to
// invoke the requested tool.
type UnauthorizedAction struct {
	// Type is "Reject" (default) or "Log" (count/tag but allow).
	Type string `json:"type,omitempty"`

	// StatusCode is the HTTP status returned when Type is Reject. Defaults to 403.
	StatusCode int `json:"statusCode,omitempty"`
}

// AIMCPRoutePolicyStatus is the observed state.
type AIMCPRoutePolicyStatus struct {
	// Conditions holds standard condition types.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ─── Defaults ────────────────────────────────────────────────────────────────

// EffectiveHeader returns the session persistence header, defaulting to
// "Mcp-Session-Id".
func (s *MCPSession) EffectiveHeader() string {
	if s != nil && s.Header != "" {
		return s.Header
	}
	return "Mcp-Session-Id"
}

// EffectiveTimeout returns the session idle timeout, defaulting to "30m".
func (s *MCPSession) EffectiveTimeout() string {
	if s != nil && s.Timeout != "" {
		return s.Timeout
	}
	return "30m"
}

// EffectiveRoleClaim returns the tool-access role claim, defaulting to "role".
func (t *MCPToolAccess) EffectiveRoleClaim() string {
	if t != nil && t.RoleClaim != "" {
		return t.RoleClaim
	}
	return "role"
}

// EffectiveType returns the unauthorized action type, defaulting to "Reject".
func (a *UnauthorizedAction) EffectiveType() string {
	if a != nil && a.Type != "" {
		return a.Type
	}
	return "Reject"
}

// EffectiveStatusCode returns the unauthorized reject status, defaulting to 403.
func (a *UnauthorizedAction) EffectiveStatusCode() int {
	if a != nil && a.StatusCode >= 400 {
		return a.StatusCode
	}
	return 403
}

// ─── Tool authorization (pure Go, mirrors the generated Lua) ──────────────────

// rulesByRole indexes the tool-access rules by role.
func (s *AIMCPRoutePolicySpec) rulesByRole(role string) (ToolAccessRule, bool) {
	if s.ToolAccess == nil {
		return ToolAccessRule{}, false
	}
	for _, r := range s.ToolAccess.Rules {
		if r.Role == role {
			return r, true
		}
	}
	return ToolAccessRule{}, false
}

// IsToolAllowed reports whether role may invoke tool. A "*" entry allows all
// tools; a trailing "*" entry is a prefix glob; otherwise the match is exact. An
// empty tool name is never allowed (fail closed). When no toolAccess is
// configured, every tool is allowed (authorization is by OAuth alone).
func (s *AIMCPRoutePolicySpec) IsToolAllowed(role, tool string) bool {
	if s.ToolAccess == nil || len(s.ToolAccess.Rules) == 0 {
		return true
	}
	if tool == "" {
		return false
	}
	rule, ok := s.rulesByRole(role)
	if !ok {
		return false
	}
	for _, a := range rule.Allow {
		switch {
		case a == "*":
			return true
		case strings.HasSuffix(a, "*"):
			if strings.HasPrefix(tool, strings.TrimSuffix(a, "*")) {
				return true
			}
		case a == tool:
			return true
		}
	}
	return false
}

// ─── Validation ──────────────────────────────────────────────────────────────

// Validate checks integrity that OpenAPI cannot express: authRef must name a
// policy, tool-access roles must be unique and non-empty with at least one allow
// entry, and onUnauthorized.type must be a known value.
func (s *AIMCPRoutePolicySpec) Validate() error {
	if s.TargetRef.Name == "" {
		return fmt.Errorf("targetRef.name is required")
	}
	if s.AuthRef != nil && s.AuthRef.Name == "" {
		return fmt.Errorf("authRef.name must not be empty when authRef is set")
	}
	if s.ToolAccess != nil {
		seen := make(map[string]bool, len(s.ToolAccess.Rules))
		for _, r := range s.ToolAccess.Rules {
			if r.Role == "" {
				return fmt.Errorf("toolAccess rule with empty role")
			}
			if seen[r.Role] {
				return fmt.Errorf("duplicate toolAccess role %q", r.Role)
			}
			seen[r.Role] = true
			if len(r.Allow) == 0 {
				return fmt.Errorf("toolAccess role %q allows no tools", r.Role)
			}
		}
	}
	if s.OnUnauthorized != nil && s.OnUnauthorized.Type != "" {
		switch s.OnUnauthorized.Type {
		case "Reject", "Log":
		default:
			return fmt.Errorf("onUnauthorized.type %q must be Reject or Log", s.OnUnauthorized.Type)
		}
	}
	return nil
}
