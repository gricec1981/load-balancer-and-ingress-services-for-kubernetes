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

// ─── AIA2ARoutePolicy ────────────────────────────────────────────────────────

// AIA2ARoutePolicyGVR is the GroupVersionResource for AIA2ARoutePolicy.
var AIA2ARoutePolicyGVR = schema.GroupVersionResource{
	Group:    PolicyGroup,
	Version:  PolicyVersion,
	Resource: "aia2aroutepolicies",
}

// AIA2ARoutePolicy governs Agent-to-Agent (A2A) protocol traffic on an
// HTTPRoute. It enforces agent identity (via a shared AIGatewayAuthPolicy),
// pins multi-turn A2A tasks to the same backend (taskAffinity), and restricts
// which calling agents may invoke which A2A methods (agentAccess).
//
// Three traffic shapes handled:
//   - Agent card discovery: GET /.well-known/agent.json
//   - Task submission:      POST / (JSON-RPC tasks/send, tasks/sendSubscribe)
//   - Task follow-ups:      tasks/get, tasks/cancel, tasks/resubscribe
//
// See docs/gateway-api/ai-gateway-a2a.md.
type AIA2ARoutePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIA2ARoutePolicySpec   `json:"spec"`
	Status AIA2ARoutePolicyStatus `json:"status,omitempty"`
}

// AIA2ARoutePolicySpec is the desired state of an AIA2ARoutePolicy.
type AIA2ARoutePolicySpec struct {
	// TargetRef identifies the A2A HTTPRoute this policy applies to.
	TargetRef PolicyTargetRef `json:"targetRef"`

	// AuthRef references an AIGatewayAuthPolicy in the same namespace whose
	// identity provider this A2A route shares. AKO reuses that policy's issuer
	// Pool + OAuth AuthProfile. Omit to run unauthenticated (not recommended).
	// +optional
	AuthRef *AuthPolicyRef `json:"authRef,omitempty"`

	// AgentCard configures proxying and optional URL rewriting for the agent
	// card served at GET /.well-known/agent.json.
	// +optional
	AgentCard *A2AAgentCard `json:"agentCard,omitempty"`

	// TaskAffinity configures task-level backend persistence. A2A tasks span
	// multiple JSON-RPC calls (send → get/cancel/resubscribe); all calls for a
	// given task ID are pinned to the same backend instance.
	// +optional
	TaskAffinity *A2ATaskAffinity `json:"taskAffinity,omitempty"`

	// AgentAccess restricts which calling agents may invoke which A2A methods.
	// The calling agent's identity is resolved from a JWT claim.
	// +optional
	AgentAccess *A2AAgentAccess `json:"agentAccess,omitempty"`

	// OnUnauthorized controls the response when a calling agent is not
	// permitted to invoke the requested method.
	// +optional
	OnUnauthorized *UnauthorizedAction `json:"onUnauthorized,omitempty"`
}

// A2AAgentCard configures how the gateway handles agent card discovery.
type A2AAgentCard struct {
	// Rewrite, when true, rewrites the `url` field in the agent card JSON to
	// the gateway URL. Use when the backend's self-reported address is internal.
	Rewrite bool `json:"rewrite,omitempty"`

	// URL is the public gateway URL injected when Rewrite is true.
	// +optional
	URL string `json:"url,omitempty"`
}

// A2ATaskAffinity pins multi-turn A2A task calls to the same backend.
// On tasks/send the response body's result.id is captured into an Avi VS
// persistence table. Subsequent calls carrying that task ID in params.id are
// re-pinned to the backend that created the task.
type A2ATaskAffinity struct {
	// Timeout is the idle task affinity lifetime. Defaults to "30m".
	Timeout string `json:"timeout,omitempty"`
}

// A2AAgentAccess restricts which calling agents may invoke which A2A methods.
type A2AAgentAccess struct {
	// AgentClaim names the verified JWT claim carrying the calling agent's
	// identity. Defaults to "sub". Use a distinct claim (e.g. "agent_id") to
	// separate human-user and agent tokens issued by the same IdP.
	AgentClaim string `json:"agentClaim,omitempty"`

	// Rules lists, per calling-agent identity, the A2A JSON-RPC methods that
	// agent may invoke.
	Rules []AgentAccessRule `json:"rules,omitempty"`
}

// AgentAccessRule permits a named calling agent to invoke a set of A2A methods.
type AgentAccessRule struct {
	// Agent is the agentClaim value this rule applies to (exact match).
	Agent string `json:"agent"`

	// Allow lists the A2A JSON-RPC methods (e.g. "tasks/send",
	// "tasks/sendSubscribe") this agent may invoke. A single "*" allows all
	// methods; a trailing "*" is a prefix glob (e.g. "tasks/*").
	Allow []string `json:"allow"`
}

// AIA2ARoutePolicyStatus is the observed state.
type AIA2ARoutePolicyStatus struct {
	// Conditions holds standard condition types.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ─── Defaults ────────────────────────────────────────────────────────────────

// EffectiveTimeout returns the task affinity idle timeout, defaulting to "30m".
func (t *A2ATaskAffinity) EffectiveTimeout() string {
	if t != nil && t.Timeout != "" {
		return t.Timeout
	}
	return "30m"
}

// EffectiveAgentClaim returns the JWT claim carrying the calling agent's
// identity, defaulting to "sub".
func (a *A2AAgentAccess) EffectiveAgentClaim() string {
	if a != nil && a.AgentClaim != "" {
		return a.AgentClaim
	}
	return "sub"
}

// ─── Method authorization (pure Go, mirrors the generated Lua) ───────────────

// IsMethodAllowed reports whether agent may invoke method. An empty method
// is never allowed. When no agentAccess rules are configured every method
// is allowed (authorization by OAuth alone).
func (s *AIA2ARoutePolicySpec) IsMethodAllowed(agent, method string) bool {
	if s.AgentAccess == nil || len(s.AgentAccess.Rules) == 0 {
		return true
	}
	if method == "" {
		return false
	}
	for _, r := range s.AgentAccess.Rules {
		if r.Agent != agent {
			continue
		}
		for _, a := range r.Allow {
			switch {
			case a == "*":
				return true
			case strings.HasSuffix(a, "*"):
				if strings.HasPrefix(method, strings.TrimSuffix(a, "*")) {
					return true
				}
			case a == method:
				return true
			}
		}
		return false
	}
	return false
}

// ─── Validation ──────────────────────────────────────────────────────────────

// Validate checks cross-field integrity that OpenAPI cannot express.
func (s *AIA2ARoutePolicySpec) Validate() error {
	if s.TargetRef.Name == "" {
		return fmt.Errorf("targetRef.name is required")
	}
	if s.AuthRef != nil && s.AuthRef.Name == "" {
		return fmt.Errorf("authRef.name must not be empty when authRef is set")
	}
	if s.AgentCard != nil && s.AgentCard.Rewrite && s.AgentCard.URL == "" {
		return fmt.Errorf("agentCard.url is required when agentCard.rewrite is true")
	}
	if s.AgentAccess != nil {
		seen := make(map[string]bool, len(s.AgentAccess.Rules))
		for _, r := range s.AgentAccess.Rules {
			if r.Agent == "" {
				return fmt.Errorf("agentAccess rule with empty agent")
			}
			if seen[r.Agent] {
				return fmt.Errorf("duplicate agentAccess agent %q", r.Agent)
			}
			seen[r.Agent] = true
			if len(r.Allow) == 0 {
				return fmt.Errorf("agentAccess agent %q allows no methods", r.Agent)
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
