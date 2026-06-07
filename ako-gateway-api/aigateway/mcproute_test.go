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

// sampleMCPSpec builds a policy with a star role, an exact+prefix role, and a
// single-tool role, sharing the IdP of an AIGatewayAuthPolicy named "llm-auth".
func sampleMCPSpec() AIMCPRoutePolicySpec {
	return AIMCPRoutePolicySpec{
		TargetRef: PolicyTargetRef{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "mcp-route"},
		AuthRef:   &AuthPolicyRef{Name: "llm-auth"},
		Session:   &MCPSession{},
		ToolAccess: &MCPToolAccess{
			RoleClaim: "role",
			Rules: []ToolAccessRule{
				{Role: "operator", Allow: []string{"*"}},
				{Role: "app-owner", Allow: []string{"search.query", "docs.*"}},
				{Role: "guest", Allow: []string{"search.query"}},
			},
		},
		OnUnauthorized: &UnauthorizedAction{Type: "Reject", StatusCode: 403},
	}
}

func TestIsToolAllowed(t *testing.T) {
	s := sampleMCPSpec()
	cases := []struct {
		role, tool string
		want       bool
	}{
		{"operator", "filesystem.write", true}, // star role: anything
		{"app-owner", "search.query", true},    // exact
		{"app-owner", "docs.read", true},       // prefix docs.*
		{"app-owner", "docs.write", true},      // prefix docs.*
		{"app-owner", "filesystem.write", false},
		{"guest", "search.query", true},
		{"guest", "docs.read", false},
		{"nobody", "search.query", false}, // unknown role
		{"app-owner", "", false},          // empty tool fails closed
	}
	for _, c := range cases {
		if got := s.IsToolAllowed(c.role, c.tool); got != c.want {
			t.Errorf("IsToolAllowed(%q,%q) = %v, want %v", c.role, c.tool, got, c.want)
		}
	}
}

func TestIsToolAllowedNoToolAccessAllowsAll(t *testing.T) {
	s := sampleMCPSpec()
	s.ToolAccess = nil
	if !s.IsToolAllowed("anyone", "anything") {
		t.Error("with no toolAccess, every tool should be allowed (OAuth-only)")
	}
}

func TestMCPEffectiveDefaults(t *testing.T) {
	var sess *MCPSession
	if got := sess.EffectiveHeader(); got != "Mcp-Session-Id" {
		t.Errorf("default header = %q, want Mcp-Session-Id", got)
	}
	if got := sess.EffectiveTimeout(); got != "30m" {
		t.Errorf("default timeout = %q, want 30m", got)
	}
	var ta *MCPToolAccess
	if got := ta.EffectiveRoleClaim(); got != "role" {
		t.Errorf("default roleClaim = %q, want role", got)
	}
	var ua *UnauthorizedAction
	if got := ua.EffectiveType(); got != "Reject" {
		t.Errorf("default type = %q, want Reject", got)
	}
	if got := ua.EffectiveStatusCode(); got != 403 {
		t.Errorf("default statusCode = %d, want 403", got)
	}
}

func TestMCPValidate(t *testing.T) {
	good := sampleMCPSpec()
	if err := good.Validate(); err != nil {
		t.Fatalf("sample spec should be valid: %v", err)
	}

	bad := sampleMCPSpec()
	bad.TargetRef.Name = ""
	if err := bad.Validate(); err == nil {
		t.Error("expected error for empty targetRef.name")
	}

	bad = sampleMCPSpec()
	bad.AuthRef = &AuthPolicyRef{Name: ""}
	if err := bad.Validate(); err == nil {
		t.Error("expected error for authRef with empty name")
	}

	bad = sampleMCPSpec()
	bad.ToolAccess.Rules = append(bad.ToolAccess.Rules, ToolAccessRule{Role: "operator", Allow: []string{"x"}})
	if err := bad.Validate(); err == nil {
		t.Error("expected error for duplicate role")
	}

	bad = sampleMCPSpec()
	bad.ToolAccess.Rules[0].Role = ""
	if err := bad.Validate(); err == nil {
		t.Error("expected error for empty role")
	}

	bad = sampleMCPSpec()
	bad.ToolAccess.Rules[2].Allow = nil
	if err := bad.Validate(); err == nil {
		t.Error("expected error for role allowing no tools")
	}

	bad = sampleMCPSpec()
	bad.OnUnauthorized.Type = "Bogus"
	if err := bad.Validate(); err == nil {
		t.Error("expected error for unknown onUnauthorized.type")
	}
}

func TestGenerateMCPScriptsReq(t *testing.T) {
	p := &AIMCPRoutePolicy{Spec: sampleMCPSpec()}
	if got := GenerateMCPToolAuthScripts(p).ReqScript; !strings.Contains(got, "set_request_body_buffer_size(32768)") {
		t.Errorf("ReqScript should enable 32KB buffering, got:\n%s", got)
	}
}

func TestGenerateMCPScriptsReqData(t *testing.T) {
	p := &AIMCPRoutePolicy{Spec: sampleMCPSpec()}
	s := GenerateMCPToolAuthScripts(p).ReqDataScript

	mustContain := []string{
		"avi.http.get_req_body(32768)",      // verified read API
		`json_str(_body, "method")`,         // method extraction
		`DQ .. "params" .. DQ`,              // nested params scan
		`method == "tools/call"`,            // only tool calls are gated
		"jwt_claim(ROLE_CLAIM)",             // verified role read
		`local ROLE_CLAIM = "role"`,         // baked claim name
		`["operator"]=true`,                 // star role baked
		`["search.query"]=true`,             // exact tool baked
		`{ "docs." }`,                       // prefix glob baked
		`avi.http.response(403`,             // reject path
		"tool_not_authorized",               // JSON-RPC error
	}
	for _, sub := range mustContain {
		if !strings.Contains(s, sub) {
			t.Errorf("ReqDataScript missing %q\n---\n%s", sub, s)
		}
	}
}

func TestGenerateMCPLogMode(t *testing.T) {
	spec := sampleMCPSpec()
	spec.OnUnauthorized = &UnauthorizedAction{Type: "Log"}
	s := GenerateMCPToolAuthScripts(&AIMCPRoutePolicy{Spec: spec}).ReqDataScript

	if !strings.Contains(s, "X-MCP-Tool-Denied") {
		t.Errorf("Log mode should tag denials, got:\n%s", s)
	}
	if strings.Contains(s, "avi.http.response(") {
		t.Errorf("Log mode must not reject the request\n%s", s)
	}
}

func TestGenerateMCPNoToolAccess(t *testing.T) {
	spec := sampleMCPSpec()
	spec.ToolAccess = nil
	scripts := GenerateMCPToolAuthScripts(&AIMCPRoutePolicy{Spec: spec})
	if scripts.ReqScript != "" || scripts.ReqDataScript != "" {
		t.Errorf("no toolAccess should produce no scripts, got req=%q reqdata=%q", scripts.ReqScript, scripts.ReqDataScript)
	}
}
