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

// A request with no JSON-RPC method skips the allow-list entirely, so any valid
// token reaches the backend. requireMethod closes that, and must stay opt-in:
// the digest agents serve plain REST through this same gateway.
func TestRequireMethodIsOptInAndFailsClosed(t *testing.T) {
	p := skillPolicy()
	off := GenerateA2AScripts(p, ClaimModeJWTQuery).ReqDataScript
	if strings.Contains(off, `if method == "" then`) {
		t.Error("requireMethod defaulted on — plain REST callers would start being rejected")
	}

	p.Spec.AgentAccess.RequireMethod = true
	on := GenerateA2AScripts(p, ClaimModeJWTQuery).ReqDataScript
	if !strings.Contains(on, `if method == "" then`) {
		t.Fatal("requireMethod set but no methodless branch emitted")
	}
	// The rejection must be the configured one, not a bare return.
	idx := strings.Index(on, `if method == "" then`)
	if !strings.Contains(on[idx:], "avi.http.response") {
		t.Error("methodless branch does not reject the request")
	}
}

// Target binding: a token minted for one agent must not be spendable at another.
// Opt-in, and an absent claim is never rejected so legacy callers keep working.
func TestTargetAgentBinding(t *testing.T) {
	p := skillPolicy()
	off := GenerateA2AScripts(p, ClaimModeJWTQuery).ReqDataScript
	if strings.Contains(off, "EXPECTED_TARGET") {
		t.Error("target binding emitted without targetAgent set")
	}

	p.Spec.AgentAccess.TargetAgent = "weather-agent"
	on := GenerateA2AScripts(p, ClaimModeJWTQuery).ReqDataScript
	for _, want := range []string{
		`local EXPECTED_TARGET = "weather-agent"`,
		`local _tgt = jwt_claim("target")`,
		`if _tgt ~= "" and _tgt ~= EXPECTED_TARGET then`,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Must be checked before the allow-list, so a misdirected token is refused
	// on target rather than on whatever its skill happens to be.
	if strings.Index(on, "EXPECTED_TARGET") > strings.Index(on, `if method ~= "" then`) {
		t.Error("target check runs after the allow-list")
	}
	// And it must be defined after the claim reader it uses.
	if h := strings.Index(on, "local function jwt_claim"); h < 0 || h > strings.Index(on, `jwt_claim("target")`) {
		t.Error("jwt_claim used before it is defined")
	}
}

// REST agents: their rules can never match a JSON-RPC method or a skill, so
// without path authorization they run unauthorized behind a valid token.
func TestAuthorizePathsGatesRESTEndpoints(t *testing.T) {
	p := skillPolicy()
	off := GenerateA2AScripts(p, ClaimModeJWTQuery).ReqDataScript
	if strings.Contains(off, "_ex[path]") {
		t.Error("path matching emitted without authorizePaths set")
	}
	// Default must keep the JSON-RPC-only guard, or REST agents start being denied.
	if !strings.Contains(off, `if method ~= "" then`) {
		t.Error("default no longer gates on method")
	}

	p.Spec.AgentAccess.AuthorizePaths = true
	on := GenerateA2AScripts(p, ClaimModeJWTQuery).ReqDataScript
	if !strings.Contains(on, "_ex[path]") {
		t.Error("exact path matching not emitted")
	}
	if !strings.Contains(on, `string.sub(path, 1, string.len(_p)) == _p`) {
		t.Error("prefix path matching not emitted")
	}
	// The rules must now run for EVERY request, not only JSON-RPC ones.
	if strings.Contains(on, `if method ~= "" then
    local _agent`) {
		t.Error("still gated on method, so REST calls would skip authorization")
	}
	// An empty method must not be compared against the allow-list, or a REST call
	// would match a rule whose entry happens to be the empty string.
	if !strings.Contains(on, `if _ex and method ~= "" and _ex[method] then`) {
		t.Error("empty method is not guarded before the exact match")
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
