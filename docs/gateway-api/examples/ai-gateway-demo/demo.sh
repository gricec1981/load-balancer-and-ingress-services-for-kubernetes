#!/usr/bin/env bash
# demo.sh — interactive walkthrough of the AKO AI Gateway features
#
# Assumes:
#   - setup.sh has already run successfully
#   - VIP environment variable is set (or will be auto-detected)
#   - Standard tools: kubectl, curl, python3

set -euo pipefail

NAMESPACE="inference"

# ─── Helpers ─────────────────────────────────────────────────────────────────

BOLD=$'\033[1m'
GRN=$'\033[0;32m'
YLW=$'\033[0;33m'
RED=$'\033[0;31m'
BLU=$'\033[0;34m'
RST=$'\033[0m'

banner() { echo; echo "${BOLD}${BLU}━━━ $* ━━━${RST}"; echo; }
run()    { echo "${BOLD}${GRN}\$ $*${RST}"; eval "$@"; echo; }
note()   { echo "  ${YLW}ℹ  $*${RST}"; }
pause()  { read -rp "  ${BOLD}[press Enter to continue]${RST} "; echo; }

# ─── Detect VIP ──────────────────────────────────────────────────────────────
if [[ -z "${VIP:-}" ]]; then
  note "VIP not set — trying to detect from the llm-route Gateway status..."
  VIP=$(kubectl get gateway llm-gateway -n "$NAMESPACE" \
    -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || echo "")
  [[ -n "$VIP" ]] || { echo "❌ Could not detect VIP. Set: export VIP=<gateway-external-ip>"; exit 1; }
  note "Detected VIP: $VIP"
fi

LLM_URL="http://${VIP}/v1/chat/completions"

# ─── Detect issuer (Part B) ───────────────────────────────────────────────────
ISSUER_VIP=$(kubectl get svc jwt-issuer -n "$NAMESPACE" \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")

echo ""
echo "${BOLD}AKO AI Gateway — Phase 1 Demo${RST}"
echo "  VIP:     $VIP"
echo "  Issuer:  ${ISSUER_VIP:-not deployed (SKIP_JWT=1 mode)}"
echo ""
pause


# ══════════════════════════════════════════════════════════════════════════════
banner "PART A — Token Rate Limiting (no JWT required)"
# ══════════════════════════════════════════════════════════════════════════════

note "Policy: 500 total tokens per consumer per hour. Each response = 100 tokens."
note "Counter keys on source IP (fallback) since x-ai-consumer header is not set yet."
note "Budget: 500 tokens → 5 successful requests, 6th is rejected."
pause

PAYLOAD='{"model":"mock-llm-v1","messages":[{"role":"user","content":"hello"}]}'

for i in 1 2 3 4 5; do
  echo "${BOLD}Request $i / 5${RST}"
  run curl -s -o /tmp/ai-resp.json -w "HTTP %{http_code}" \
    -X POST "$LLM_URL" \
    -H "Content-Type: application/json" \
    -d "'$PAYLOAD'"
  python3 -c "
import json, sys
try:
    r = json.load(open('/tmp/ai-resp.json'))
    if 'choices' in r:
        u = r['usage']
        print(f'  → model: {r[\"model\"]}  |  tokens: prompt={u[\"prompt_tokens\"]} + completion={u[\"completion_tokens\"]} = total={u[\"total_tokens\"]}')
    else:
        print(f'  → {json.dumps(r)}')
except: pass
"
  sleep 0.5
done

echo ""
echo "${BOLD}Request 6 / 5 — SHOULD BE REJECTED (budget exhausted) ${RST}"
run curl -s -w "\nHTTP %{http_code}\n" \
  -X POST "$LLM_URL" \
  -H "Content-Type: application/json" \
  -d "'$PAYLOAD'"
note "Expected: HTTP 429 with JSON error body and Retry-After header"
pause

note "Check AKO logs to confirm the DataScript was applied:"
run kubectl logs -n avi-system ako-0 -c ako-gateway-api --tail=5 \| grep -i "DataScript\|AITokenRate"


# ══════════════════════════════════════════════════════════════════════════════
banner "PART A — Per-consumer isolation"
# ══════════════════════════════════════════════════════════════════════════════

note "Set x-ai-consumer header to 'alice' — her counter is independent from the IP counter."
note "Alice starts fresh; her budget is also 500 tokens."

for i in 1 2 3; do
  echo "${BOLD}alice request $i${RST}"
  run curl -s -o /tmp/ai-resp.json -w "HTTP %{http_code}" \
    -X POST "$LLM_URL" \
    -H "Content-Type: application/json" \
    -H "x-ai-consumer: alice" \
    -d "'$PAYLOAD'"
  echo ""
done

note "Alice can still make requests even though the IP-keyed budget is exhausted."
pause

if [[ -z "$ISSUER_VIP" ]]; then
  echo "${YLW}JWT issuer not deployed (SKIP_JWT=1). Skipping Part B.${RST}"
  echo ""
  echo "Run setup.sh without SKIP_JWT=1 to enable Part B."
  exit 0
fi

ISSUER_URL="http://${ISSUER_VIP}:8080"


# ══════════════════════════════════════════════════════════════════════════════
banner "PART B — JWT Authentication"
# ══════════════════════════════════════════════════════════════════════════════

note "AIGatewayAuthPolicy is now active. The VS requires a valid Bearer JWT."
pause

echo "${BOLD}1. Request with no token — expect 401${RST}"
run curl -s -w "\nHTTP %{http_code}\n" \
  -X POST "$LLM_URL" \
  -H "Content-Type: application/json" \
  -d "'$PAYLOAD'"
note "Expected: HTTP 401 from Avi SSO Policy"
pause

echo "${BOLD}2. Fetch a valid token from the in-cluster issuer (subject: alice, tenant: acme)${RST}"
run TOKEN=\$\(curl -sf "${ISSUER_URL}/token?sub=alice&tenant=acme&model=mock-llm-v1" \| python3 -c \"import sys,json; print\(json.load\(sys.stdin\)\[\'token\'\]\)\"\)
TOKEN=$(curl -sf "${ISSUER_URL}/token?sub=alice&tenant=acme&model=mock-llm-v1" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "  Token (first 60 chars): ${TOKEN:0:60}..."
pause

echo "${BOLD}3. Request with valid JWT — expect 200 + forwarded claim headers${RST}"
note "Watch for x-ai-consumer, tenant, model headers in the backend log."
run curl -s -w "\nHTTP %{http_code}\n" \
  -X POST "$LLM_URL" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "'$PAYLOAD'"
note "Expected: HTTP 200. The 'sub' claim (alice) is in x-ai-consumer header; 'tenant' and 'model' are forwarded."
pause


# ══════════════════════════════════════════════════════════════════════════════
banner "PART B + A — JWT identity drives rate limit key"
# ══════════════════════════════════════════════════════════════════════════════

note "With JWT auth active, x-ai-consumer is set by the SSO policy (not manually)."
note "The AITokenRateLimitPolicy keys on x-ai-consumer, so each subject has its own budget."
note "Subject 'alice' has a fresh 500-token budget (different from the IP-keyed counter)."

for i in 1 2 3 4 5; do
  echo "${BOLD}alice (JWT) request $i${RST}"
  run curl -s -o /tmp/ai-resp.json -w "HTTP %{http_code}" \
    -X POST "$LLM_URL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" \
    -d "'$PAYLOAD'"
  echo ""
  sleep 0.3
done

echo "${BOLD}alice (JWT) request 6 — SHOULD BE REJECTED${RST}"
run curl -s -w "\nHTTP %{http_code}\n" \
  -X POST "$LLM_URL" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "'$PAYLOAD'"
note "Expected: HTTP 429 — alice's 500-token hourly budget is exhausted."
pause

echo "${BOLD}bob has a fresh budget — gets a different token${RST}"
BOB_TOKEN=$(curl -sf "${ISSUER_URL}/token?sub=bob&tenant=acme" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
run curl -s -o /tmp/ai-resp.json -w "HTTP %{http_code}" \
  -X POST "$LLM_URL" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $BOB_TOKEN" \
  -d "'$PAYLOAD'"
note "Expected: HTTP 200 — bob's counter is independent from alice's."

echo ""
echo "══════════════════════════════════════════════════════════════"
echo " Demo complete!"
echo ""
echo " Avi UI walkthrough:"
echo "   Applications → Virtual Services → llm-route VS"
echo "   ├── Security    → SSO Policy: inference-llm-auth-sso"
echo "   ├── Policies    → HTTP Policy Set: <vsname>-ai-auth"
echo "   └── DataScripts → <vsname>-ai-tok-req, <vsname>-ai-tok-resp"
echo "══════════════════════════════════════════════════════════════"
