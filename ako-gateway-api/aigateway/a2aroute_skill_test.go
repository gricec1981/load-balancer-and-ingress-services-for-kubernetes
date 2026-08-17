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
	"strings"
	"testing"
)

// skillPolicy restricts one caller to a skill and another to a method, so both
// dimensions are exercised by the same policy.
func skillPolicy() *AIA2ARoutePolicy {
	return &AIA2ARoutePolicy{
		Spec: AIA2ARoutePolicySpec{
			TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "log-collector-agent-a2a"},
			AgentAccess: &A2AAgentAccess{
				AgentClaim: "sub",
				Rules: []AgentAccessRule{
					{Agent: "avi-controller-agent", Allow: []string{"log.collection"}},
					{Agent: "agent-hub", Allow: []string{"tasks/send"}},
					{Agent: "ops-agent", Allow: []string{"log.*"}},
					{Agent: "admin-agent", Allow: []string{"*"}},
				},
			},
		},
	}
}

func TestIsCallAllowedMatchesSkillOrMethod(t *testing.T) {
	s := skillPolicy().Spec
	cases := []struct {
		name          string
		agent         string
		method, skill string
		want          bool
	}{
		// The rule the SE could never match before: a skill-ID allow-list.
		{"skill rule, skill presented", "avi-controller-agent", "tasks/send", "log.collection", true},
		{"skill rule, no skill claim", "avi-controller-agent", "tasks/send", "", false},
		{"skill rule, wrong skill", "avi-controller-agent", "tasks/send", "log.deletion", false},

		// Method rules keep working exactly as before.
		{"method rule, legacy token", "agent-hub", "tasks/send", "", true},
		{"method rule, other method", "agent-hub", "tasks/cancel", "", false},
		{"method rule, skill irrelevant", "agent-hub", "tasks/send", "anything", true},

		// Prefix globs apply to either dimension.
		{"prefix matches skill", "ops-agent", "tasks/send", "log.collection", true},
		{"prefix does not match method", "ops-agent", "tasks/send", "", false},

		// Wildcard and unknown callers.
		{"wildcard allows all", "admin-agent", "tasks/cancel", "", true},
		{"unlisted agent denied", "weather-agent", "tasks/send", "log.collection", false},

		// An empty method is never authorized, regardless of skill.
		{"empty method denied", "admin-agent", "", "log.collection", false},
	}
	for _, c := range cases {
		if got := s.IsCallAllowed(c.agent, c.method, c.skill); got != c.want {
			t.Errorf("%s: IsCallAllowed(%q,%q,%q) = %v, want %v",
				c.name, c.agent, c.method, c.skill, got, c.want)
		}
	}
}

// The empty skill must never satisfy a rule — otherwise a legacy token would be
// authorized by any policy, which would be a silent privilege escalation.
func TestEmptySkillNeverMatches(t *testing.T) {
	s := AIA2ARoutePolicySpec{
		AgentAccess: &A2AAgentAccess{
			Rules: []AgentAccessRule{{Agent: "a", Allow: []string{"", "log.collection"}}},
		},
	}
	if s.IsCallAllowed("a", "tasks/send", "") {
		t.Error("empty skill matched an empty allow-list entry")
	}
}

// IsMethodAllowed is the pre-existing entry point; it must keep its semantics.
func TestIsMethodAllowedUnchanged(t *testing.T) {
	s := skillPolicy().Spec
	if !s.IsMethodAllowed("agent-hub", "tasks/send") {
		t.Error("method rule regressed")
	}
	if s.IsMethodAllowed("avi-controller-agent", "tasks/send") {
		t.Error("a skill-only rule must not be satisfied by the method alone")
	}
}

func TestEffectiveSkillClaimDefaults(t *testing.T) {
	var nilAccess *A2AAgentAccess
	if got := nilAccess.EffectiveSkillClaim(); got != "skill" {
		t.Errorf("nil access: got %q, want \"skill\"", got)
	}
	if got := (&A2AAgentAccess{}).EffectiveSkillClaim(); got != "skill" {
		t.Errorf("unset: got %q, want \"skill\"", got)
	}
	if got := (&A2AAgentAccess{SkillClaim: "scope"}).EffectiveSkillClaim(); got != "scope" {
		t.Errorf("override: got %q, want \"scope\"", got)
	}
}

// The generated Lua must read the skill claim, guard against the empty string,
// and still be emitted below the jwt_claim helper that defines the reader.
func TestGeneratedLuaChecksSkill(t *testing.T) {
	scripts := GenerateA2AScripts(skillPolicy(), ClaimModeJWTQuery)
	req := scripts.ReqDataScript

	for _, want := range []string{
		`local SKILL_CLAIM = "skill"`,
		`local _skill = jwt_claim(SKILL_CLAIM)`,
		`_skill ~= "" and _ex and _ex[_skill]`,
	} {
		if !strings.Contains(req, want) {
			t.Errorf("generated Lua missing %q", want)
		}
	}
	// The prefix loop must test the skill too, not just the method.
	if !strings.Contains(req, `string.sub(_skill, 1, string.len(_p)) == _p`) {
		t.Error("prefix rules are not applied to the skill claim")
	}
	// Ordering: jwt_claim must be defined before it is called.
	helper := strings.Index(req, "local function jwt_claim")
	call := strings.Index(req, "jwt_claim(SKILL_CLAIM)")
	if helper < 0 || call < 0 || helper > call {
		t.Errorf("jwt_claim defined at %d but called at %d", helper, call)
	}
}
