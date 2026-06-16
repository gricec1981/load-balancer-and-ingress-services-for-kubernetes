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

// RespBodyBufferBytes is the same size in bytes, and exists because the two units
// meet in one comparison.
//
// avi.http.set_response_body_buffer_size and avi.http.get_response_body both take
// KILOBYTES — which is why RespBodyBufferKB is passed to them raw. But `#_body` in
// Lua is a BYTE count, so the truncation test ("did the body fill the buffer?")
// has to be made against bytes. Getting that comparison wrong in either direction
// is silent: too high and the fail-closed penalty never fires, so an over-buffer
// completion goes unmetered; too low and every ordinary response looks truncated.
const RespBodyBufferBytes = RespBodyBufferKB * 1024

// FailClosedTokens is the conservative token amount charged when a response was
// demonstrably UNREADABLE rather than merely usage-free: the body filled the
// buffer (so `usage` sat past RespBodyBufferKB) or it arrived compressed. Charging
// a large penalty instead of 0 keeps such a response from slipping through the
// budget unmetered; it already reached the client, so this consumes the consumer's
// budget and their *next* request is blocked. Derived from the buffer size
// (~RespBodyBufferKB*1024/4 tokens) — a response that didn't fit was at least this
// large.
//
// It must never be charged on absence of `usage` alone. A short 2xx JSON body with
// no usage is simply not a completion, and charging it made one GET /v1/models
// cost 65536 tokens and 429 a 500-token consumer for the rest of the window.
//
// It is a BUDGET figure, not a measurement: the ledger records it as
// meter_quality="penalty" and must never sum it into reported consumption.
const FailClosedTokens = RespBodyBufferBytes / 4

// DSNameReq / DSNameResp / DSNameRespData are the suffixes appended to the VS
// name to generate DataScript set names.  One DataScript set is created per
// event phase.
const (
	DSNameSuffixReq            = "-ai-tok-req"
	DSNameSuffixResp           = "-ai-tok-resp"
	DSNameSuffixRespData       = "-ai-tok-respdata"
	DSNameSuffixReqDataEnforce = "-ai-tok-reqdata"
)

// RPSRateLimiterSuffix names the native Avi rate limiter (on the request-phase
// VSDataScriptSet) that the request-rate DataScript references via
// avi.vs.ratelimit.exceed(). Scoped to the VS so it never collides across VSes.
const RPSRateLimiterSuffix = "-ai-rps"

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

	// ReqDataEnforceScript is the HTTP_REQ_DATA phase Lua for limits whose budget
	// ceiling depends on a value that is only known after the request body is read
	// — specifically a per-tier budget keyed on the `ai_tier` reqvar that an
	// AIModelRoutePolicy sets in its own HTTP_REQ_DATA script. It is empty unless a
	// limit's groupHeader names a "reqvar:" source. It must run *after* the
	// model-route script (lower DataScript index sets ai_tier first — verified that
	// reqvars cross DataScriptSets and execution follows index order).
	ReqDataEnforceScript string
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
func GenerateTokenAccountingScripts(policy *AITokenRateLimitPolicy, mode AuthClaimMode) TokenAccountingScripts {
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
	helper := jwtClaimHelper(mode)

	// jwt_claim must be defined before ANY block that calls it: Lua resolves a
	// `local function` only from its declaration onward, so a call emitted above
	// this point would hit a nil global at request time. The counters endpoint
	// calls it when claim-gating is configured, hence emitting it first.
	reqParts = append(reqParts, helper)

	// ── Read-only counters endpoint (dashboard UI) ────────────────────────
	// Prepended *before* enforcement so it intercepts GET /v1/admin/counters and
	// returns the live per-user token usage as JSON, then returns. The SE table
	// cannot be enumerated, so the caller passes the users it wants in ?users=.
	//
	// Emitted when EITHER credential is configured. Gating on the admin token
	// alone made the shared secret the feature's on-switch as well as its lock:
	// dropping the Secret to get the plaintext out of the generated DataScript
	// deleted the endpoint instead of retiring the credential, which is the one
	// end state this whole mechanism exists to reach.
	if (policy.AdminToken != "" || policy.AdminClaimName != "") && len(spec.Limits) > 0 {
		reqParts = append(reqParts, buildCountersEndpointBlock(spec.Limits[0], policy.CounterEpoch,
			policy.AdminToken, policy.AdminClaimName, policy.AdminClaimValue))

		// ── Usage-record drain (token ledger collector) ───────────────────
		// Same admin credentials, same /v1/admin/ prefix, same "the DataScript
		// is the gate" posture as the counters endpoint above. Where counters
		// answers "how much is this identity using right now" from a scalar the
		// budget also reads, this hands over the individual records so an
		// out-of-band collector can keep the history the SE deliberately does
		// not — see docs/gateway-api/ai-gateway-token-ledger.md.
		reqParts = append(reqParts, buildQueryValueHelper())
		reqParts = append(reqParts, buildUsageDrainBlock(
			policy.AdminToken, policy.AdminClaimName, policy.AdminClaimValue))
	}

	// ── Request-phase: enforce limits ─────────────────────────────────────
	reqParts = append(reqParts, "-- AKO AI Gateway: token-budget enforcement")
	reqParts = append(reqParts, identityBlock)
	reqParts = append(reqParts, "local now = os.time()")

	// Limits whose budget depends on the tier (groupHeader "reqvar:ai_tier") are
	// enforced in HTTP_REQ_DATA instead, because the tier is only set there (by the
	// model-route script). All other limits enforce in HTTP_REQ as before.
	var reqDataEnforceParts []string
	needReqData := false
	for _, limit := range spec.Limits {
		if limitUsesReqvar(limit) {
			needReqData = true
			continue
		}
		reqParts = append(reqParts, buildReqLimitBlock(limit, policy.CounterEpoch))
	}
	if needReqData {
		reqDataEnforceParts = append(reqDataEnforceParts,
			"-- AKO AI Gateway: tier-dependent token-budget enforcement (HTTP_REQ_DATA, after model routing sets ai_tier)")
		reqDataEnforceParts = append(reqDataEnforceParts, helper)
		reqDataEnforceParts = append(reqDataEnforceParts, identityBlock)
		reqDataEnforceParts = append(reqDataEnforceParts, "local now = os.time()")
		for _, limit := range spec.Limits {
			if limitUsesReqvar(limit) {
				reqDataEnforceParts = append(reqDataEnforceParts, buildReqLimitBlock(limit, policy.CounterEpoch))
			}
		}
	}

	// ── Skip metering for external-provider tiers ────────────────────────
	// A provider tier (e.g. Gemini) is routed by AIModelRoutePolicy, which sets
	// the ai_skip_meter reqvar. Its response is chunked (like streaming), which
	// the body-based usage parser can't read anyway, so skip the response phases
	// entirely rather than error on it.
	skipGuard := `if avi.http.get_reqvar("ai_skip_meter") == "1" then return end`
	respParts = append(respParts, skipGuard)
	respDataParts = append(respDataParts, skipGuard)

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
		respDataParts = append(respDataParts, buildRespLimitBlock(limit, policy.CounterEpoch))
	}

	// The ledger record goes last: the budget counters are what enforcement acts
	// on, so they are written first and are never delayed by accounting work.
	respDataParts = append(respDataParts, buildUsageRecordBlock(spec.TargetRef.Name))

	return TokenAccountingScripts{
		ReqScript:            strings.Join(reqParts, "\n"),
		RespScript:           strings.Join(respParts, "\n"),
		RespDataScript:       strings.Join(respDataParts, "\n"),
		ReqDataEnforceScript: strings.Join(reqDataEnforceParts, "\n"),
	}
}

// limitUsesReqvar reports whether a limit's budget ceiling is selected by a value
// carried in a request-scoped variable (e.g. the `ai_tier` reqvar set by an
// AIModelRoutePolicy) rather than a JWT claim/header. Such limits must be enforced
// in HTTP_REQ_DATA, after the reqvar is set.
func limitUsesReqvar(l TokenLimit) bool {
	return strings.HasPrefix(l.GroupHeader, "reqvar:")
}

// groupReadExpr returns the Lua that resolves the per-group budget selector into
// the local `group_hdr`. A "reqvar:<name>" source reads avi.http.get_reqvar
// (e.g. the tier set by model routing); anything else is read as an OAuth-verified
// claim via jwt_claim.
func groupReadExpr(groupHeader string) string {
	if strings.HasPrefix(groupHeader, "reqvar:") {
		name := strings.TrimPrefix(groupHeader, "reqvar:")
		return fmt.Sprintf("  local group_hdr = avi.http.get_reqvar(%q) or \"\"\n", name)
	}
	return fmt.Sprintf("  local _ok_g, _g = pcall(jwt_claim, %q)\n  local group_hdr = (_ok_g and _g) or \"\"\n", groupHeader)
}

// jwtClaimHelper returns a Lua function `jwt_claim(claim)` that returns a string
// claim from the SE-validated JWT. The implementation depends on how the SE was
// asked to validate the token (see AuthClaimMode):
//
//   - ClaimModeOAuth: the SE runs the OAuth resource-server flow and exposes
//     claims via avi.http.oauth_get_claim(provider_index, claim). Provider index
//     0 selects the only oauth_settings entry. The value may come back as a Lua
//     table (multi-valued claim), so the first scalar is unwrapped.
//
//   - ClaimModeJWTQuery: the SE validated a bearer JWT presented as the ?<jwt>=
//     query param (oauth_get_claim is unavailable here). Since the SE does NOT
//     strip the query param, the helper reads the same (already-validated) token
//     and base64url-decodes its payload to read the claim. Decode-and-trust is
//     safe ONLY because the SE already verified the signature/aud/exp — never
//     emit this variant on a VS that isn't enforcing jwt_config validation.
//
// Both variants are sandbox-safe (no string.match, tonumber guarded) and yield ""
// rather than raising on a missing claim or unauthenticated request.
func jwtClaimHelper(mode AuthClaimMode) string {
	if mode == ClaimModeJWTQuery {
		return jwtQueryClaimHelper()
	}
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

// jwtQueryClaimHelper returns the ClaimModeJWTQuery variant of jwt_claim: it pulls
// the SE-validated token from the ?<JwtQueryParamName>= query param and decodes
// the JWT payload to read a claim. Uniquely-named internal locals (_b64url_decode,
// _json_scalar) so it composes safely alongside json_str / other helpers already
// emitted by the model-route and MCP generators.
func jwtQueryClaimHelper() string {
	return fmt.Sprintf(`-- AKO AI Gateway: read a claim from the SE-validated query-param JWT.
-- The SE validated the token at ?%[1]s=<jwt> (jwt_location=QUERY_PARAM); it is not
-- stripped, so decode its payload here. base64url + minimal JSON scalar read only.
local function _b64url_decode(data)
  local map = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
  local rev = {}
  for i = 1, #map do rev[string.sub(map, i, i)] = i - 1 end
  local out, buf, bits = {}, 0, 0
  for i = 1, #data do
    local v = rev[string.sub(data, i, i)]
    if v ~= nil then
      buf = buf * 64 + v
      bits = bits + 6
      if bits >= 8 then
        bits = bits - 8
        out[#out + 1] = string.char(math.floor(buf / (2 ^ bits)) %% 256)
        buf = buf %% (2 ^ bits) -- drop the consumed high bits so buf stays bounded
      end
    end
  end
  return table.concat(out)
end
local function _json_scalar(s, k)
  local p = string.find(s, '"' .. k .. '"', 1, true)
  if not p then return "" end
  local c = string.find(s, ":", p, true)
  if not c then return "" end
  local i = c + 1
  while i <= #s and string.sub(s, i, i) == " " do i = i + 1 end
  local ch = string.sub(s, i, i)
  if ch == '"' then
    local e = string.find(s, '"', i + 1, true)
    if not e then return "" end
    return string.sub(s, i + 1, e - 1)
  elseif ch == "[" then
    local q1 = string.find(s, '"', i, true)
    if not q1 then return "" end
    local q2 = string.find(s, '"', q1 + 1, true)
    if not q2 then return "" end
    return string.sub(s, q1 + 1, q2 - 1)
  else
    local e = string.find(s, ",", i, true) or string.find(s, "}", i, true) or (#s + 1)
    return string.sub(s, i, e - 1)
  end
end
local function jwt_claim(claim)
  local q = avi.http.get_query() or ""
  local s = string.find(q, "%[1]s=", 1, true)
  if not s then return "" end
  local rest = string.sub(q, s + %[2]d)
  local amp = string.find(rest, "&", 1, true)
  local tok = amp and string.sub(rest, 1, amp - 1) or rest
  if tok == "" then return "" end
  local d1 = string.find(tok, ".", 1, true)
  if not d1 then return "" end
  local d2 = string.find(tok, ".", d1 + 1, true)
  local payload = d2 and string.sub(tok, d1 + 1, d2 - 1) or string.sub(tok, d1 + 1)
  return _json_scalar(_b64url_decode(payload), claim)
end`, JwtQueryParamName, len(JwtQueryParamName)+1)
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
// incorporates an optional reset epoch, the limit name, identity dimension, and
// current window boundary. Bumping epoch moves to a fresh keyspace (counter reset).
func counterKeyExpr(limit TokenLimit, epoch string) string {
	windowSec := windowSeconds(limit.Window)
	keyExpr := counterKeyIdentityExpr(limit.Key)
	prefix := limit.Name
	if epoch != "" {
		prefix = epoch + ":" + limit.Name
	}
	return fmt.Sprintf(`%q..":"..%s..":"..math.floor(now/%d)*%d`,
		prefix, keyExpr, windowSec, windowSec)
}

// buildCountersEndpointBlock returns the Lua for a read-only admin endpoint that
// the dashboard UI polls to render per-user token usage. It runs in HTTP_REQ
// before enforcement: on GET /v1/admin/counters it checks the X-Admin-Token
// header (auth is SKIP_AUTHENTICATION'd for this path in the SSO policy, so the
// DataScript is the gate), then for each user passed in ?users=a,b,c it rebuilds
// the *same* counter key the response-phase accounting writes and returns the
// running total. The SE string table has no enumeration API, which is why the
// caller must supply the identities it cares about.
//
// Key parsing avoids Lua patterns (string.match/gmatch are restricted in the SE
// sandbox); it uses plain string.find/string.sub only.
// When claimName/claimValue are non-empty the gate also accepts a caller whose
// SE-validated JWT carries that claim, so a client can authenticate as itself
// instead of replaying a shared secret. Both gates are live at once: that is what
// makes the migration reversible — callers move one at a time, and clearing the
// annotation drops back to header-only without touching the image.
func buildCountersEndpointBlock(limit TokenLimit, epoch, adminToken, claimName, claimValue string) string {
	windowSec := windowSeconds(limit.Window)
	prefix := limit.Name
	if epoch != "" {
		prefix = epoch + ":" + limit.Name
	}
	// The gate itself lives in buildAdminAuthGate, shared with the usage drain:
	// two admin endpoints with the same trust decision must not carry two
	// hand-copied copies of it.
	return fmt.Sprintf(`-- AKO AI Gateway: read-only token-counters endpoint (dashboard UI)
do
  if avi.http.get_path() == "/v1/admin/counters" then
%s
    local q = avi.http.get_query() or ""
    local users = ""
    do
      local s = string.find(q, "users=", 1, true)
      if s then
        local rest = string.sub(q, s + 6)
        local amp = string.find(rest, "&", 1, true)
        users = amp and string.sub(rest, 1, amp - 1) or rest
      end
    end
    local wb = math.floor(os.time() / %d) * %d
    local parts = {}
    if users ~= "" then
      local start = 1
      while true do
        local c = string.find(users, ",", start, true)
        local u = c and string.sub(users, start, c - 1) or string.sub(users, start)
        if u ~= "" then
          local used = tonumber(avi.vs.table_lookup(%q..":"..u..":"..wb) or 0) or 0
          parts[#parts + 1] = '{"user":"'..u..'","used":'..used..'}'
        end
        if not c then break end
        start = c + 1
      end
    end
    avi.http.response(200, {["Content-Type"]="application/json"},
      '{"window":'..wb..',"limit":%q,"counters":['..table.concat(parts, ",")..']}')
    return
  end
end`, buildAdminAuthGate(adminToken, claimName, claimValue),
		windowSec, windowSec, prefix, limit.Name)
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
func buildReqLimitBlock(limit TokenLimit, epoch string) string {
	windowSec := windowSeconds(limit.Window)
	keyExpr := counterKeyExpr(limit, epoch)
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
		// Per-group budget: read the group selector (an OAuth claim, or a "reqvar:"
		// source such as the ai_tier set by model routing) into group_hdr.
		b.WriteString(groupReadExpr(limit.GroupHeader))
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
// to read. A response is scanned when it is a successful (2xx) response with a JSON
// content-type — so streaming text/event-stream responses and error responses are
// skipped outright. Read-only JSON responses (GET /v1/models) still get scanned;
// they simply parse no usage and, being short, are charged nothing. There is no
// method test because avi.http.get_method does not exist on this SE build.
//
// status is called through a deferred closure, not `pcall(avi.http.status)`: this
// sandbox raises on the *field access* for an unknown avi.http name, and the
// argument is evaluated before pcall ever runs, so the bare form protects nothing.
//
// HTTP_RESP_DATA can't read headers, so the ai_meter reqvar is how that phase
// learns "this response is metered" without re-sniffing the body.
func buildBufferEnableBlock() string {
	return fmt.Sprintf(`-- AKO AI Gateway: decide+flag a metered response, buffer its body for HTTP_RESP_DATA
do
  local ct = avi.http.get_header("Content-Type") or ""
  local is_json = string.find(ct, "application/json", 1, true) ~= nil
  -- No method test here. avi.http.get_method does not exist on this SE build, and
  -- the bare pcall(avi.http.get_method) form failed open (is_post kept its 'true'
  -- default) — which is what charged read-only requests the full penalty. Truncation
  -- evidence in HTTP_RESP_DATA decides fail-closed instead, so no method is needed.
  local is_ok = true
  do local ok, s = pcall(function() return avi.http.status() end); if ok and s then local n = tonumber(s or 0); if n then is_ok = (n >= 200 and n < 300) end end end
  -- A compressed body can never be parsed for 'usage'. Record that here, because
  -- HTTP_RESP_DATA cannot read headers, so the penalty stays fail-closed for it.
  local enc = avi.http.get_header("Content-Encoding") or ""
  if enc ~= "" and enc ~= "identity" then avi.http.set_reqvar("ai_meter_enc", "1") end
  if is_json and is_ok then
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
local cached_tokens     = 0
local reasoning_tokens  = 0
local model_name        = ""
local meter_quality     = "none"
if avi.http.get_reqvar("ai_meter") == "1" then
  local _body = avi.http.get_response_body(%d)
  -- The value of a quoted JSON string key. Used for the model the backend
  -- actually served, which is not always the one the caller asked for once model
  -- routing has picked a tier.
  local function _str_after(key)
    if not _body then return "" end
    local s = string.find(_body, key, 1, true)
    if not s then return "" end
    local q1 = string.find(_body, '"', s + #key, true)
    if not q1 then return "" end
    local q2 = string.find(_body, '"', q1 + 1, true)
    if not q2 then return "" end
    return string.sub(_body, q1 + 1, q2 - 1)
  end
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
  -- Kept as separate dimensions, not folded into the totals: a cached prompt
  -- token and a fresh one cost very different amounts, and reasoning tokens are
  -- billed as output the caller never sees. A ledger that sums them away cannot
  -- ever produce a cost.
  cached_tokens     = _num_after('"cached_tokens"')
  reasoning_tokens  = _num_after('"reasoning_tokens"')
  model_name        = _str_after('"model"')
  if total_tokens > 0 then
    meter_quality = "exact"
  elseif (_body and #_body >= %d) or avi.http.get_reqvar("ai_meter_enc") == "1" then
    -- Unreadable, not unmetered: the body filled the buffer (so 'usage' sat past its
    -- end) or arrived compressed. Charge the penalty so a large completion cannot
    -- slip through the budget. A SHORT body with no 'usage' is simply not a
    -- completion (GET /v1/models, an error envelope) and now costs nothing — that
    -- blanket charge was the whole reason a read-only request burned 65536 tokens.
    total_tokens      = %d
    prompt_tokens     = %d
    completion_tokens = %d
    meter_quality     = "penalty"
  end
end
-- Provenance for the usage ledger: only "exact" is a measurement. The budget may be
-- charged a penalty; the reported figure must never present one as measured.
pcall(function() avi.http.set_reqvar("ai_meter_quality", meter_quality) end)`,
		RespBodyBufferKB, RespBodyBufferBytes, FailClosedTokens, FailClosedTokens, FailClosedTokens)
}

// buildRespLimitBlock generates the Lua snippet that increments one counter in
// the HTTP_RESP phase, with a TTL equal to the window length.
func buildRespLimitBlock(limit TokenLimit, epoch string) string {
	windowSec := windowSeconds(limit.Window)
	keyExpr := counterKeyExpr(limit, epoch)
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
func DSReqName(vsName string) string            { return vsName + DSNameSuffixReq }
func DSRespName(vsName string) string           { return vsName + DSNameSuffixResp }
func DSRespDataName(vsName string) string       { return vsName + DSNameSuffixRespData }
func DSReqDataEnforceName(vsName string) string { return vsName + DSNameSuffixReqDataEnforce }

// RPSRateLimiterName returns the native rate limiter name for a given VS. The
// same name is used both in the RateLimiter object on the VSDataScriptSet and in
// the avi.vs.ratelimit.exceed() call in the script — they must match.
func RPSRateLimiterName(vsName string) string { return vsName + RPSRateLimiterSuffix }
