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

// DataScript-set name suffixes for the A2A scripts.
const (
	DSNameSuffixA2AReq     = "-ai-a2a-req"
	DSNameSuffixA2AReqData = "-ai-a2a-reqdata"
	DSNameSuffixA2AResp    = "-ai-a2a-resp"
	DSNameSuffixA2ARespData = "-ai-a2a-respdata"
)

func DSA2AReqName(vsName string) string      { return vsName + DSNameSuffixA2AReq }
func DSA2AReqDataName(vsName string) string  { return vsName + DSNameSuffixA2AReqData }
func DSA2ARespName(vsName string) string     { return vsName + DSNameSuffixA2AResp }
func DSA2ARespDataName(vsName string) string { return vsName + DSNameSuffixA2ARespData }

// A2AScripts holds all four Lua snippets generated from an AIA2ARoutePolicy.
type A2AScripts struct {
	// ReqScript buffers the request body and re-pins known task IDs.
	ReqScript string

	// ReqDataScript reads the buffered body, enforces agent RBAC, and for
	// follow-up task methods looks up params.id to pin the backend.
	ReqDataScript string

	// RespScript enables response-body buffering on tasks/send responses so
	// the RespDataScript can capture the new task ID.
	RespScript string

	// RespDataScript reads the buffered response body on tasks/send, extracts
	// result.id, and stores the task ID → backend mapping in the VS table.
	RespDataScript string
}

// A2ATaskAffinityTTLSeconds converts the human-readable timeout string from an
// A2ATaskAffinity into the integer seconds passed to avi.vs.table_insert.
// Supports "Nm" (minutes) and "Nh" (hours); anything else falls back to 1800.
func A2ATaskAffinityTTLSeconds(timeout string) int {
	if timeout == "" {
		return 1800
	}
	var n int
	switch {
	case strings.HasSuffix(timeout, "h"):
		fmt.Sscanf(strings.TrimSuffix(timeout, "h"), "%d", &n)
		return n * 3600
	case strings.HasSuffix(timeout, "m"):
		fmt.Sscanf(strings.TrimSuffix(timeout, "m"), "%d", &n)
		return n * 60
	default:
		fmt.Sscanf(timeout, "%d", &n)
		if n > 0 {
			return n
		}
		return 1800
	}
}

// GenerateA2AScripts produces all four DataScript snippets for the policy.
// When taskAffinity and agentAccess are both absent the scripts are still
// generated (agent card path detection still applies).
func GenerateA2AScripts(policy *AIA2ARoutePolicy, mode AuthClaimMode) A2AScripts {
	spec := policy.Spec
	ttl := A2ATaskAffinityTTLSeconds(spec.TaskAffinity.EffectiveTimeout())

	// ── HTTP_REQ ──────────────────────────────────────────────────────────────
	// Buffer the request body and, for follow-up task calls that already carry
	// a known params.id, re-pin the request to the backend that owns the task.
	req := fmt.Sprintf(`-- AKO AI Gateway: A2A request phase — buffer body + task affinity re-pin
do
  -- Pass agent card discovery through without body inspection.
  local path = ""
  do local ok, p = pcall(avi.http.get_path); if ok then path = p end end
  if path == "/.well-known/agent.json" then return end

  -- Buffer body so HTTP_REQ_DATA can read it.
  pcall(function() avi.http.set_request_body_buffer_size(%d) end)
end`, ReqBodyBufferBytes)

	// ── HTTP_REQ_DATA ────────────────────────────────────────────────────────
	// Read the buffered body: extract method + params.id.
	// 1. For follow-up methods (tasks/get, tasks/cancel, tasks/resubscribe):
	//    look up params.id in the VS table and pin the backend.
	// 2. Enforce agent RBAC on the extracted method.
	var reqData strings.Builder
	reqData.WriteString("-- AKO AI Gateway: A2A request-data phase — task affinity + agent RBAC\n")
	reqData.WriteString("do\n")
	reqData.WriteString("  local path = \"\"\n")
	reqData.WriteString("  do local ok, p = pcall(avi.http.get_path); if ok then path = p end end\n")
	reqData.WriteString("  if path == \"/.well-known/agent.json\" then return end\n\n")

	// Baked agent RBAC tables (empty when no agentAccess configured).
	if spec.AgentAccess != nil && len(spec.AgentAccess.Rules) > 0 {
		agentStar, agentExact, agentPrefix := classifyAgentRules(spec.AgentAccess.Rules)
		reqData.WriteString(luaRoleSet("AGENT_STAR", agentStar))
		reqData.WriteString(luaNestedBoolMap("ALLOW", agentExact))
		reqData.WriteString(luaListMap("ALLOW_PFX", agentPrefix))
		fmt.Fprintf(&reqData, "  local AGENT_CLAIM = %s\n", luaStr(spec.AgentAccess.EffectiveAgentClaim()))
		fmt.Fprintf(&reqData, "  local SKILL_CLAIM = %s\n", luaStr(spec.AgentAccess.EffectiveSkillClaim()))
		if spec.AgentAccess.TargetAgent != "" {
			fmt.Fprintf(&reqData, "  local EXPECTED_TARGET = %s\n", luaStr(spec.AgentAccess.TargetAgent))
		}
		reqData.WriteString(jwtClaimHelper(mode))
	}

	reqData.WriteString(jsonStrHelper())
	fmt.Fprintf(&reqData, `
  local _ok, _body = pcall(function() return avi.http.get_req_body(%d) end)
  if not _ok or type(_body) ~= "string" then _body = "" end
  local method = json_str(_body, "method")

  -- Extract params.id for task affinity on follow-up calls.
  local task_id = ""
  local FOLLOWUP = {["tasks/get"]=true, ["tasks/cancel"]=true, ["tasks/resubscribe"]=true}
  if FOLLOWUP[method] then
    local _ps = string.find(_body, "\"params\"", 1, true)
    if _ps then task_id = json_str(string.sub(_body, _ps), "id") end
    if task_id ~= "" then
      local pool_name = avi.vs.table_lookup("a2a_pool", task_id, %d)
      local server_ip = avi.vs.table_lookup("a2a_srv", task_id, %d)
      if pool_name and server_ip then
        pcall(avi.pool.select, pool_name, server_ip)
      end
    end
  end
`, ReqBodyBufferBytes, ttl, ttl)

	// Agent RBAC enforcement — only when agentAccess is configured.
	if spec.AgentAccess != nil && len(spec.AgentAccess.Rules) > 0 {
		reject := a2aRejectStmt(spec.OnUnauthorized)

		// Target binding: refuse a token minted for a different agent. Checked
		// before the allow-list, so a misdirected token is rejected on the
		// strongest available ground rather than on whatever its skill happens
		// to be. An absent claim is never rejected (legacy callers).
		if spec.AgentAccess.TargetAgent != "" {
			reqData.WriteString(`
  do
    local _tgt = jwt_claim("target")
    if _tgt ~= "" and _tgt ~= EXPECTED_TARGET then
`)
			reqData.WriteString(reject)
			reqData.WriteString(`
    end
  end
`)
		}

		// With path authorization the rules are evaluated for every request, not
		// just JSON-RPC ones — that is the whole point, since a REST call has no
		// method to gate on.
		guard := `
  if method ~= "" then`
		if spec.AgentAccess.AuthorizePaths {
			guard = `
  do`
		}
		reqData.WriteString(guard + `
    local _agent = jwt_claim(AGENT_CLAIM)
    -- The skill the caller says it is invoking, from its per-target token. A
    -- legacy token carries no skill, leaving this empty; the empty string is
    -- never tested, so method-only policies behave exactly as before.
    local _skill = jwt_claim(SKILL_CLAIM)
    local _ok2 = false
    if AGENT_STAR[_agent] then
      _ok2 = true
    else
      local _ex = ALLOW[_agent]
      if _ex and method ~= "" and _ex[method] then _ok2 = true end
      if not _ok2 and _skill ~= "" and _ex and _ex[_skill] then _ok2 = true end` +
			pathExactMatch(spec.AgentAccess.AuthorizePaths) + `
      if not _ok2 then
        local _pf = ALLOW_PFX[_agent]
        if _pf then
          for i = 1, #_pf do
            local _p = _pf[i]
            if method ~= "" and string.sub(method, 1, string.len(_p)) == _p then _ok2 = true break end
            if _skill ~= "" and string.sub(_skill, 1, string.len(_p)) == _p then _ok2 = true break end` +
			pathPrefixMatch(spec.AgentAccess.AuthorizePaths) + `
          end
        end
      end
    end
    if not _ok2 then
`)
		reqData.WriteString(reject)
		reqData.WriteString(`
    end
  end
`)
		// Without a method there is nothing to authorize against, so the block
		// above is skipped entirely — fail-open for any non-JSON-RPC request.
		// requireMethod closes that for agents that speak only JSON-RPC.
		if spec.AgentAccess.RequireMethod {
			reqData.WriteString(`
  if method == "" then
`)
			reqData.WriteString(reject)
			reqData.WriteString(`
  end
`)
		}
	}
	reqData.WriteString("end")

	// ── HTTP_RESP ────────────────────────────────────────────────────────────
	// Enable response-body buffering only on tasks/send responses, so
	// HTTP_RESP_DATA can extract the new task ID from result.id.
	resp := fmt.Sprintf(`-- AKO AI Gateway: A2A response phase — enable body buffering for tasks/send
do
  -- Only tasks/send creates a new task whose ID we need to capture.
  -- Reuse the reqvar set in HTTP_REQ_DATA to avoid re-parsing the body here.
  local method = ""
  do local ok, v = pcall(avi.http.get_reqvar, "a2a_method"); if ok and v then method = v end end
  if method == "tasks/send" or method == "tasks/sendSubscribe" then
    pcall(function() avi.http.set_response_body_buffer_size(%d) end)
  end
end`, RespBodyBufferKB*1024)

	// ── HTTP_RESP_DATA ───────────────────────────────────────────────────────
	// Read the buffered tasks/send response, extract result.id, store in VS
	// table so follow-up calls can be re-pinned.
	respData := fmt.Sprintf(`-- AKO AI Gateway: A2A response-data phase — capture task ID → backend
do
  local method = ""
  do local ok, v = pcall(avi.http.get_reqvar, "a2a_method"); if ok and v then method = v end end
  if method ~= "tasks/send" and method ~= "tasks/sendSubscribe" then return end

  local _ok, _body = pcall(function() return avi.http.get_response_body(%d) end)
  if not _ok or type(_body) ~= "string" or _body == "" then return end

  -- Extract result.id from the JSON-RPC response.
  local task_id = ""
  local _rs = string.find(_body, "\"result\"", 1, true)
  if _rs then task_id = json_str(string.sub(_body, _rs), "id") end
  if task_id == "" then return end

  local pool_name, server_ip
  do local ok, v = pcall(avi.pool.name); if ok then pool_name = v end end
  do local ok, v = pcall(avi.pool.server_ip); if ok then server_ip = v end end
  if pool_name and server_ip then
    pcall(avi.vs.table_remove, "a2a_pool", task_id)
    pcall(avi.vs.table_remove, "a2a_srv", task_id)
    pcall(avi.vs.table_insert, "a2a_pool", task_id, pool_name, %d)
    pcall(avi.vs.table_insert, "a2a_srv", task_id, server_ip, %d)
  end
end`, RespBodyBufferKB*1024, ttl, ttl)

	// Store the A2A method in a reqvar so the response phase can read it
	// without re-buffering the request. Append to reqData after the main body.
	reqDataFinal := reqData.String() + fmt.Sprintf(`

-- Stash method for the response phase.
do
  if method ~= "" then
    pcall(avi.http.set_reqvar, "a2a_method", method)
  end
end`)

	// Re-inject json_str into respData (it runs in a separate DataScript context).
	respData = jsonStrHelper() + "\n" + respData

	return A2AScripts{
		ReqScript:      req,
		ReqDataScript:  reqDataFinal,
		RespScript:     resp,
		RespDataScript: respData,
	}
}

// a2aRejectStmt returns the Lua executed when an agent call is unauthorized.
func a2aRejectStmt(a *UnauthorizedAction) string {
	if a.EffectiveType() == "Log" {
		return `      pcall(function() avi.http.add_header("X-A2A-Method-Denied", method) end)`
	}
	return fmt.Sprintf(
		`      avi.http.response(%d, {["Content-Type"]="application/json"}, `+
			`'{"jsonrpc":"2.0","error":{"code":-32001,"message":"method_not_authorized"}}') return`,
		a.EffectiveStatusCode())
}

// pathExactMatch / pathPrefixMatch add the request path as a matched dimension
// when authorizePaths is set. `path` is already in scope — it is read at the top
// of the request-data block for the agent-card exemption — and is never empty,
// so unlike method and skill it needs no emptiness guard.
func pathExactMatch(on bool) string {
	if !on {
		return ""
	}
	return `
      if not _ok2 and _ex and _ex[path] then _ok2 = true end`
}

func pathPrefixMatch(on bool) string {
	if !on {
		return ""
	}
	return `
            if string.sub(path, 1, string.len(_p)) == _p then _ok2 = true break end`
}

// classifyAgentRules splits agent allow-lists into the same three baked tables
// used by the MCP tool-auth scripts: wildcard agents, exact method sets, and
// prefix globs.
func classifyAgentRules(rules []AgentAccessRule) (star map[string]bool, exact map[string]map[string]bool, prefix map[string][]string) {
	star = map[string]bool{}
	exact = map[string]map[string]bool{}
	prefix = map[string][]string{}
	for _, r := range rules {
		for _, a := range r.Allow {
			switch {
			case a == "*":
				star[r.Agent] = true
			case strings.HasSuffix(a, "*"):
				prefix[r.Agent] = append(prefix[r.Agent], strings.TrimSuffix(a, "*"))
			default:
				if exact[r.Agent] == nil {
					exact[r.Agent] = map[string]bool{}
				}
				exact[r.Agent][a] = true
			}
		}
	}
	// Sort prefix slices for deterministic output.
	for k := range prefix {
		sort.Strings(prefix[k])
	}
	return star, exact, prefix
}
