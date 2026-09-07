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

// Streamed completions and the token budget.
//
// A streamed response (`"stream": true` → text/event-stream) cannot be metered
// from its body: HTTP_RESP_DATA is buffer-complete, and turning buffering on
// collapses the stream into one delivery (probed live). Until the SE can count
// a stream itself, the budget is charged at ADMISSION with what the client asked
// for — the same thing the hosted providers' rate limiters do — and the ledger
// records that reservation as quality "estimated", never as a measurement.
//
// Three pieces, in three phases:
//
//	HTTP_REQ        buffer the request head (32 KB cap on this SE build)
//	HTTP_REQ_DATA   detect "stream": true, reserve prompt-estimate + max_tokens
//	                against every limit, flag the request, log the decision
//	HTTP_RESP       2xx  → append an "estimated" ledger row from the flags
//	                else → hand the reservation back
//	                and, for a text/event-stream nobody reserved (the field sat
//	                past the buffered head), charge the fail-closed penalty so
//	                a long-prompt stream cannot slip through at zero.
//
// Deny mode answers 400 in HTTP_REQ_DATA, before routing and before the backend.
// Allow mode emits none of this and leaves the pre-existing bypass in place.

// ReqBodyBufferBytesForStreaming is the request-head size the token policy
// buffers so HTTP_REQ_DATA can read stream/max_tokens. Same cap as model
// routing (avi.http.set_request_body_buffer_size rejects more than 32768).
const ReqBodyBufferBytesForStreaming = ReqBodyBufferBytes

// buildReqBodyBufferBlock returns the HTTP_REQ Lua that enables request-body
// buffering. Idempotent alongside the model-route script's identical call.
func buildReqBodyBufferBlock() string {
	return fmt.Sprintf(`-- AKO AI Gateway: buffer the request head so HTTP_REQ_DATA can read stream/max_tokens
pcall(function() avi.http.set_request_body_buffer_size(%d) end)`, ReqBodyBufferBytesForStreaming)
}

// buildStreamReserveBlock returns the HTTP_REQ_DATA Lua for Reserve and Deny
// modes. `identity`, `now` and jwt_claim must be in scope (the caller emits the
// shared header first). charge is invoked per limit and must return Lua that
// adds the locals prompt_tokens / completion_tokens / total_tokens to that
// limit's counters — the same blocks the response phase uses for a measured
// completion, so a reservation and a measurement land in the same keys.
func buildStreamReserveBlock(limits []TokenLimit, st StreamingPolicy, charge func(TokenLimit) string) string {
	var b strings.Builder
	b.WriteString("-- AKO AI Gateway: streamed request — reserve the budget at admission (HTTP_REQ_DATA)\n")
	b.WriteString("do\n")
	fmt.Fprintf(&b, "  local _ok_rb, _rb = pcall(function() return avi.http.get_req_body(%d) end)\n", ReqBodyBufferBytesForStreaming)
	b.WriteString(`  if not _ok_rb or _rb == nil then _rb = "" end
  -- Sandbox-safe scans (no string.match): the index just past the ':' and any
  -- whitespace after a quoted key, or nil when the key is not in the head.
  local function _rb_after(key)
    local s = string.find(_rb, key, 1, true)
    if not s then return nil end
    local i = s + #key
    while i <= #_rb do
      local c = string.byte(_rb, i)
      if c ~= 32 and c ~= 9 and c ~= 10 and c ~= 13 and c ~= 58 then break end
      i = i + 1
    end
    return i
  end
  local function _rb_num(key)
    local i = _rb_after(key)
    if not i then return 0 end
    local j = i
    while j <= #_rb do local c = string.byte(_rb, j); if c < 48 or c > 57 then break end; j = j + 1 end
    if j > i then return tonumber(string.sub(_rb, i, j - 1)) or 0 end
    return 0
  end
  local function _rb_str(key)
    local i = _rb_after(key)
    if not i or string.sub(_rb, i, i) ~= '"' then return "" end
    local e = string.find(_rb, '"', i + 1, true)
    if not e then return "" end
    return string.sub(_rb, i + 1, e - 1)
  end
  local _stream = false
  do local i = _rb_after('"stream"'); if i and string.sub(_rb, i, i + 3) == "true" then _stream = true end end
  if _stream then
`)
	if st.Mode == StreamingModeDeny {
		b.WriteString(`    pcall(function() avi.vs.log("ai-gateway: stream=1 mode=deny") end)
    avi.http.response(400, {["Content-Type"] = "application/json"},
      '{"error":"streaming_not_allowed","hint":"this route does not admit streamed responses; send stream:false"}')
    return
  end
end
`)
		return b.String()
	}

	// Reserve mode.
	b.WriteString(`    local _max = _rb_num('"max_tokens"')
    do local _mc = _rb_num('"max_completion_tokens"'); if _mc > _max then _max = _mc end end
    if _max <= 0 then
`)
	if st.DefaultMaxTokens > 0 {
		fmt.Fprintf(&b, "      _max = %d\n", st.DefaultMaxTokens)
	} else {
		b.WriteString(`      -- No ceiling means no bound: the backend would generate to its context
      -- limit and the reservation would charge a fraction of it. Refuse instead.
      pcall(function() avi.vs.log("ai-gateway: stream=1 mode=reserve rejected=max_tokens_required") end)
      avi.http.response(400, {["Content-Type"] = "application/json"},
        '{"error":"max_tokens_required","hint":"a streamed request must state max_tokens so its budget can be reserved"}')
      return
`)
	}
	b.WriteString(`    end
    do local _n = _rb_num('"n"'); if _n > 1 and _n <= 16 then _max = _max * _n end end
`)
	fmt.Fprintf(&b, "    local prompt_tokens = math.floor(#_rb / %d)\n", st.PromptCharsPerToken)
	b.WriteString(`    if prompt_tokens < 1 then prompt_tokens = 1 end
    local completion_tokens = _max
    local total_tokens = prompt_tokens + completion_tokens
`)
	for _, limit := range limits {
		b.WriteString(indent(charge(limit), "    "))
	}
	b.WriteString(`    avi.http.set_reqvar("ai_stream", "1")
    avi.http.set_reqvar("ai_res_prompt", tostring(prompt_tokens))
    avi.http.set_reqvar("ai_res_completion", tostring(completion_tokens))
    avi.http.set_reqvar("ai_res_model", _rb_str('"model"'))
    -- The response phases must leave the stream alone: no buffering, no parse.
    avi.http.set_reqvar("ai_skip_meter", "1")
    pcall(function() avi.vs.log("ai-gateway: stream=1 mode=reserve reserved=" .. total_tokens .. " max_tokens=" .. _max .. " prompt_est=" .. prompt_tokens) end)
  end
end
`)
	return b.String()
}

// buildStreamRespBlock returns the HTTP_RESP Lua that closes the loop on a
// streamed response. It must run BEFORE the ai_skip_meter guard (a reserved
// stream sets that flag) and needs `identity`, `now` and jwt_claim in scope.
//
// charge and release are invoked per limit with the token locals in scope:
// charge adds them (the fail-closed penalty path), release subtracts them and
// clamps at zero (a reservation handed back on a non-2xx).
func buildStreamRespBlock(limits []TokenLimit, route string, charge, release func(TokenLimit) string) string {
	var b strings.Builder
	b.WriteString("-- AKO AI Gateway: streamed response — record the reservation, or fail closed on a stream nobody reserved (HTTP_RESP)\n")
	b.WriteString(`do
  local _st = 0
  do local ok, s = pcall(function() return avi.http.status() end); if ok and s then _st = tonumber(s or 0) or 0 end end
  local _ct = avi.http.get_header("Content-Type") or ""
  local _sse = string.find(_ct, "text/event-stream", 1, true) ~= nil
  if avi.http.get_reqvar("ai_stream") == "1" then
    local prompt_tokens = tonumber(avi.http.get_reqvar("ai_res_prompt") or "0") or 0
    local completion_tokens = tonumber(avi.http.get_reqvar("ai_res_completion") or "0") or 0
    local total_tokens = prompt_tokens + completion_tokens
    local cached_tokens, reasoning_tokens = 0, 0
    local model_name = avi.http.get_reqvar("ai_res_model") or ""
    if _st >= 200 and _st < 300 then
      -- The stream is being relayed as this runs. What the ledger can honestly
      -- say is what was reserved, and it says so in the quality column.
      local meter_quality = "estimated"
`)
	b.WriteString(indent(buildUsageRecordBlock(route), "      "))
	b.WriteString(`    else
      -- The client got an error, not a stream: give the reservation back.
`)
	for _, limit := range limits {
		b.WriteString(indent(release(limit), "      "))
	}
	b.WriteString(`      pcall(function() avi.vs.log("ai-gateway: stream=1 released=" .. total_tokens .. " status=" .. _st) end)
    end
    return
  elseif _sse and avi.http.get_reqvar("ai_skip_meter") ~= "1" then
    -- A stream this policy never reserved: the "stream" field sat past the
    -- buffered request head (long prompt, field serialised late). It cannot be
    -- read now and it must not cost zero — charge the same fail-closed penalty
    -- an unreadable JSON body gets, and record it as a penalty.
`)
	fmt.Fprintf(&b, "    local prompt_tokens, completion_tokens, total_tokens = %d, %d, %d\n", FailClosedTokens, FailClosedTokens, FailClosedTokens)
	b.WriteString(`    local cached_tokens, reasoning_tokens = 0, 0
    local model_name = ""
    local meter_quality = "penalty"
`)
	for _, limit := range limits {
		b.WriteString(indent(charge(limit), "    "))
	}
	b.WriteString(indent(buildUsageRecordBlock(route), "    "))
	b.WriteString(`    pcall(function() avi.vs.log("ai-gateway: stream=1 undetected penalty=" .. total_tokens) end)
    return
  end
end
`)
	return b.String()
}

// buildReleaseBlock returns Lua that subtracts the token locals from one limit's
// counters, clamped at zero: the per-SE window counter always, and the native
// carry when the limit is enforced on the native limiter. A carry already
// charged to the limiter by a later request of the same consumer cannot be
// recovered; the clamp bounds that to one reservation.
func buildReleaseBlock(limit TokenLimit, epoch string, native bool) string {
	windowSec := windowSeconds(limit.Window)
	dimVar := tokenDimensionExpr(limit.Tokens)

	var b strings.Builder
	fmt.Fprintf(&b, "-- release: %s\n", limit.Name)
	b.WriteString("do\n")
	fmt.Fprintf(&b, "  local k = %s\n", counterKeyExpr(limit, epoch))
	b.WriteString("  local cur = tonumber(avi.vs.table_lookup(k) or 0) or 0\n")
	fmt.Fprintf(&b, "  local nv = cur - %s\n", dimVar)
	b.WriteString("  if nv < 0 then nv = 0 end\n")
	fmt.Fprintf(&b, "  local ttl = (%d - (now %% %d)) + 5\n", windowSec, windowSec)
	b.WriteString("  avi.vs.table_remove(k)\n")
	b.WriteString("  avi.vs.table_insert(k, tostring(nv), math.max(1, ttl))\n")
	if native {
		fmt.Fprintf(&b, "  local _ck = %s\n", nativeCarryKeyExpr(limit, ""))
		b.WriteString("  local _carry = tonumber(avi.vs.table_lookup(_ck) or 0) or 0\n")
		fmt.Fprintf(&b, "  local _nc = _carry - %s\n", dimVar)
		b.WriteString("  if _nc < 0 then _nc = 0 end\n")
		b.WriteString("  avi.vs.table_remove(_ck)\n")
		fmt.Fprintf(&b, "  if _nc > 0 then avi.vs.table_insert(_ck, tostring(_nc), %d) end\n", windowSec)
	}
	b.WriteString("end\n")
	return b.String()
}

// indent prefixes every non-empty line of a generated block. Purely cosmetic —
// the SE does not care — but a reviewer reading the script off the controller
// does.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
