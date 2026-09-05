#!/usr/bin/env sh
# ─────────────────────────────────────────────────────────────────────────────
# auth-probe.sh — does Avi 32.1.1 close the BEARER GAP? (READ-ONLY by default.)
#
# THE ONE QUESTION THIS EXISTS TO ANSWER
# --------------------------------------
# Today AKO must put the token in the URL (`?jwt=`) for ONE reason: Avi validates
# a bearer JWT *or* exposes claims to a DataScript, never both. Specifically,
# SSO_TYPE_JWT "strips the Authorization header before any DataScript runs"
# (ako-gateway-api/aigateway/oauth_rest.go:34), so AKO sets
# jwt_location=JWT_LOCATION_QUERY_PARAM (translator.go:184) and the DataScript
# base64url-decodes the token out of the URI itself (jwtClaimHelper).
#
# The cost is the unmaskable token-in-log leak: masking populates `orig_uri`
# with the raw token on 30.2.1+ and no config suppresses it.
#
# 32.1.1 ships MCP load balancing with "OAuth 2.0-based authorization" and an
# RFC 9728 JWT Protected Metadata Resource. If — and only if — a DataScript can
# still read the claims when the token arrives in a HEADER, then `?jwt=` can be
# retired entirely and the leak goes away.
#
#   PASS  = claims (or the Authorization header) are readable in HTTP_REQ_DATA
#           while the SE validates a header-presented token.
#   FAIL  = same either/or as 31.2.1; keep jwtQuery, and fix the leak instead
#           with RFC 8705 cert-bound tokens (avi.ssl.client_cert, spike S8).
#
# SAFETY: the core run issues ONLY GETs (+ one /login, which creates no config).
# PROBE_ENUM=1 adds deliberately-invalid POSTs that the controller rejects with
# 400 — they create nothing, and anything that unexpectedly succeeds is deleted.
# PROBE_LIVE=1 is the only mode that creates real objects; it tears them down in
# a trap, and every object it makes is named with the AUTHPROBE_ prefix.
#
# Usage:
#   export CTRL=<ip> AVIUSER=admin AVIPASS='***' AVIVER=32.1.1
#   sh auth-probe.sh                  # read-only: sections 1-5
#   PROBE_ENUM=1 sh auth-probe.sh     # + enumerate enums (creates nothing)
#   PROBE_LIVE=1 sh auth-probe.sh     # + the decisive header test (section 6)
#
# RESULT 2026-09-05 (vks-ai-01, Avi 31.2.1, throwaway EVH child; scripts in the session scratchpad):
#   - HTTP_AUTH / HTTP_POST_AUTH: get_header, add_header, replace_header, remove_header,
#     get_cookie are ALL "disabled in the event of http_auth" -> the copy-the-header idea
#     is dead by design, not by ordering. Reqvars set there DO persist into HTTP_REQ.
#   - BUT avi.http.get_userid() returns the validated JWT `sub` in HTTP_POST_AUTH and
#     HTTP_REQ (nil pre-auth; two tokens, two subjects, both correct; no token -> 401)
#     with the token in the AUTHORIZATION HEADER. Verified identity in Lua, no ?jwt=.
#     Only non-sub claims stay hidden. See docs/gateway-api/ai-gateway-auth.md.
#   - avi.utils.sha1_hash/md5_hash/base64_*/rand_bytes and avi.http.method() all work.
# ─────────────────────────────────────────────────────────────────────────────
set -eu

CTRL="${CTRL:?set CTRL=<controller ip/host>}"
AVIUSER="${AVIUSER:?set AVIUSER=<api user>}"
AVIPASS="${AVIPASS:?set AVIPASS=<api password>}"
AVIVER="${AVIVER:-32.1.1}"
B="https://${CTRL}"
JAR="$(mktemp)"
OUT="${OUT:-/tmp/auth-probe-out}"; mkdir -p "$OUT"
trap 'rm -f "$JAR"' EXIT

have_jq=0; command -v jq >/dev/null 2>&1 && have_jq=1
g() { curl -sk -b "$JAR" -H "X-Avi-Version: ${AVIVER}" "$@"; }
p() { curl -sk -b "$JAR" -H "X-Avi-Version: ${AVIVER}" -H "Content-Type: application/json" "$@"; }
sec() { printf '\n===== %s =====\n' "$*"; }

curl -sk -c "$JAR" -H "X-Avi-Version: ${AVIVER}" -H "Content-Type: application/json" \
  -X POST "${B}/login" -d "{\"username\":\"${AVIUSER}\",\"password\":\"${AVIPASS}\"}" >/dev/null

# ── 1. Version — settle it before trusting anything else ─────────────────────
sec "1. CONTROLLER VERSION (expect 32.1.1)"
g "${B}/api/initial-data" | tr ',' '\n' | grep -o '"version":"[^"]*"' | head -3
g "${B}/api/cluster/version" | head -c 300; echo

# ── 2. THE HIGHEST-VALUE READ: Avi's own system DataScripts ──────────────────
# If 32.1.1 exposes any way for Lua to see a validated token, Avi's own MCP/auth
# scripts are where it will be used, and they name the function for us.
sec "2. SYSTEM DATASCRIPTS — grep Avi's own Lua for claim/JWT access"
g "${B}/api/vsdatascriptset?page_size=200" > "$OUT/datascriptsets.json"
if [ "$have_jq" = 1 ]; then
  jq -r '.results[] | .name' "$OUT/datascriptsets.json" | sort
  echo "--- any system script touching jwt/claim/oauth/authorization/get_header ---"
  jq -r '.results[] | .name as $n | (.datascript[]?.script // "") | select(
      test("jwt|claim|oauth|[Aa]uthorization|get_header"; "i")) | "### " + $n + "\n" + .' \
    "$OUT/datascriptsets.json" | head -120
else
  grep -o '"name":"[^"]*"' "$OUT/datascriptsets.json" | sort -u
  echo "(install jq for the Lua grep; raw saved to $OUT/datascriptsets.json)"
fi
echo ">>> READ System-Standard-MCP's Lua in full — it is Avi's reference MCP implementation."

# ── 3. The JWT/OAuth object model — what fields exist now ────────────────────
sec "3. AUTH OBJECT MODEL"
for obj in authprofile ssopolicy; do
  echo "--- /api/$obj (names + type) ---"
  g "${B}/api/${obj}?page_size=100" | tr '{' '\n' | grep -o '"name":"[^"]*"\|"type":"[^"]*"' | head -20
done
echo "--- System-Secure-HTTP-MCP application profile (full) ---"
g "${B}/api/applicationprofile?name=System-Secure-HTTP-MCP" > "$OUT/mcp-appprofile.json"
head -c 2000 "$OUT/mcp-appprofile.json"; echo
echo ">>> LOOK FOR: app_service_type, and ANY oauth / metadata / resource / well_known field."

# ── 4. RFC 9728 — where is the protected-resource metadata configured? ───────
# The docs say 32.1.1 "publishes OAuth authorization server metadata at a
# well-known endpoint". Find the object that carries it. 404 = not this one.
sec "4. RFC 9728 PROTECTED RESOURCE METADATA — which endpoint models it?"
for ep in protectedresource oauthresource jwtprotectedresource resourcemetadata \
          oauthprofile mcpprofile authorizationserver wellknownprofile; do
  code=$(g -o /dev/null -w '%{http_code}' "${B}/api/${ep}?page_size=1")
  [ "$code" = "404" ] || echo "  /api/${ep} -> HTTP ${code}   <-- EXISTS, inspect it"
done
echo "--- grep the whole API surface for 9728/metadata wording ---"
g "${B}/api/doc" > "$OUT/apidoc.json" 2>/dev/null || true
[ -s "$OUT/apidoc.json" ] && grep -o -i '[a-z_]*\(protected_resource\|resource_metadata\|well_known\|oauth_metadata\)[a-z_]*' \
  "$OUT/apidoc.json" | sort -u | head -20

# ── 5. THE DECIDING FIELD: jwt_location — is there a header option? ──────────
sec "5. jwt_location ENUM (the field AKO pins to QUERY_PARAM)"
echo "--- VSes that already carry a jwt_config ---"
g "${B}/api/virtualservice?page_size=200" | tr '{' '\n' | grep -o '"jwt_location":"[^"]*"' | sort -u
if [ "${PROBE_ENUM:-0}" = "1" ]; then
  echo "--- enumerate by sending an invalid value (rejected, creates nothing) ---"
  p -X POST "${B}/api/virtualservice" \
    -d '{"name":"AUTHPROBE_enum","jwt_config":{"jwt_location":"ZZZ_INVALID","audience":"x","jwt_name":"jwt"}}' \
    | head -c 400; echo
  echo ">>> The 400 should NAME every valid value. A *_HEADER option is the prize."
fi

# ── 6. DECISIVE LIVE TEST (PROBE_LIVE=1) ─────────────────────────────────────
# Does a DataScript see the Authorization header when the SE validated it?
sec "6. LIVE HEADER-CLAIM TEST"
if [ "${PROBE_LIVE:-0}" != "1" ]; then
  echo "skipped (set PROBE_LIVE=1). What it does:"
  echo "  a) create DataScriptSet AUTHPROBE_ds with, in HTTP_REQ_DATA:"
  echo "       local h = avi.http.get_header('Authorization')"
  echo "       avi.vs.log('AUTHPROBE hdr=' .. tostring(h))"
  echo "  b) attach it to a throwaway VS that has jwt_config in HEADER mode"
  echo "     (or SSO_TYPE_JWT / the MCP OAuth profile) pointed at your JWKS"
  echo "  c) curl it with 'Authorization: Bearer <token>'"
  echo "  d) read the VS log's DataScript tab for the AUTHPROBE line"
  echo "  e) tear everything down"
  echo ""
  echo "  hdr=nil        -> gap UNCHANGED. Keep ?jwt=. Fix the leak with"
  echo "                    RFC 8705 cert-bound tokens (avi.ssl, spike S8)."
  echo "  hdr=Bearer ... -> GAP CLOSED. Retire jwtQuery; the URI leak disappears."
else
  echo "PROBE_LIVE is intentionally left for a human to drive: it needs YOUR jwks_uri,"
  echo "issuer and a valid token, and it creates a VS on a shared controller."
  echo "Build it from section 5's enum result, then read the DataScript log."
fi

sec "SUMMARY — the four answers to bring back"
cat <<'EOT'
  1. Controller version (confirm 32.1.1).
  2. Does any SYSTEM DataScript read a validated token? Which avi.* call?
  3. Does jwt_location have a *_HEADER value? (section 5)
  4. LIVE: is Authorization readable in HTTP_REQ_DATA under header validation?
     -> that single yes/no decides whether `?jwt=` can be retired.
EOT
