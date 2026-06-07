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
)

// DataScript-set name suffixes for the MCP tool-authorization scripts.
const (
	DSNameSuffixMCPReq     = "-ai-mcp-req"
	DSNameSuffixMCPReqData = "-ai-mcp-reqdata"
)

// DSMCPReqName / DSMCPReqDataName build the DataScript-set names for the MCP
// tool-authorization scripts on a given VS.
func DSMCPReqName(vsName string) string     { return vsName + DSNameSuffixMCPReq }
func DSMCPReqDataName(vsName string) string { return vsName + DSNameSuffixMCPReqData }

// MCPToolAuthScripts holds the two Lua snippets generated from an
// AIMCPRoutePolicy's toolAccess: the HTTP_REQ phase that enables request-body
// buffering and the HTTP_REQ_DATA phase that reads the JSON-RPC body, extracts
// the called tool, and authorizes it against the caller's verified role.
type MCPToolAuthScripts struct {
	// ReqScript is the HTTP_REQ phase Lua (enables request-body buffering). Empty
	// when the policy has no toolAccess.
	ReqScript string

	// ReqDataScript is the HTTP_REQ_DATA phase Lua (read body, gate tools/call).
	// Empty when the policy has no toolAccess.
	ReqDataScript string
}

// GenerateMCPToolAuthScripts produces the tool-authorization DataScript snippets
// for the policy. When toolAccess is absent the request is authorized by OAuth
// alone and no scripts are generated (both fields empty).
//
// The generated HTTP_REQ_DATA script gates only the JSON-RPC "tools/call" method
// — protocol plumbing (initialize, tools/list, ping, …) passes the OAuth check
// alone. The decision tables (ROLE_STAR / ALLOW / ALLOW_PFX) and ROLE_CLAIM are
// baked in as Lua constants, exactly as GenerateModelRouteScripts bakes its
// model→tier tables.
func GenerateMCPToolAuthScripts(policy *AIMCPRoutePolicy) MCPToolAuthScripts {
	spec := policy.Spec
	if spec.ToolAccess == nil || len(spec.ToolAccess.Rules) == 0 {
		return MCPToolAuthScripts{}
	}

	reqScript := fmt.Sprintf(
		"-- AKO AI Gateway: enable request-body buffering for MCP tool authorization\n"+
			"pcall(function() avi.http.set_request_body_buffer_size(%d) end)",
		ReqBodyBufferBytes)

	star, exact, prefix := classifyToolRules(spec.ToolAccess.Rules)

	var b strings.Builder
	b.WriteString("-- AKO AI Gateway: MCP per-role tool authorization (HTTP_REQ_DATA)\n")

	// ── Baked-in constant tables ─────────────────────────────────────────────
	b.WriteString(luaRoleSet("ROLE_STAR", star))
	b.WriteString(luaNestedBoolMap("ALLOW", exact))
	b.WriteString(luaListMap("ALLOW_PFX", prefix))
	fmt.Fprintf(&b, "local ROLE_CLAIM = %s\n", luaStr(spec.ToolAccess.EffectiveRoleClaim()))

	// ── Helpers (json_str + jwt_claim) ───────────────────────────────────────
	b.WriteString(jsonStrHelper())
	b.WriteString(jwtClaimHelper())
	b.WriteString("\n")

	// ── Read body + extract method and tool name ─────────────────────────────
	fmt.Fprintf(&b, `
local _ok, _body = pcall(function() return avi.http.get_req_body(%d) end)
if not _ok or type(_body) ~= "string" then _body = "" end
local method = json_str(_body, "method")
local tool = ""
do
  local _ps = string.find(_body, DQ .. "params" .. DQ, 1, true)
  if _ps then tool = json_str(string.sub(_body, _ps), "name") end
end
`, ReqBodyBufferBytes)

	// ── Authorize tools/call against the role's allow-list ───────────────────
	reject := mcpRejectStmt(spec.OnUnauthorized)
	b.WriteString(`
if method == "tools/call" then
  local _role = jwt_claim(ROLE_CLAIM)
  local _ok2 = false
  if ROLE_STAR[_role] then
    _ok2 = true
  else
    local _ex = ALLOW[_role]
    if _ex and _ex[tool] then _ok2 = true end
    if not _ok2 then
      local _pf = ALLOW_PFX[_role]
      if _pf then
        for i = 1, #_pf do
          if string.sub(tool, 1, string.len(_pf[i])) == _pf[i] then _ok2 = true break end
        end
      end
    end
  end
  if tool == "" then _ok2 = false end
  if not _ok2 then
`)
	b.WriteString(reject)
	b.WriteString(`
  end
end`)

	return MCPToolAuthScripts{ReqScript: reqScript, ReqDataScript: b.String()}
}

// mcpRejectStmt returns the Lua executed when a tool call is unauthorized:
// either an immediate JSON-RPC error response (Reject) or a non-blocking tag
// that lets the call through for observation (Log).
func mcpRejectStmt(a *UnauthorizedAction) string {
	if a.EffectiveType() == "Log" {
		return `    pcall(function() avi.http.add_header("X-MCP-Tool-Denied", tool) end)`
	}
	return fmt.Sprintf(
		`    avi.http.response(%d, {["Content-Type"]="application/json"}, `+
			`'{"jsonrpc":"2.0","error":{"code":-32001,"message":"tool_not_authorized"}}') return`,
		a.EffectiveStatusCode())
}

// classifyToolRules splits the allow-lists into three baked tables: roles allowed
// every tool ("*"), per-role exact tool sets, and per-role trailing-"*" prefixes.
func classifyToolRules(rules []ToolAccessRule) (star map[string]bool, exact map[string]map[string]bool, prefix map[string][]string) {
	star = map[string]bool{}
	exact = map[string]map[string]bool{}
	prefix = map[string][]string{}
	for _, r := range rules {
		for _, a := range r.Allow {
			switch {
			case a == "*":
				star[r.Role] = true
			case strings.HasSuffix(a, "*"):
				prefix[r.Role] = append(prefix[r.Role], strings.TrimSuffix(a, "*"))
			default:
				if exact[r.Role] == nil {
					exact[r.Role] = map[string]bool{}
				}
				exact[r.Role][a] = true
			}
		}
	}
	return star, exact, prefix
}

// ─── Lua literal builders (MCP-specific; deterministic ordering) ──────────────

// luaRoleSet renders `local <name> = { ["role"]=true, ... }`.
func luaRoleSet(name string, set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("[%s]=true", luaStr(k)))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(parts, ", "))
}

// luaNestedBoolMap renders `local <name> = { ["role"]={ ["tool"]=true, ... }, ... }`.
func luaNestedBoolMap(name string, m map[string]map[string]bool) string {
	roles := make([]string, 0, len(m))
	for r := range m {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	var roleParts []string
	for _, r := range roles {
		tools := make([]string, 0, len(m[r]))
		for t := range m[r] {
			tools = append(tools, t)
		}
		sort.Strings(tools)
		var toolParts []string
		for _, t := range tools {
			toolParts = append(toolParts, fmt.Sprintf("[%s]=true", luaStr(t)))
		}
		roleParts = append(roleParts, fmt.Sprintf("[%s]={ %s }", luaStr(r), strings.Join(toolParts, ", ")))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(roleParts, ", "))
}

// luaListMap renders `local <name> = { ["role"]={ "pfx", ... }, ... }`.
func luaListMap(name string, m map[string][]string) string {
	roles := make([]string, 0, len(m))
	for r := range m {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	var roleParts []string
	for _, r := range roles {
		items := append([]string(nil), m[r]...)
		sort.Strings(items)
		var its []string
		for _, it := range items {
			its = append(its, luaStr(it))
		}
		roleParts = append(roleParts, fmt.Sprintf("[%s]={ %s }", luaStr(r), strings.Join(its, ", ")))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(roleParts, ", "))
}
