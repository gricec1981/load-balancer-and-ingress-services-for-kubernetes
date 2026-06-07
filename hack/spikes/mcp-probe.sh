#!/usr/bin/env sh
# ─────────────────────────────────────────────────────────────────────────────
# mcp-probe.sh — discover Avi 32.1.1 MCP object model (READ-ONLY by default).
#
# Purpose: after upgrading the controller to 32.1.1, find out how MCP load
# balancing is actually modeled, so the spike-gated ⚠️ items in
# docs/gateway-api/ai-gateway-mcp.md can be filled in with real object names.
# It answers three questions:
#   1. Is there a dedicated MCP *application profile* type? (§2 / §10 Spike-1)
#   2. What *persistence* profile carries Mcp-Session-Id? (custom HTTP header?)
#   3. Is per-tool JWT authorization a NATIVE object/field, or must it be a
#      DataScript? (§6 / §10 — the decision that changes the implementation.)
#
# SAFETY: the core run issues ONLY GETs (+ one /login, which creates no config).
# It creates/modifies/deletes NOTHING. An optional enum probe (PROBE_ENUM=1)
# sends deliberately-invalid POSTs that the controller rejects with HTTP 400 —
# those create nothing; if one ever unexpectedly succeeds it is auto-deleted.
#
# Run anywhere that can reach the controller (in-cluster curl pod, or your box
# if the mgmt IP is reachable). Only needs: sh + curl. Uses jq if present.
#
# Usage:
#   export CTRL=10.225.0.4 AVIUSER=admin AVIPASS='***' AVIVER=32.1.1
#   sh mcp-probe.sh                 # read-only discovery
#   PROBE_ENUM=1 sh mcp-probe.sh    # also enumerate valid enum values (still creates nothing)
# ─────────────────────────────────────────────────────────────────────────────
set -eu

CTRL="${CTRL:?set CTRL=<controller ip/host>}"
AVIUSER="${AVIUSER:?set AVIUSER=<api user>}"
AVIPASS="${AVIPASS:?set AVIPASS=<api password>}"
AVIVER="${AVIVER:-32.1.1}"
B="https://${CTRL}"
JAR="$(mktemp)"
OUT="${OUT:-/tmp/mcp-probe-out}"; mkdir -p "$OUT"
trap 'rm -f "$JAR"' EXIT

have_jq=0; command -v jq >/dev/null 2>&1 && have_jq=1

# authenticated GET helper
g() { curl -sk -b "$JAR" -H "X-Avi-Version: ${AVIVER}" "$@"; }
sec() { printf '\n===== %s =====\n' "$*"; }

# ── 1. login (only non-GET in the core run; creates no config) ───────────────
curl -sk -c "$JAR" -H "X-Avi-Version: ${AVIVER}" -H "Content-Type: application/json" \
     -d "{\"username\":\"${AVIUSER}\",\"password\":\"${AVIPASS}\"}" "${B}/login" >/dev/null
echo "logged in to ${CTRL} (X-Avi-Version: ${AVIVER}); raw responses saved under ${OUT}/"

# ── 2. does a dedicated MCP object type / endpoint exist? ────────────────────
# Probe candidate endpoints; HTTP 200 => that object type exists on this build.
sec "candidate endpoint existence (200 = exists, 404 = no such type)"
for api in \
  applicationprofile applicationpersistenceprofile ssopolicy authprofile \
  jwtserverprofile jwtprofile authmappingprofile \
  mcpprofile mcpconfig mcpserver toolauthprofile authorizationpolicy \
  aiconfig llmprofile ; do
  code="$(g -o /dev/null -w '%{http_code}' "${B}/api/${api}?page_size=1" || echo ERR)"
  printf '  %-28s HTTP %s\n' "$api" "$code"
done

# ── 3. inventory: which object types' JSON even mentions "mcp" ───────────────
sec "objects whose JSON mentions 'mcp' (case-insensitive) + raw dump"
for api in applicationprofile applicationpersistenceprofile ssopolicy authprofile \
           jwtserverprofile jwtprofile networkprofile vsdatascriptset ; do
  raw="${OUT}/${api}.json"
  g "${B}/api/${api}?page_size=200" > "$raw" 2>/dev/null || { echo "  ${api}: fetch failed"; continue; }
  mentions="$(tr 'A-Z' 'a-z' < "$raw" | grep -o mcp | wc -l | tr -d ' ')"
  count="$(grep -o '"url"' "$raw" | wc -l | tr -d ' ')"
  printf '  %-28s objects=%-4s mcp-mentions=%s\n' "$api" "$count" "$mentions"
done

# ── 4. application profile types present (look for an MCP type) ──────────────
sec "applicationprofile: name -> type"
if [ "$have_jq" = 1 ]; then
  jq -r '.results[] | "  \(.name)\t-> \(.type)"' "${OUT}/applicationprofile.json"
  echo "  -- any profile with an 'mcp' field block: --"
  jq -r '.results[] | select(tostring|test("mcp";"i")) | "  HIT: \(.name)"' "${OUT}/applicationprofile.json" || true
else
  grep -oE '"(name|type)":"[^"]*"' "${OUT}/applicationprofile.json" | sed 's/^/  /'
fi

# ── 5. persistence profile types (which carries Mcp-Session-Id?) ────────────
sec "applicationpersistenceprofile: name -> persistence_type (+ header config)"
if [ "$have_jq" = 1 ]; then
  jq -r '.results[] | "  \(.name)\t-> \(.persistence_type)  hdr=\(.http_header_persistence_profile.prst_hdr_name // "-")"' \
     "${OUT}/applicationpersistenceprofile.json"
else
  grep -oE '"(name|persistence_type|prst_hdr_name)":"[^"]*"' "${OUT}/applicationpersistenceprofile.json" | sed 's/^/  /'
fi

# ── 6. THE KEY QUESTION: native per-tool JWT authorization? ─────────────────
# Scan auth-bearing objects for any field hinting at tool/role/claim-level
# authorization. If these appear as native config, §6's DataScript layer can be
# replaced by config. If nothing shows up, the DataScript approach stands.
sec "native tool/role authorization hints across auth objects"
for api in ssopolicy authprofile jwtserverprofile jwtprofile authmappingprofile authorizationpolicy ; do
  f="${OUT}/${api}.json"; [ -s "$f" ] || g "${B}/api/${api}?page_size=200" > "$f" 2>/dev/null || true
  [ -s "$f" ] || continue
  hits="$(tr ',{}' '\n' < "$f" | grep -iE 'tool|authoriz|claim|role|scope|mcp' | sed 's/^[ "]*//' | sort -u || true)"
  if [ -n "$hits" ]; then printf '  --- %s ---\n' "$api"; printf '%s\n' "$hits" | sed 's/^/      /'; fi
done
echo "  (interpretation: native 'tool'/'authorization' rule fields here => §6 can be NATIVE;"
echo "   only generic JWT/claim validation => keep the DataScript tool-authz layer.)"

# ── 7. OPTIONAL: enumerate valid enum values via rejected POSTs (creates nothing) ──
if [ "${PROBE_ENUM:-0}" = "1" ]; then
  sec "enum discovery via invalid POST (HTTP 400 expected; nothing is created)"
  csrf="$(grep -i csrftoken "$JAR" | awk '{print $NF}' | tail -1)"
  probe_enum() { # $1 = api, $2 = json body with a bogus enum
    resp="$(curl -sk -b "$JAR" -X POST \
        -H "X-Avi-Version: ${AVIVER}" -H "Content-Type: application/json" \
        -H "X-CSRFToken: ${csrf}" -H "Referer: ${B}" \
        -d "$2" "${B}/api/$1" )"
    # Safety: if it somehow created an object, delete it immediately.
    url="$(printf '%s' "$resp" | grep -oE '"url":"[^"]*"' | head -1 | sed 's/.*:"//;s/"$//')"
    [ -n "$url" ] && curl -sk -b "$JAR" -X DELETE -H "X-Avi-Version: ${AVIVER}" \
        -H "X-CSRFToken: ${csrf}" -H "Referer: ${B}" "$url" >/dev/null 2>&1 && \
        echo "  (auto-deleted unexpectedly-created $1)"
    printf '  %s allowed values -> ' "$1"
    printf '%s' "$resp" | tr ',' '\n' | grep -iE 'must be|not in|valid|enum|choices' | head -3 | sed 's/^/    /' \
      || echo "(no enum echoed; inspect: $resp)"
  }
  probe_enum applicationprofile            '{"name":"__mcp_probe_delete_me__","type":"__PROBE__"}'
  probe_enum applicationpersistenceprofile '{"name":"__mcp_probe_delete_me__","persistence_type":"__PROBE__"}'
  probe_enum ssopolicy                     '{"name":"__mcp_probe_delete_me__","type":"__PROBE__"}'
fi

sec "done — read-only core; no objects created/modified. Raw JSON in ${OUT}/"
echo "Next: paste the real type names into docs/gateway-api/ai-gateway-mcp.md §2/§5,"
echo "and resolve §6 (native vs DataScript) from section 6 above."
