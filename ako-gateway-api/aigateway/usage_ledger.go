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

import "fmt"

// Usage ledger — the SE side of true token accounting.
//
// The budget counter in datascript.go answers "may this consumer proceed": one
// scalar per identity per window, SE-local, deliberately pessimistic. It is the
// wrong shape for reporting, which needs history, enumeration, dimensions, and
// an honest distinction between what was measured and what was assumed.
//
// So the response phase also appends one immutable RECORD per metered response
// to a ring in the same SE table, and a read-only drain endpoint hands those
// records to an out-of-band collector that owns the durable store. The SE table
// is a short transport queue here, not a store: nothing long-lived is kept on
// the SE, and the records leave within the ring TTL.
//
// Design: docs/gateway-api/ai-gateway-token-ledger.md
const (
	// UsageSeqKey holds "<inst>:<seq>" — the ring's instance id and its head.
	//
	// Both live in ONE key on purpose. They must expire together: if the
	// instance id could lapse while the sequence kept climbing, the collector
	// would see a new instance, reset its cursor to zero, and re-read every
	// record still in the ring as if it were new. One key, one lifetime, no
	// duplicate accounting.
	UsageSeqKey = "ai_useq"

	// UsageRecPrefix is the per-record key prefix: UsageRecPrefix .. <seq>.
	UsageRecPrefix = "ai_urec:"

	// UsageRingTTLSeconds is how long a record survives on the SE. It only has
	// to outlive the collector's poll interval by a wide margin — long enough to
	// ride out a collector restart, short enough that the SE never becomes a
	// store.
	UsageRingTTLSeconds = 300

	// UsageSeqTTLSeconds keeps the ring head alive across idle periods. It is
	// refreshed on every record, so it only lapses after a full day of no
	// metered traffic — at which point the instance id changes too and the
	// collector correctly treats it as a fresh ring.
	UsageSeqTTLSeconds = 86400

	// UsageDrainMax caps records per drain response. The SE builds this string
	// in Lua and returns it from a DataScript, so it must stay well inside a
	// sane response size.
	UsageDrainMax = 200

	// UsageScanMax caps how far back a single drain will walk. A collector that
	// has been down longer than the ring TTL would otherwise ask the SE to probe
	// tens of thousands of expired keys to find nothing. The gap it skipped is
	// reported as "lost" rather than silently closed.
	UsageScanMax = 1000

	// UsageDrainPath is the read-only drain endpoint. It sits under the same
	// /v1/admin/ prefix the counters endpoint uses, so it inherits that
	// authentication exemption (DefaultAdminSkipPath) and the DataScript is the
	// gate — see buildAdminAuthGate.
	UsageDrainPath = "/v1/admin/usage"
)

// buildAdminAuthGate returns Lua that sets a local `_authed` from whichever
// admin credentials are configured, and 403s when neither matches.
//
// Extracted so the counters endpoint and the usage drain cannot drift apart:
// they are the same trust decision, and a second endpoint with its own
// hand-copied gate is how one of them ends up accepting an empty header.
//
// The shared-secret branch is emitted ONLY when a secret is configured. Passing
// an empty token through would generate `get_header(...) == ""`, which any caller
// sending an empty header satisfies — that turns the gate off rather than
// removing it, and is what lets the secret actually be retired.
func buildAdminAuthGate(adminToken, claimName, claimValue string) string {
	gate := "    local _authed = false"
	if adminToken != "" {
		gate += fmt.Sprintf(`
    if avi.http.get_header("X-Admin-Token", avi.HTTP_REQUEST) == %q then _authed = true end`, adminToken)
	}
	// pcall: jwt_claim reads the SE-validated token, which is absent on an
	// unauthenticated request. A raw error here would abort the whole HTTP_REQ
	// script, so failure must degrade to "claim did not match".
	if claimName != "" && claimValue != "" {
		gate += fmt.Sprintf(`
    if not _authed then
      local _okc, _cv = pcall(jwt_claim, %q)
      if _okc and _cv == %q then _authed = true end
    end`, claimName, claimValue)
	}
	gate += `
    if not _authed then
      avi.http.response(403, {["Content-Type"]="application/json"}, '{"error":"forbidden"}')
      return
    end`
	return gate
}

// buildQueryValueHelper returns a Lua `_qval(name)` that reads one query
// parameter.
//
// It matches on "&<name>=" against a query string with a leading "&" prepended,
// so a parameter cannot be satisfied by another one that merely ends with its
// name. Plain string.find only — the SE sandbox restricts Lua patterns.
func buildQueryValueHelper() string {
	return `-- AKO AI Gateway: read one query parameter (no Lua patterns in this sandbox)
local function _qval(name)
  local q = "&" .. (avi.http.get_query() or "")
  local s = string.find(q, "&" .. name .. "=", 1, true)
  if not s then return "" end
  local rest = string.sub(q, s + #name + 2)
  local amp = string.find(rest, "&", 1, true)
  return amp and string.sub(rest, 1, amp - 1) or rest
end`
}

// buildUsageDrainBlock returns the Lua for the read-only drain endpoint the
// usage collector polls.
//
// It answers with the ring's instance id, its head, and the records after the
// caller's cursor. The collector keys its cursor on the instance id, which is
// what makes the drain safe on a scaled-out VS: a GET on the VIP lands on
// whichever SE the load balancer picks, each SE has its own table and therefore
// its own instance and sequence, and per-instance cursors mean records are
// neither double-counted nor missed as the collector is bounced between them.
//
// `next` is the cursor to send back, and `lost` reports how many sequence
// numbers were skipped because the caller fell further behind than UsageScanMax.
// A gap is reported, never silently closed — a ledger that quietly loses rows is
// worse than one that admits it did.
func buildUsageDrainBlock(adminToken, claimName, claimValue string) string {
	return fmt.Sprintf(`-- AKO AI Gateway: read-only usage-record drain (token ledger collector)
do
  if avi.http.get_path() == %q then
%s
    local cur = avi.vs.table_lookup(%q) or ""
    local inst, head = "", 0
    do
      local d = string.find(cur, ":", 1, true)
      if d then
        inst = string.sub(cur, 1, d - 1)
        head = tonumber(string.sub(cur, d + 1)) or 0
      end
    end
    local after = tonumber(_qval("after")) or 0
    local want  = tonumber(_qval("max")) or 0
    if want <= 0 or want > %d then want = %d end
    -- A cursor ahead of the head means this is a different (or restarted) ring
    -- than the caller last saw. Start from the beginning rather than stalling.
    if after < 0 or after > head then after = 0 end
    local lo = after + 1
    if head - after > %d then lo = head - %d + 1 end
    local parts, n, i = {}, 0, lo
    while i <= head and n < want do
      local r = avi.vs.table_lookup(%q .. i)
      if r then
        parts[#parts + 1] = '{"seq":' .. i .. ',"r":"' .. r .. '"}'
        n = n + 1
      end
      i = i + 1
    end
    avi.http.response(200, {["Content-Type"]="application/json"},
      '{"inst":"' .. inst .. '","head":' .. head .. ',"next":' .. (i - 1) ..
      ',"lost":' .. (lo - after - 1) .. ',"records":[' .. table.concat(parts, ",") .. ']}')
    return
  end
end`, UsageDrainPath, buildAdminAuthGate(adminToken, claimName, claimValue),
		UsageSeqKey, UsageDrainMax, UsageDrainMax, UsageScanMax, UsageScanMax, UsageRecPrefix)
}

// buildSafeFieldHelper returns a Lua `_safe(s, maxn)` that reduces a value to
// characters that are safe both inside the pipe-delimited record and inside the
// JSON string the drain endpoint embeds it in.
//
// This is the reason the drain can build its JSON by concatenation: every field
// is already known to contain no quote, no backslash and no pipe, so there is
// nothing to escape at read time. Everything the record carries — a JWT sub, a
// model name, a tier slug, an HTTPRoute name — is inside this character set
// already; anything else is a caller trying something.
//
// string.char is avoided in favour of string.sub: this sandbox has a reduced
// string library and there is no reason to depend on one more function than the
// job needs.
func buildSafeFieldHelper() string {
	return `local function _safe(s, maxn)
  if not s or s == "" then return "-" end
  local n = #s
  if n > maxn then n = maxn end
  local out = ""
  for i = 1, n do
    local c = string.byte(s, i)
    if (c >= 48 and c <= 57) or (c >= 65 and c <= 90) or (c >= 97 and c <= 122)
       or c == 45 or c == 46 or c == 95 or c == 58 or c == 47 or c == 64 then
      out = out .. string.sub(s, i, i)
    else
      out = out .. "_"
    end
  end
  if out == "" then return "-" end
  return out
end`
}

// buildUsageRecordBlock returns the Lua that appends one usage record to the
// ring, in the HTTP_RESP_DATA phase after the usage block has been parsed.
//
// Field order is fixed and positional (pipe-delimited) rather than JSON: the SE
// builds this string per response, and _safe has already guaranteed every field
// is free of the delimiter. The collector splits it.
//
//	ts | identity | model | tier | prompt | completion | cached | reasoning | quality | route | chain
//
// `chain` is the W3C trace id carried in by the request (see buildChainIDBlock);
// empty when the caller sent none. Appended last so a collector that knows only
// ten fields still parses the first ten.
//
// Only responses the meter actually saw are recorded (`meter_quality ~= "none"`),
// so a health check or a non-JSON response costs nothing here. Penalty rows ARE
// recorded — the ledger needs to show that a budget was charged without a
// measurement, which is precisely the fact today's single number hides.
func buildUsageRecordBlock(route string) string {
	return fmt.Sprintf(`-- AKO AI Gateway: append this response to the usage-record ring (token ledger)
do
  if meter_quality ~= "none" then
%s
    local cur = avi.vs.table_lookup(%q) or ""
    local inst, seq = "", 0
    do
      local d = string.find(cur, ":", 1, true)
      if d then
        inst = string.sub(cur, 1, d - 1)
        seq = tonumber(string.sub(cur, d + 1)) or 0
      end
    end
    -- First record on this SE mints the ring's instance id. It only has to be
    -- distinct per SE, and the client address of the first metered response plus
    -- the second it happened in is enough: two SEs do not serve the same
    -- connection.
    if inst == "" then inst = _safe(tostring(now) .. "-" .. (avi.vs.client_ip() or "se"), 40) end
    seq = seq + 1
    avi.vs.table_remove(%q)
    avi.vs.table_insert(%q, inst .. ":" .. seq, %d)
    local rec = tostring(now)
      .. "|" .. _safe(identity, 96)
      .. "|" .. _safe(model_name, 64)
      .. "|" .. _safe(avi.http.get_reqvar("ai_tier") or "", 32)
      .. "|" .. tostring(prompt_tokens)
      .. "|" .. tostring(completion_tokens)
      .. "|" .. tostring(cached_tokens)
      .. "|" .. tostring(reasoning_tokens)
      .. "|" .. meter_quality
      .. "|" .. %q
      .. "|" .. _safe(avi.http.get_reqvar("ai_chain") or "", 32)
    avi.vs.table_remove(%q .. seq)
    avi.vs.table_insert(%q .. seq, rec, %d)
  end
end`, buildSafeFieldHelper(), UsageSeqKey, UsageSeqKey, UsageSeqKey, UsageSeqTTLSeconds,
		safeRouteName(route), UsageRecPrefix, UsageRecPrefix, UsageRingTTLSeconds)
}

// safeRouteName reduces the HTTPRoute name to the same character set _safe
// enforces at runtime. The route is a Kubernetes object name baked in at
// generation time, so this only ever guards against a name the API server would
// not have accepted anyway — but the record's parseability should not depend on
// that assumption holding.
func safeRouteName(route string) string {
	if route == "" {
		return "-"
	}
	out := make([]byte, 0, len(route))
	for i := 0; i < len(route) && i < 64; i++ {
		c := route[i]
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z',
			c == '-', c == '.', c == '_', c == ':', c == '/', c == '@':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return string(out)
}

// ChainHeader is the request header carrying the chain id: W3C traceparent,
// "00-<32 hex trace-id>-<16 hex span-id>-<flags>". The trace id is the chain.
const ChainHeader = "traceparent"

// buildChainIDBlock returns HTTP_REQ Lua that keeps the caller's chain id in the
// `ai_chain` reqvar for the response phases to record. Only the 32-hex trace id
// is kept — it is what every hop of one user request shares — and only when the
// header has the standard shape; anything else records as no chain rather than
// as a made-up one. The SE does not mint an id: a request that arrives without
// one is a chain of its own, and saying so is more honest than inventing a root.
func buildChainIDBlock() string {
	return fmt.Sprintf(`-- AKO AI Gateway: chain id (W3C traceparent trace-id → reqvar ai_chain)
do
  local _tp = nil
  pcall(function() _tp = avi.http.get_header(%q, avi.HTTP_REQUEST) end)
  if _tp ~= nil and type(_tp) == "string" and #_tp >= 55 and string.sub(_tp, 3, 3) == "-" and string.sub(_tp, 36, 36) == "-" then
    local _tid = string.lower(string.sub(_tp, 4, 35))
    local _ok = true
    for _i = 1, 32 do
      local _b = string.byte(_tid, _i)
      if not ((_b >= 48 and _b <= 57) or (_b >= 97 and _b <= 102)) then _ok = false break end
    end
    if _ok and _tid ~= "00000000000000000000000000000000" then
      pcall(function() avi.http.set_reqvar("ai_chain", _tid) end)
    end
  end
end`, ChainHeader)
}
