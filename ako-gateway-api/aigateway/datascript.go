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
)

// DataScript event types used by Avi SE DataScripts.
const (
	DSEvtHTTPReq      = "VS_DATASCRIPT_EVT_HTTP_REQ"
	DSEvtHTTPResp     = "VS_DATASCRIPT_EVT_HTTP_RESP"
	DSEvtHTTPRespData = "VS_DATASCRIPT_EVT_HTTP_RESP_DATA"
)

// DSTableName is the Avi SE table used to store per-window token counters.
// The table is shared state across connections on the same SE.
const DSTableName = "ai_tok"

// RespBodyBufferKB is how much of the response body the SE buffers so the
// HTTP_RESP_DATA script can read the OpenAI `usage` block (which sits at the end
// of the JSON). `usage` is only reachable if the whole body fits, so size this to
// your largest expected completion (~max_tokens * 6 bytes). 256 KB covers
// ~40K output tokens; raise it for long-form generation (cost is SE memory per
// in-flight buffered response).
const RespBodyBufferKB = 256

// FailClosedTokens is the conservative token amount charged when a buffered JSON
// completion has no parseable `usage` — i.e. the body was truncated (larger than
// RespBodyBufferKB) or compressed. Charging a large penalty (instead of 0) keeps
// an over-buffer or unparseable response from slipping through the budget
// unmetered; the response itself already reached the client, so this consumes the
// consumer's budget so their *next* request is blocked. Derived from the buffer
// size (~RespBodyBufferKB*1024/4 tokens) — a response that didn't fit must have
// been at least this large.
const FailClosedTokens = RespBodyBufferKB * 1024 / 4

// DSNameReq / DSNameResp / DSNameRespData are the suffixes appended to the VS
// name to generate DataScript set names.  One DataScript set is created per
// event phase.
const (
	DSNameSuffixReq      = "-ai-tok-req"
	DSNameSuffixResp     = "-ai-tok-resp"
	DSNameSuffixRespData = "-ai-tok-respdata"
)

// TokenAccountingScripts holds the Lua snippets generated from an
// AITokenRateLimitPolicy: request-phase enforcement, a response-header-phase
// buffer-enable, and response-body-phase accounting.
type TokenAccountingScripts struct {
	// ReqScript is the HTTP_REQ phase Lua code.  It looks up per-identity token
	// counters and rejects the request when any limit is already exceeded.
	ReqScript string

	// RespScript is the HTTP_RESP phase Lua code.  Response headers are parsed
	// here, but the body is not yet available — so this script only enables
	// response-body buffering (avi.http.set_response_body_buffer_size) so the
	// HTTP_RESP_DATA phase can read it.
	RespScript string

	// RespDataScript is the HTTP_RESP_DATA phase Lua code.  The buffered response
	// body is available here (avi.http.get_response_body), so it parses the
	// OpenAI-compatible `usage` block directly from the JSON body and increments
	// every applicable counter — no dependency on the backend emitting token
	// headers.
	RespDataScript string
}

// GenerateTokenAccountingScripts produces the two Lua DataScript snippets that
// implement reactive token-budget enforcement for the given policy.
//
// Architecture (design doc §5):
//   - Request phase: reject if any counter already ≥ budget for the current window.
//   - Response phase: parse `usage.total_tokens` (or prompt/completion as configured)
//     from the response body and increment counters with a TTL equal to the window.
//   - Cross-SE consistency: each SE maintains independent per-VS shared state;
//     the resulting bounded overage is acceptable for quota-style limits.  Use
//     the native Avi rate limiter (requestRateLimit) for exact enforcement.
func GenerateTokenAccountingScripts(policy *AITokenRateLimitPolicy) TokenAccountingScripts {
	spec := policy.Spec

	identityHeader := spec.EffectiveIdentityHeader()
	fallback := "clientIP"
	if spec.IdentitySource != nil && spec.IdentitySource.Fallback != "" {
		fallback = spec.IdentitySource.Fallback
	}

	var reqParts, respParts, respDataParts []string

	// ── Shared header: resolve identity ────────────────────────────────────
	identityBlock := buildIdentityBlock(identityHeader, fallback)

	// jwt_claim helper is shared by both phases (identity + group extraction).
	helper := jwtClaimHelper()

	// ── Request-phase: enforce limits ─────────────────────────────────────
	reqParts = append(reqParts, "-- AKO AI Gateway: token-budget enforcement")
	reqParts = append(reqParts, helper)
	reqParts = append(reqParts, identityBlock)
	reqParts = append(reqParts, "local now = os.time()")

	for _, limit := range spec.Limits {
		reqParts = append(reqParts, buildReqLimitBlock(limit))
	}

	// ── Response-header phase: enable body buffering ──────────────────────
	// The body is not available in HTTP_RESP; this only turns on buffering (for
	// POST responses with a JSON content-type) so HTTP_RESP_DATA can read it.
	respParts = append(respParts, buildBufferEnableBlock())

	// ── Response-body phase: account for tokens ───────────────────────────
	respDataParts = append(respDataParts, "-- AKO AI Gateway: token-usage accounting (from response body)")
	respDataParts = append(respDataParts, helper)
	respDataParts = append(respDataParts, identityBlock)
	respDataParts = append(respDataParts, "local now = os.time()")
	respDataParts = append(respDataParts, buildUsageParseBlock())

	for _, limit := range spec.Limits {
		respDataParts = append(respDataParts, buildRespLimitBlock(limit))
	}

	return TokenAccountingScripts{
		ReqScript:      strings.Join(reqParts, "\n"),
		RespScript:     strings.Join(respParts, "\n"),
		RespDataScript: strings.Join(respDataParts, "\n"),
	}
}

// jwtClaimHelper returns a Lua function `jwt_claim(claim)` that returns a string
// claim from the OAuth-validated access token. With the OAuth resource-server flow
// the Avi SE validates the bearer JWT and exposes its claims to the DataScript via
// avi.http.oauth_get_claim(). The provider index 0 selects the first (only)
// oauth_settings entry on the VS. Every call is pcall-guarded so a missing claim or
// an unauthenticated request yields "" rather than raising in the SE sandbox.
func jwtClaimHelper() string {
	return `-- AKO AI Gateway: read a claim from the OAuth-validated access token.
-- avi.http.oauth_get_claim(provider_index, claim) returns the claim value; for
-- this Avi build it comes back as a Lua table (claims may be multi-valued), so
-- unwrap the first scalar. Provider index 0 selects the only oauth_settings entry.
local function jwt_claim(claim)
  local ok, v = pcall(avi.http.oauth_get_claim, 0, claim)
  if not ok or v == nil then return "" end
  if type(v) == "table" then
    if v[1] ~= nil then return tostring(v[1]) end
    for _, val in pairs(v) do return tostring(val) end
    return ""
  end
  return tostring(v)
end`
}

// buildIdentityBlock returns the Lua snippet that resolves the consumer identity
// into the local variable `identity`.
func buildIdentityBlock(header, fallback string) string {
	var b strings.Builder
	// Prefer the validated JWT subject claim (decoded from the bearer token),
	// then the forwarded identity header, then the configured fallback.
	b.WriteString("local _ok_id, _id = pcall(jwt_claim, \"sub\")\n")
	b.WriteString("local identity = (_ok_id and _id) or \"\"\n")
	fmt.Fprintf(&b, "if identity == \"\" then identity = avi.http.get_header(%q, avi.HTTP_REQUEST) or \"\" end\n", header)
	if fallback == "clientIP" {
		b.WriteString(`if not identity or identity == "" then
  identity = avi.vs.client_ip()
end`)
	} else {
		// fallback == "reject"
		b.WriteString(`if not identity or identity == "" then
  avi.http.response(401, {["Content-Type"] = "application/json"},
    '{"error":"missing_identity_header"}')
  return
end`)
	}
	return b.String()
}

// windowSeconds converts a window string (e.g. "1m", "24h", "7d") to seconds.
// Returns 60 for unrecognised formats.
func windowSeconds(w string) int64 {
	if len(w) < 2 {
		return 60
	}
	suffix := w[len(w)-1:]
	numStr := w[:len(w)-1]
	var n int64
	fmt.Sscanf(numStr, "%d", &n)
	if n <= 0 {
		return 60
	}
	switch suffix {
	case "s":
		return n
	case "m":
		return n * 60
	case "h":
		return n * 3600
	case "d":
		return n * 86400
	}
	return 60
}

// counterKey returns a unique Lua expression for the counter table key that
// incorporates the limit name, identity dimension, and current window boundary.
func counterKeyExpr(limit TokenLimit) string {
	windowSec := windowSeconds(limit.Window)
	keyExpr := counterKeyIdentityExpr(limit.Key)
	return fmt.Sprintf(`%q..":"..%s..":"..math.floor(now/%d)*%d`,
		limit.Name, keyExpr, windowSec, windowSec)
}

// counterKeyIdentityExpr returns the Lua expression that evaluates to the key
// dimension value for a given limit.
func counterKeyIdentityExpr(key string) string {
	switch {
	case key == "consumer":
		return "identity"
	case key == "clientIP":
		return "avi.vs.client_ip()"
	case strings.HasPrefix(key, "header:"):
		headerName := strings.TrimPrefix(key, "header:")
		return fmt.Sprintf(`(avi.http.get_header(%q) or "unknown")`, headerName)
	default:
		return "identity"
	}
}

// tokenDimensionExpr returns the Lua variable name carrying the token count for
// the given dimension ("prompt", "completion", or "total").
func tokenDimensionExpr(tokens string) string {
	switch tokens {
	case "prompt":
		return "prompt_tokens"
	case "completion":
		return "completion_tokens"
	default:
		return "total_tokens"
	}
}

// buildReqLimitBlock generates the Lua snippet that enforces one token limit in
// the HTTP_REQ phase: look up the counter and reject if already at budget.
// When limit.GroupHeader is set it emits a per-group budget table so each group
// gets its own ceiling while the counter is still keyed per-consumer.
func buildReqLimitBlock(limit TokenLimit) string {
	windowSec := windowSeconds(limit.Window)
	keyExpr := counterKeyExpr(limit)
	action := limit.Action

	statusCode := 429
	doRetryAfter := false
	actionType := "Reject"
	if action != nil {
		if action.StatusCode >= 400 {
			statusCode = action.StatusCode
		}
		if action.RetryAfter {
			doRetryAfter = true
		}
		if action.Type != "" {
			actionType = action.Type
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "-- limit: %s", limit.Name)
	if limit.GroupHeader != "" {
		fmt.Fprintf(&b, " (group-based budget via header '%s')", limit.GroupHeader)
	} else {
		fmt.Fprintf(&b, " (budget %d / %s)", limit.Budget, limit.Window)
	}
	fmt.Fprintf(&b, "\ndo\n")
	fmt.Fprintf(&b, "  local k = %s\n", keyExpr)
	fmt.Fprintf(&b, "  local cur = tonumber(avi.vs.table_lookup(k) or 0)\n")

	if limit.GroupHeader != "" && len(limit.GroupBudgets) > 0 {
		// Per-group budget: read the group from the OAuth-validated access token.
		fmt.Fprintf(&b, "  local _ok_g, _g = pcall(jwt_claim, %q)\n", limit.GroupHeader)
		fmt.Fprintf(&b, "  local group_hdr = (_ok_g and _g) or \"\"\n")
		fmt.Fprintf(&b, "  local group_budgets = {")
		first := true
		for g, budget := range limit.GroupBudgets {
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "[%q]=%d", g, budget)
			first = false
		}
		fmt.Fprintf(&b, "}\n")
		fmt.Fprintf(&b, "  local budget = group_budgets[group_hdr]\n")
		// Unknown group: use fallback Budget (0 = deny, >0 = allow with fallback limit)
		if limit.Budget > 0 {
			fmt.Fprintf(&b, "  if not budget then budget = %d end\n", limit.Budget)
		} else {
			fmt.Fprintf(&b, "  if not budget then\n")
			fmt.Fprintf(&b, "    avi.http.response(403, {[\"Content-Type\"]=\"application/json\"},\n")
			fmt.Fprintf(&b, "      '{\"error\":\"unknown_group\",\"group\":\"'..group_hdr..'\"}')\n")
			fmt.Fprintf(&b, "    return\n  end\n")
		}
		fmt.Fprintf(&b, "  if cur >= budget then\n")
	} else {
		fmt.Fprintf(&b, "  if cur >= %d then\n", limit.Budget)
	}

	switch actionType {
	case "Log":
		fmt.Fprintf(&b, "    avi.vs.log(string.format(\"ai-gateway: limit %s exceeded (cur=%%s)\", tostring(cur)))\n",
			limit.Name)
	default:
		headers := `{["Content-Type"] = "application/json"`
		if doRetryAfter {
			fmt.Fprintf(&b, "    local retry_at = (math.floor(now/%d)+1)*%d - now\n", windowSec, windowSec)
			headers += `, ["Retry-After"] = tostring(math.max(1, retry_at))`
		}
		headers += "}"
		budgetExpr := fmt.Sprintf("%d", limit.Budget)
		if limit.GroupHeader != "" && len(limit.GroupBudgets) > 0 {
			budgetExpr = "budget"
		}
		fmt.Fprintf(&b, "    avi.http.response(%d, %s,\n", statusCode, headers)
		fmt.Fprintf(&b, "      string.format('{\"error\":\"token_budget_exceeded\",\"limit\":%q,\"current\":%%d,\"budget\":%%d}', cur, %s))\n",
			limit.Name, budgetExpr)
		fmt.Fprintf(&b, "    return\n")
	}

	fmt.Fprintf(&b, "  end\n")
	fmt.Fprintf(&b, "end\n")
	return b.String()
}

// buildBufferEnableBlock generates the HTTP_RESP Lua that decides whether a
// response should be metered and, if so, enables response-body buffering and
// records the decision in a request-scoped variable (ai_meter) for HTTP_RESP_DATA
// to read. A response is metered only when it is a successful (2xx) POST with a
// JSON content-type — so GET /metrics scrapes, health checks, streaming
// text/event-stream responses, and error responses are all skipped (the SE never
// buffers/scans a body that carries no usage, and errors aren't penalized).
// get_method and status are pcall-guarded, so the gate degrades gracefully if a
// function is unavailable in this event.
//
// HTTP_RESP_DATA can't read headers, so the ai_meter reqvar is how that phase
// learns "this response is metered" without re-sniffing the body.
func buildBufferEnableBlock() string {
	return fmt.Sprintf(`-- AKO AI Gateway: decide+flag a metered response, buffer its body for HTTP_RESP_DATA
do
  local ct = avi.http.get_header("Content-Type") or ""
  local is_json = string.find(ct, "application/json", 1, true) ~= nil
  local is_post = true
  do local ok, m = pcall(avi.http.get_method); if ok and m then is_post = (m == "POST") end end
  local is_ok = true
  do local ok, s = pcall(avi.http.status); if ok then local n = tonumber(s); if n then is_ok = (n >= 200 and n < 300) end end end
  if is_json and is_post and is_ok then
    avi.http.set_response_body_buffer_size(%d)
    avi.http.set_reqvar("ai_meter", "1")
  end
end`, RespBodyBufferKB)
}

// buildUsageParseBlock generates the Lua snippet that extracts prompt_tokens,
// completion_tokens, and total_tokens from the OpenAI-compatible `usage` block in
// the buffered response body. It runs in the HTTP_RESP_DATA event and only acts on
// responses HTTP_RESP flagged as metered (the ai_meter reqvar) — so it never reads
// the body for non-metered responses and the fail-closed decision is exact rather
// than a body-content guess. No dependency on the backend emitting token headers,
// so it works with stock vLLM.
//
// Avi's Lua sandbox lacks string.match, so the numeric value after each quoted
// JSON key is extracted with plain string.find + a string.byte digit scan.
func buildUsageParseBlock() string {
	return fmt.Sprintf(`-- token usage parsed from the buffered response body (HTTP_RESP_DATA)
local prompt_tokens     = 0
local completion_tokens = 0
local total_tokens      = 0
if avi.http.get_reqvar("ai_meter") == "1" then
  local _body = avi.http.get_response_body(%d)
  local function _num_after(key)
    if not _body then return 0 end
    local s = string.find(_body, key, 1, true)
    if not s then return 0 end
    local i = s + #key
    while i <= #_body do local c = string.byte(_body, i); if c >= 48 and c <= 57 then break end; i = i + 1 end
    local j = i
    while j <= #_body do local c = string.byte(_body, j); if c < 48 or c > 57 then break end; j = j + 1 end
    if j > i then return tonumber(string.sub(_body, i, j - 1)) or 0 end
    return 0
  end
  prompt_tokens     = _num_after('"prompt_tokens"')
  completion_tokens = _num_after('"completion_tokens"')
  total_tokens      = _num_after('"total_tokens"')
  if total_tokens == 0 then total_tokens = prompt_tokens + completion_tokens end
  -- Fail-closed: HTTP_RESP flagged this as a metered (2xx JSON POST) response, so
  -- if its usage didn't parse the body was truncated (larger than the buffer) or
  -- compressed. Charge a penalty so it can't slip through the budget unmetered.
  if total_tokens == 0 then
    total_tokens      = %d
    prompt_tokens     = %d
    completion_tokens = %d
  end
end`, RespBodyBufferKB, FailClosedTokens, FailClosedTokens, FailClosedTokens)
}

// buildRespLimitBlock generates the Lua snippet that increments one counter in
// the HTTP_RESP phase, with a TTL equal to the window length.
func buildRespLimitBlock(limit TokenLimit) string {
	windowSec := windowSeconds(limit.Window)
	keyExpr := counterKeyExpr(limit)
	dimVar := tokenDimensionExpr(limit.Tokens)

	var b strings.Builder
	fmt.Fprintf(&b, "-- account: %s\n", limit.Name)
	fmt.Fprintf(&b, "do\n")
	fmt.Fprintf(&b, "  local k = %s\n", keyExpr)
	fmt.Fprintf(&b, "  local cur = tonumber(avi.vs.table_lookup(k) or 0)\n")
	fmt.Fprintf(&b, "  local ttl = (%d - (now %% %d)) + 5\n", windowSec, windowSec)
	// avi.vs.table_insert does NOT overwrite an existing key — it is a true insert.
	// Remove the old entry first so the updated counter always gets written.
	fmt.Fprintf(&b, "  avi.vs.table_remove(k)\n")
	fmt.Fprintf(&b, "  avi.vs.table_insert(k, tostring(cur + %s), math.max(1, ttl))\n",
		dimVar)
	fmt.Fprintf(&b, "end\n")
	return b.String()
}

// DSNameForVS returns the DataScript set names for a given VS name.
func DSReqName(vsName string) string      { return vsName + DSNameSuffixReq }
func DSRespName(vsName string) string     { return vsName + DSNameSuffixResp }
func DSRespDataName(vsName string) string { return vsName + DSNameSuffixRespData }
