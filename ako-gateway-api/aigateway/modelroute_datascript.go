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

// ReqBodyBufferBytes is how much of the request body the SE buffers so the
// HTTP_REQ_DATA script can read the `model` field. On the Avi 31.2.2 build
// avi.http.set_request_body_buffer_size caps at 32768 (32 KB); since `model`
// sits at the *start* of the JSON, the buffered head is enough to read it.
// (Spike-verified; see docs/gateway-api/model-routing.md.)
const ReqBodyBufferBytes = 32768

// DSEvtHTTPReqData is the Avi SE DataScript event in which the buffered request
// body becomes readable via avi.http.get_req_body(). HTTP_REQ enables buffering;
// HTTP_REQ_DATA reads it. (The request-side mirror of HTTP_RESP/HTTP_RESP_DATA.)
const DSEvtHTTPReqData = "VS_DATASCRIPT_EVT_HTTP_REQ_DATA"

// DataScript-set name suffixes for the model-route scripts (one per event phase).
const (
	DSNameSuffixModelRouteReq     = "-ai-mr-req"
	DSNameSuffixModelRouteReqData = "-ai-mr-reqdata"
)

// DSModelRouteReqName / DSModelRouteReqDataName build the DataScript-set names for
// the model-route scripts on a given VS.
func DSModelRouteReqName(vsName string) string     { return vsName + DSNameSuffixModelRouteReq }
func DSModelRouteReqDataName(vsName string) string { return vsName + DSNameSuffixModelRouteReqData }

// ModelRouteScripts holds the two Lua snippets generated from an
// AIModelRoutePolicy: the HTTP_REQ phase that enables request-body buffering and
// the HTTP_REQ_DATA phase that reads the body, resolves the tier, applies
// entitlement, and selects the tier's Avi Pool Group.
type ModelRouteScripts struct {
	// ReqScript is the HTTP_REQ phase Lua. The body is not yet readable here; it
	// only turns on request-body buffering so HTTP_REQ_DATA can read it.
	ReqScript string

	// ReqDataScript is the HTTP_REQ_DATA phase Lua. It reads the buffered body,
	// extracts the model, resolves the tier (with entitlement/downgrade), and
	// calls avi.poolgroup.select() on the tier's Pool Group.
	ReqDataScript string
}

// GenerateModelRouteScripts produces the two DataScript snippets for the policy.
//
// tierPG maps a tier name to the Avi Pool Group name AKO builds for that tier.
// Every Pool Group named here must also be listed in the DataScriptSet's
// pool_group_refs, or Avi rejects the script (HTTP 400) — see the model-routing
// design doc.
func GenerateModelRouteScripts(policy *AIModelRoutePolicy, tierPG map[string]string, providers map[string]*ProviderRuntime, remotes map[string]*RemoteRuntime, mode AuthClaimMode) ModelRouteScripts {
	spec := policy.Spec

	reqScript := fmt.Sprintf(
		"-- AKO AI Gateway: enable request-body buffering for model routing\n"+
			"pcall(function() avi.http.set_request_body_buffer_size(%d) end)",
		ReqBodyBufferBytes)

	var b strings.Builder
	b.WriteString("-- AKO AI Gateway: model-based tier routing (HTTP_REQ_DATA)\n")

	// ── Baked-in constant tables ─────────────────────────────────────────────
	b.WriteString(luaStringMap("MODEL_TIERS", exactModelTiers(spec.ModelTiers)))
	b.WriteString(luaPrefixes("PREFIXES", spec.prefixEntries()))
	fmt.Fprintf(&b, "local DEFAULT_TIER = %s\n", luaStr(spec.DefaultTier))
	b.WriteString(luaStringMap("TIER_PG", tierPG))
	b.WriteString(luaProviders("PROVIDERS", providers))
	b.WriteString(luaRemotes("REMOTES", remotes))
	b.WriteString(luaList("PREF", spec.tierNames()))

	entitled := spec.Entitlements != nil && len(spec.Entitlements.Rules) > 0
	if entitled {
		b.WriteString(luaEntitle("ENTITLE", spec.Entitlements.Rules))
		fmt.Fprintf(&b, "local GROUP_CLAIM = %s\n", luaStr(spec.Entitlements.EffectiveGroupClaim()))
	}

	// ── Helpers ──────────────────────────────────────────────────────────────
	b.WriteString(jsonStrHelper())
	if entitled {
		b.WriteString(jwtClaimHelper(mode))
		b.WriteString("\n")
	}

	// ── Read body + extract model ───────────────────────────────────────────
	fmt.Fprintf(&b, `
local _ok, _body = pcall(function() return avi.http.get_req_body(%d) end)
if not _ok or type(_body) ~= "string" then _body = "" end
local model = json_str(_body, %s)
`, ReqBodyBufferBytes, luaStr(spec.EffectiveModelField()))

	// ── Resolve tier (exact → longest prefix → default) ─────────────────────
	b.WriteString(`
local tier = MODEL_TIERS[model]
if not tier then
  for i = 1, #PREFIXES do
    local p = PREFIXES[i]
    if string.sub(model, 1, string.len(p.k)) == p.k then tier = p.v break end
  end
end
if not tier then tier = DEFAULT_TIER end
`)

	// ── Entitlement (only when configured) ──────────────────────────────────
	// _rt remembers the tier the caller's model actually asked for, because a
	// downgrade overwrites `tier` in place. Without it the log below can say
	// which tier served the request but never that it was not the one requested.
	if entitled {
		b.WriteString("\nlocal _rt = tier\n")
		status := spec.OnUnentitled.EffectiveStatusCode()
		reject := fmt.Sprintf(
			`avi.http.response(%d, {["Content-Type"]="application/json"}, '{"error":"tier_not_entitled"}') return`,
			status)
		if spec.OnUnentitled.EffectiveType() == "Reject" {
			fmt.Fprintf(&b, `
local _a = ENTITLE[jwt_claim(GROUP_CLAIM)]
if not (_a and _a[tier]) then
  %s
end
`, reject)
		} else { // Downgrade
			fmt.Fprintf(&b, `
local _a = ENTITLE[jwt_claim(GROUP_CLAIM)]
if not (_a and _a[tier]) then
  local _best = nil
  if _a then for i = 1, #PREF do if _a[PREF[i]] then _best = PREF[i] break end end end
  if _best then tier = _best else %s end
end
`, reject)
		}
	}

	// ── Route to the tier's Pool Group + record tier for token policy ────────
	b.WriteString(`
local pg = TIER_PG[tier]
if pg then avi.poolgroup.select(pg) end`)

	// ── External-provider tier: rewrite path + Host and inject the API key ───
	// (spike-verified on Avi 31.2.1: set_path/replace_header/add_header reach the
	// provider over the FQDN pool's backend TLS). ai_skip_meter tells the token
	// policy's response script to skip body metering — a provider response is
	// chunked, which the response-body metering can't parse anyway.
	if len(providers) > 0 {
		b.WriteString(`
local _pv = PROVIDERS[tier]
if _pv then
  avi.http.set_path(_pv.p)
  pcall(function() avi.http.set_query("") end)
  avi.http.replace_header("Host", _pv.h)
  if _pv.ah ~= "" then avi.http.remove_header(_pv.ah) avi.http.add_header(_pv.ah, _pv.av) end
  pcall(function() avi.http.set_reqvar("ai_skip_meter", "1") end)
end`)
	}

	// ── Remote-site tier: rewrite Host so the peer's EVH child VS matches ────
	// That is the whole rewrite. A peer gateway speaks our dialect, so the path,
	// the body and the caller's Authorization header all pass through untouched,
	// and the response is metered here exactly as a local tier's would be — a
	// remote tier must not silently stop counting tokens. Host rewriting is
	// skipped when the peer serves the same FQDN (remote.preserveHost).
	if len(remotes) > 0 {
		b.WriteString(`
local _rm = REMOTES[tier]
if _rm and _rm.h ~= "" then avi.http.replace_header("Host", _rm.h) end`)
	}

	// ── Name the routing decision in the VS client log ──────────────────────
	// Everything else the client log records about this request is a hash: under
	// EVH the pool and pool-group names are `<prefix>--<sha1>`, so the log can
	// show that two requests went to different backends but never that one asked
	// for qwen3-14b and was served by the GPU tier. This line puts the decision
	// itself — model, tier, and whether entitlement moved it — in the entry.
	//
	// The closure form of pcall is load-bearing, not style: an unknown avi.* field
	// RAISES on access, and `pcall(avi.vs.log, _l)` evaluates that access before
	// pcall is entered. Wrapped this way, an SE build without the API costs a
	// missing log line rather than a 500 on the front door.
	b.WriteString(`
do
  local _l = "ai-gateway: model=" .. (model ~= "" and model or "-") .. " tier=" .. tier`)
	if entitled {
		b.WriteString(`
  if _rt ~= tier then _l = _l .. " requested=" .. _rt .. " downgraded=1" end`)
	}
	b.WriteString(`
  _l = _l .. " pool-group=" .. (pg or "-")`)
	if len(providers) > 0 {
		b.WriteString(`
  if _pv then _l = _l .. " provider=" .. _pv.h end`)
	}
	if len(remotes) > 0 {
		b.WriteString(`
  if _rm then _l = _l .. " remote=" .. _rm.h end`)
	}
	b.WriteString(`
  pcall(function() avi.vs.log(_l) end)
end`)

	b.WriteString("\npcall(function() avi.http.set_reqvar(\"ai_tier\", tier) end)")

	return ModelRouteScripts{ReqScript: reqScript, ReqDataScript: b.String()}
}

// jsonStrHelper returns a Lua function `json_str(body, key)` that extracts the
// string value of a top-level JSON key using a sandbox-safe scan (Avi's Lua
// sandbox lacks string.match). It finds the quoted key, then the next two
// double-quotes, and returns the substring between them. Good enough for the
// `model` field (a short, unescaped identifier).
func jsonStrHelper() string {
	return `
local DQ = string.char(34)
local function json_str(body, key)
  local pat = DQ .. key .. DQ
  local s = string.find(body, pat, 1, true)
  if not s then return "" end
  local q1 = string.find(body, DQ, s + string.len(pat), true)
  if not q1 then return "" end
  local q2 = string.find(body, DQ, q1 + 1, true)
  if not q2 then return "" end
  return string.sub(body, q1 + 1, q2 - 1)
end
`
}

// ─── Lua literal builders ─────────────────────────────────────────────────────

var luaEscaper = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	"\n", `\n`,
	"\r", `\r`,
	"\t", `\t`,
)

// luaStr renders a Go string as a double-quoted Lua string literal.
func luaStr(s string) string {
	return `"` + luaEscaper.Replace(s) + `"`
}

// exactModelTiers returns only the non-glob (exact) entries of a model→tier map.
func exactModelTiers(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if !strings.HasSuffix(k, "*") {
			out[k] = v
		}
	}
	return out
}

// luaStringMap renders `local <name> = { ["k"]="v", ... }` with keys sorted for
// deterministic output.
func luaStringMap(name string, m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("[%s]=%s", luaStr(k), luaStr(m[k])))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(parts, ", "))
}

// luaPrefixes renders the prefix-glob rules as an ordered Lua array of {k=,v=}.
func luaPrefixes(name string, entries []prefixEntry) string {
	var parts []string
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("{k=%s, v=%s}", luaStr(e.Prefix), luaStr(e.Tier)))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(parts, ", "))
}

// luaProviders renders the per-tier provider rewrite table:
//   local <name> = { ["gemini"]={p="/path", h="host", ah="Authorization", av="Bearer k"} }
// The API key is baked into the SE-side Lua (tenant-scoped Avi config), the same
// way group budgets and model→tier maps are baked in.
func luaProviders(name string, providers map[string]*ProviderRuntime) string {
	tiers := make([]string, 0, len(providers))
	for t := range providers {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)
	var parts []string
	for _, t := range tiers {
		p := providers[t]
		parts = append(parts, fmt.Sprintf("[%s]={p=%s, h=%s, ah=%s, av=%s}",
			luaStr(t), luaStr(p.Path), luaStr(p.Host), luaStr(p.AuthHeader), luaStr(p.AuthValue)))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(parts, ", "))
}

// luaRemotes renders the per-tier remote-site table:
//
//	local <name> = { ["premium-eu"]={h="llm.siteb.ai.avi.com"} }
//
// h is the Host header to send to the peer, or "" to preserve the client's.
func luaRemotes(name string, remotes map[string]*RemoteRuntime) string {
	tiers := make([]string, 0, len(remotes))
	for t := range remotes {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)
	var parts []string
	for _, t := range tiers {
		parts = append(parts, fmt.Sprintf("[%s]={h=%s}", luaStr(t), luaStr(remotes[t].Host)))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(parts, ", "))
}

// luaList renders `local <name> = { "a", "b", ... }`.
func luaList(name string, items []string) string {
	var parts []string
	for _, it := range items {
		parts = append(parts, luaStr(it))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(parts, ", "))
}

// luaEntitle renders `local <name> = { ["group"]={ ["tier"]=true, ... }, ... }`
// with deterministic ordering.
func luaEntitle(name string, rules []EntitlementRule) string {
	groups := make([]EntitlementRule, len(rules))
	copy(groups, rules)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Group < groups[j].Group })
	var groupParts []string
	for _, r := range groups {
		tiers := append([]string(nil), r.Allow...)
		sort.Strings(tiers)
		var tierParts []string
		for _, t := range tiers {
			tierParts = append(tierParts, fmt.Sprintf("[%s]=true", luaStr(t)))
		}
		groupParts = append(groupParts, fmt.Sprintf("[%s]={ %s }", luaStr(r.Group), strings.Join(tierParts, ", ")))
	}
	return fmt.Sprintf("local %s = { %s }\n", name, strings.Join(groupParts, ", "))
}
