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

package nodes

import (
	"testing"

	akogatewayapiaigateway "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/ako-gateway-api/aigateway"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/nodes"
)

func authRefTestVS() *nodes.AviEvhVsNode {
	return &nodes.AviEvhVsNode{Name: "vs-tools", Tenant: "admin"}
}

func hasAuthGuard(vs *nodes.AviEvhVsNode) bool {
	for i, ds := range vs.HTTPDSrefs {
		if ds != nil && ds.Name == akogatewayapiaigateway.DSAuthDenyName(vs.Name) {
			return i == 0
		}
	}
	return false
}

// An invalid MCP route policy is skipped, but one that declares an authRef
// must not leave the route unauthenticated: the VS fails closed, and the guard
// is recorded in the rebuild's pass so the sweep keeps it.
func TestApplyMCPRoutePolicyInvalidWithAuthRefFailsClosed(t *testing.T) {
	cases := map[string]*akogatewayapiaigateway.AuthPolicyRef{
		"named authRef": {Name: "llm-auth"},
		"empty authRef": {Name: ""},
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			p := &akogatewayapiaigateway.AIMCPRoutePolicy{}
			p.Namespace, p.Name = "tools", "mcp"
			p.Spec.TargetRef.Name = "mcp-route"
			p.Spec.AuthRef = ref
			p.Spec.ToolAccess = &akogatewayapiaigateway.MCPToolAccess{
				Rules: []akogatewayapiaigateway.ToolAccessRule{{Role: "", Allow: []string{"*"}}}, // invalid: empty role
			}
			if p.Spec.Validate() == nil {
				t.Fatal("precondition: policy must be invalid")
			}
			vs := authRefTestVS()

			pass := akogatewayapiaigateway.BeginAuthGuardPass("key")
			ApplyMCPRoutePolicy("key", p, vs, "", "/", akogatewayapiaigateway.ClaimModeJWTHeader)
			akogatewayapiaigateway.EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})

			if !hasAuthGuard(vs) {
				t.Fatalf("invalid policy with an authRef must fail the VS closed; DataScripts: %d", len(vs.HTTPDSrefs))
			}
			if len(vs.HTTPDSrefs) != 1 {
				t.Errorf("the rest of an invalid policy must still be skipped: only the guard may be attached, got %d DataScripts",
					len(vs.HTTPDSrefs))
			}
		})
	}
}

// A valid MCP policy attaches AKO's own session scripts and leaves the VS on its
// ordinary HTTP profile: on Avi 32.1.3 an MCP-service-type profile makes the
// controller attach System-Standard-MCP, which 500s every Mcp-Session-Id request.
func TestApplyMCPRoutePolicyKeepsHTTPProfile(t *testing.T) {
	p := &akogatewayapiaigateway.AIMCPRoutePolicy{}
	p.Namespace, p.Name = "tools", "mcp"
	p.Spec.TargetRef.Name = "mcp-route"
	vs := authRefTestVS()
	vs.ApplicationProfile = "System-HTTP"
	ApplyMCPRoutePolicy("key", p, vs, "", "/", akogatewayapiaigateway.ClaimModeJWTHeader)
	if vs.ApplicationProfile != "System-HTTP" {
		t.Errorf("app profile changed to %q; an MCP-service-type profile brings the failing system script", vs.ApplicationProfile)
	}
	want := map[string]bool{
		akogatewayapiaigateway.DSMCPSessReqName(vs.Name):  false,
		akogatewayapiaigateway.DSMCPSessRespName(vs.Name): false,
	}
	for _, ds := range vs.HTTPDSrefs {
		if _, ok := want[ds.Name]; ok {
			want[ds.Name] = true
		}
		if ds.Name == akogatewayapiaigateway.MCPSessionDataScript {
			t.Errorf("the system %s DataScript must not be referenced", ds.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("session DataScript %s not attached", name)
		}
	}
}

// Without an authRef an invalid policy is skipped as before: no guard.
func TestApplyMCPRoutePolicyInvalidWithoutAuthRefSkipped(t *testing.T) {
	p := &akogatewayapiaigateway.AIMCPRoutePolicy{}
	p.Namespace, p.Name = "tools", "mcp"
	p.Spec.TargetRef.Name = "mcp-route"
	p.Spec.ToolAccess = &akogatewayapiaigateway.MCPToolAccess{
		Rules: []akogatewayapiaigateway.ToolAccessRule{{Role: "", Allow: []string{"*"}}},
	}
	vs := authRefTestVS()
	ApplyMCPRoutePolicy("key", p, vs, "", "/", akogatewayapiaigateway.ClaimModeJWTHeader)
	if hasAuthGuard(vs) || len(vs.HTTPDSrefs) != 0 {
		t.Fatalf("invalid policy without authRef must be skipped untouched, got %d DataScripts", len(vs.HTTPDSrefs))
	}
}

func TestApplyA2ARoutePolicyInvalidWithAuthRefFailsClosed(t *testing.T) {
	p := &akogatewayapiaigateway.AIA2ARoutePolicy{}
	p.Namespace, p.Name = "agents", "a2a"
	p.Spec.TargetRef.Name = "a2a-route"
	p.Spec.AuthRef = &akogatewayapiaigateway.AuthPolicyRef{Name: "agent-auth"}
	p.Spec.AgentAccess = &akogatewayapiaigateway.A2AAgentAccess{
		Rules: []akogatewayapiaigateway.AgentAccessRule{{Agent: "", Allow: []string{"tasks/send"}}}, // invalid: empty agent
	}
	if p.Spec.Validate() == nil {
		t.Fatal("precondition: policy must be invalid")
	}
	vs := authRefTestVS()

	pass := akogatewayapiaigateway.BeginAuthGuardPass("key")
	ApplyA2ARoutePolicy("key", p, vs, "", "/", akogatewayapiaigateway.ClaimModeJWTHeader)
	akogatewayapiaigateway.EndAuthGuardPass(pass, []nodes.AviVsEvhSniModel{vs})

	if !hasAuthGuard(vs) {
		t.Fatalf("invalid A2A policy with an authRef must fail the VS closed; DataScripts: %d", len(vs.HTTPDSrefs))
	}
	if len(vs.HTTPDSrefs) != 1 {
		t.Errorf("only the guard may be attached for an invalid policy, got %d DataScripts", len(vs.HTTPDSrefs))
	}
}

func TestApplyA2ARoutePolicyInvalidWithoutAuthRefSkipped(t *testing.T) {
	p := &akogatewayapiaigateway.AIA2ARoutePolicy{}
	p.Namespace, p.Name = "agents", "a2a"
	p.Spec.TargetRef.Name = "a2a-route"
	p.Spec.AgentAccess = &akogatewayapiaigateway.A2AAgentAccess{
		Rules: []akogatewayapiaigateway.AgentAccessRule{{Agent: "", Allow: []string{"tasks/send"}}},
	}
	vs := authRefTestVS()
	ApplyA2ARoutePolicy("key", p, vs, "", "/", akogatewayapiaigateway.ClaimModeJWTHeader)
	if len(vs.HTTPDSrefs) != 0 {
		t.Fatalf("invalid policy without authRef must be skipped untouched, got %d DataScripts", len(vs.HTTPDSrefs))
	}
}
