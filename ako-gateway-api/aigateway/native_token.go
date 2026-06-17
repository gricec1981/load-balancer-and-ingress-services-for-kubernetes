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

	"google.golang.org/protobuf/proto"

	avimodels "github.com/vmware/alb-sdk/go/models"
)

// Native token-budget enforcement (TokenLimit.Backend == "native").
//
// A token budget is mapped onto the native Avi rate limiter: count = budget,
// period = window, request_key = the keyed identity, consume = tokens used. The
// SE owns the (cross-SE-consistent) token bucket. Enforcement is split across two
// phases, like the DataScript backend:
//   - gate (HTTP_REQ, or HTTP_REQ_DATA for reqvar/tier limits): a consume=1 probe
//     rejects when the bucket is empty. The chosen limiter name is stashed in a
//     reqvar so the consume phase hits the same bucket.
//   - consume (HTTP_RESP_DATA): consume the parsed token count, AND keep the
//     DataScript table counter for the admin/dashboard endpoint (hybrid display).
//
// Tradeoffs (rolling window, approximate display count) and rationale:
// docs/gateway-api/native-token-budget-design.md.

const nativeTokLimiterSuffix = "-tk"

// nativeLimiterPartSanitizer keeps Avi rate-limiter names to a safe charset.
var nativeLimiterPartSanitizer = strings.NewReplacer(
	" ", "-", "/", "-", ":", "-", "*", "-", ",", "-",
)

func sanitizeLimiterPart(s string) string {
	return nativeLimiterPartSanitizer.Replace(s)
}

// nativeTokFallbackGroup is the synthetic group key for the unknown-group fallback
// limiter (built from TokenLimit.Budget). Never a real group value.
const nativeTokFallbackGroup = "__default__"

// nativeTokenLimiterName returns the Avi rate-limiter name for a native-backend
// token limit (group "" for a non-grouped limit). The same function is used to
// build the RateLimiter objects (buildNativeTokenLimiters) and the names the Lua
// references, so they always match.
func nativeTokenLimiterName(vsName, limitName, group string) string {
	n := vsName + "-" + sanitizeLimiterPart(limitName) + nativeTokLimiterSuffix
	if group != "" {
		n += "-" + sanitizeLimiterPart(group)
	}
	return n
}

// buildNativeTokenLimiters builds the RateLimiter objects for one native-backend
// limit: one per group budget (+ an unknown-group fallback when Budget > 0), or a
// single limiter when the limit is not grouped. count/burst = budget, period =
// window seconds.
func buildNativeTokenLimiters(limit TokenLimit, vsName string) []*avimodels.RateLimiter {
	period := uint32(windowSeconds(limit.Window))
	var out []*avimodels.RateLimiter
	add := func(group string, budget int64) {
		if budget <= 0 {
			return
		}
		c := uint32(budget)
		out = append(out, &avimodels.RateLimiter{
			Name:    proto.String(nativeTokenLimiterName(vsName, limit.Name, group)),
			Count:   proto.Uint32(c),
			Period:  proto.Uint32(period),
			BurstSz: proto.Uint32(c),
		})
	}
	if limit.GroupHeader != "" && len(limit.GroupBudgets) > 0 {
		groups := make([]string, 0, len(limit.GroupBudgets))
		for g := range limit.GroupBudgets {
			groups = append(groups, g)
		}
		sort.Strings(groups)
		for _, g := range groups {
			add(g, limit.GroupBudgets[g])
		}
		add(nativeTokFallbackGroup, limit.Budget) // unknown-group fallback (if > 0)
	} else {
		add("", limit.Budget)
	}
	return out
}

// nativeRlnameResolution emits Lua that sets `local rlname` to the rate-limiter
// name for this request's group (or the single limiter for a non-grouped limit).
// For grouped limits, an unknown group sets rlname to the fallback name when
// Budget > 0, else leaves it nil (the gate rejects; the consume skips). Both the
// gate and the consume call this independently — they must NOT rely on a reqvar
// surviving across HTTP_REQ → HTTP_RESP_DATA, so each re-resolves from the
// (response-readable) group claim. Expects jwt_claim/identity already in scope.
func nativeRlnameResolution(limit TokenLimit, vsName string) string {
	grouped := limit.GroupHeader != "" && len(limit.GroupBudgets) > 0
	if !grouped {
		return fmt.Sprintf("  local rlname = %s\n", luaStr(nativeTokenLimiterName(vsName, limit.Name, "")))
	}
	var b strings.Builder
	b.WriteString(groupReadExpr(limit.GroupHeader)) // -> local group_hdr
	b.WriteString("  local _lm = {")
	groups := make([]string, 0, len(limit.GroupBudgets))
	for g := range limit.GroupBudgets {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for i, g := range groups {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "[%s]=%s", luaStr(g), luaStr(nativeTokenLimiterName(vsName, limit.Name, g)))
	}
	b.WriteString("}\n")
	b.WriteString("  local rlname = _lm[group_hdr]\n")
	if limit.Budget > 0 {
		fmt.Fprintf(&b, "  if not rlname then rlname = %s end\n",
			luaStr(nativeTokenLimiterName(vsName, limit.Name, nativeTokFallbackGroup)))
	}
	return b.String()
}

// buildNativeGateBlock emits the HTTP_REQ Lua: resolve the limiter name, reject an
// unknown group (Budget 0), then reject via a consume=1 probe when the consumer's
// bucket is empty. Lives in the same VSDataScriptSet as the consume so they share
// the bucket. `identity`/jwt_claim are expected in scope (caller prepends header).
func buildNativeGateBlock(limit TokenLimit, vsName string) string {
	keyExpr := counterKeyIdentityExpr(limit.Key) // request_key dimension

	statusCode := 429
	doRetryAfter := false
	if limit.Action != nil {
		if limit.Action.StatusCode >= 400 {
			statusCode = limit.Action.StatusCode
		}
		doRetryAfter = limit.Action.RetryAfter
	}

	grouped := limit.GroupHeader != "" && len(limit.GroupBudgets) > 0

	var b strings.Builder
	fmt.Fprintf(&b, "-- limit: %s (native token budget — gate)\n", limit.Name)
	b.WriteString("do\n")
	fmt.Fprintf(&b, "  local rk = %s\n", keyExpr)
	b.WriteString(nativeRlnameResolution(limit, vsName))
	// Unknown group with no fallback budget -> reject.
	if grouped && limit.Budget <= 0 {
		b.WriteString("  if not rlname then\n")
		b.WriteString("    avi.http.response(403, {[\"Content-Type\"]=\"application/json\"},\n")
		b.WriteString("      '{\"error\":\"unknown_group\",\"group\":\"'..group_hdr..'\"}')\n")
		b.WriteString("    return\n  end\n")
	}
	// Gate: a consume=1 probe trips when the bucket is empty.
	b.WriteString("  if avi.vs.ratelimit.exceed(rlname, rk, 1) then\n")
	headers := `{["Content-Type"] = "application/json"`
	if doRetryAfter {
		headers += `, ["Retry-After"] = "1"`
	}
	headers += "}"
	fmt.Fprintf(&b, "    avi.http.response(%d, %s,\n", statusCode, headers)
	fmt.Fprintf(&b, "      %s)\n", luaStr(fmt.Sprintf(`{"error":"token_budget_exceeded","limit":"%s"}`, limit.Name)))
	b.WriteString("    return\n  end\n")
	b.WriteString("end\n")
	return b.String()
}

// buildNativeConsumeBlock emits the HTTP_RESP_DATA Lua for a native limit:
// re-resolve the limiter name (no cross-phase reqvar dependency) and consume the
// parsed token count, then run the DataScript table increment as a display-only
// counter so the admin endpoint keeps working (hybrid).
func buildNativeConsumeBlock(limit TokenLimit, epoch, vsName string) string {
	keyExpr := counterKeyIdentityExpr(limit.Key)
	dimVar := tokenDimensionExpr(limit.Tokens)

	var b strings.Builder
	fmt.Fprintf(&b, "-- account: %s (native consume)\n", limit.Name)
	b.WriteString("do\n")
	fmt.Fprintf(&b, "  local rk = %s\n", keyExpr)
	b.WriteString(nativeRlnameResolution(limit, vsName))
	fmt.Fprintf(&b, "  if rlname and rlname ~= \"\" then avi.vs.ratelimit.exceed(rlname, rk, %s) end\n", dimVar)
	b.WriteString("end\n")

	// Hybrid display counter (per-SE table) so the admin counters endpoint shows
	// usage; enforcement is the native limiter above.
	return b.String() + buildRespLimitBlock(limit, epoch)
}
