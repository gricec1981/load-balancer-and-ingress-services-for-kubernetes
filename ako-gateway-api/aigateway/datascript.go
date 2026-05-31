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
	DSEvtHTTPReq  = "VS_DATASCRIPT_EVT_HTTP_REQ"
	DSEvtHTTPResp = "VS_DATASCRIPT_EVT_HTTP_RESP"
)

// DSTableName is the Avi SE table used to store per-window token counters.
// The table is shared state across connections on the same SE.
const DSTableName = "ai_tok"

// DSNameReq / DSNameResp are the suffixes appended to the VS name to generate
// DataScript set names.  One DataScript set is created per event phase.
const (
	DSNameSuffixReq  = "-ai-tok-req"
	DSNameSuffixResp = "-ai-tok-resp"
)

// TokenAccountingScripts holds the two Lua snippets (request-phase enforcement
// and response-phase accounting) generated from an AITokenRateLimitPolicy.
type TokenAccountingScripts struct {
	// ReqScript is the HTTP_REQ phase Lua code.  It looks up per-identity token
	// counters and rejects the request when any limit is already exceeded.
	ReqScript string

	// RespScript is the HTTP_RESP phase Lua code.  It parses the OpenAI-compatible
	// `usage` block in the response body and increments every applicable counter.
	// For streaming (SSE) responses the script parses only the terminal `data:`
	// chunk that carries `stream_options.include_usage=true`.
	RespScript string
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

	var reqParts, respParts []string

	// ── Shared header: resolve identity ────────────────────────────────────
	identityBlock := buildIdentityBlock(identityHeader, fallback)

	// ── Request-phase: enforce limits ─────────────────────────────────────
	reqParts = append(reqParts, "-- AKO AI Gateway: token-budget enforcement")
	reqParts = append(reqParts, identityBlock)
	reqParts = append(reqParts, "local now = os.time()")

	for _, limit := range spec.Limits {
		reqParts = append(reqParts, buildReqLimitBlock(limit))
	}

	// ── Response-phase: account for tokens ────────────────────────────────
	respParts = append(respParts, "-- AKO AI Gateway: token-usage accounting")
	respParts = append(respParts, identityBlock)
	respParts = append(respParts, "local now = os.time()")
	respParts = append(respParts, buildUsageParseBlock())

	for _, limit := range spec.Limits {
		respParts = append(respParts, buildRespLimitBlock(limit))
	}

	return TokenAccountingScripts{
		ReqScript:  strings.Join(reqParts, "\n"),
		RespScript: strings.Join(respParts, "\n"),
	}
}

// buildIdentityBlock returns the Lua snippet that resolves the consumer identity
// into the local variable `identity`.
func buildIdentityBlock(header, fallback string) string {
	var b strings.Builder
	// In HTTP_RESP, get_header reads response headers by default.
	// Pass avi.HTTP_REQUEST to read a request header in either phase.
	fmt.Fprintf(&b, `local identity = avi.http.get_header(%q, avi.HTTP_REQUEST)`, header)
	b.WriteString("\n")
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
	fmt.Fprintf(&b, "-- limit: %s (budget %d / %s)\n", limit.Name, limit.Budget, limit.Window)
	fmt.Fprintf(&b, "do\n")
	fmt.Fprintf(&b, "  local k = %s\n", keyExpr)
	fmt.Fprintf(&b, "  local cur = tonumber(avi.vs.table_lookup(k) or 0)\n")
	fmt.Fprintf(&b, "  if cur >= %d then\n", limit.Budget)

	switch actionType {
	case "Log":
		fmt.Fprintf(&b, "    -- Log-only mode: allow but emit a log entry\n")
		fmt.Fprintf(&b, "    avi.vs.log(string.format(\"ai-gateway: limit %s exceeded (cur=%%s budget=%d)\", tostring(cur)))\n",
			limit.Name, limit.Budget)
	default:
		headers := fmt.Sprintf(`{["Content-Type"] = "application/json"`)
		if doRetryAfter {
			fmt.Fprintf(&b, "    local retry_at = (math.floor(now/%d)+1)*%d - now\n", windowSec, windowSec)
			headers += `, ["Retry-After"] = tostring(math.max(1, retry_at))`
		}
		headers += "}"
		fmt.Fprintf(&b, "    avi.http.response(%d, %s,\n", statusCode, headers)
		fmt.Fprintf(&b, "      string.format('{\"error\":\"token_budget_exceeded\",\"limit\":%q,\"current\":%%d,\"budget\":%d}', cur))\n",
			limit.Name, limit.Budget)
		fmt.Fprintf(&b, "    return\n")
	}

	fmt.Fprintf(&b, "  end\n")
	fmt.Fprintf(&b, "end\n")
	return b.String()
}

// buildUsageParseBlock generates the Lua snippet that extracts prompt_tokens,
// completion_tokens, and total_tokens from the backend response.
//
// Avi DataScripts cannot read the HTTP response *body* in the HTTP_RESP event
// (avi.http.get_body is unavailable there; body access requires the
// HTTP_RESP_DATA buffered-event mechanism). For reliable token accounting the
// backend reports usage via response headers, which avi.http.get_header reads
// directly in the HTTP_RESP event:
//
//	X-Prompt-Tokens / X-Completion-Tokens / X-Total-Tokens
//
// An OpenAI-style proxy/sidecar can surface usage.* from the JSON body into
// these headers; full in-SE body parsing is a follow-up (HTTP_RESP_DATA).
func buildUsageParseBlock() string {
	return `-- token usage from backend response headers (HTTP_RESP cannot read the body)
local prompt_tokens     = tonumber(avi.http.get_header("X-Prompt-Tokens")) or 0
local completion_tokens = tonumber(avi.http.get_header("X-Completion-Tokens")) or 0
local total_tokens      = tonumber(avi.http.get_header("X-Total-Tokens")) or (prompt_tokens + completion_tokens)`
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
func DSReqName(vsName string) string  { return vsName + DSNameSuffixReq }
func DSRespName(vsName string) string { return vsName + DSNameSuffixResp }
