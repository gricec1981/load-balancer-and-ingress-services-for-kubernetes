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
echo " LLM demo complete. Run Part C below for the A2A gateway demo."
echo ""
echo " Avi UI walkthrough:"
echo "   Applications → Virtual Services → llm-route VS"
echo "   ├── Security    → SSO Policy: inference-llm-auth-sso"
echo "   ├── Policies    → HTTP Policy Set: <vsname>-ai-auth"
echo "   └── DataScripts → <vsname>-ai-tok-req, <vsname>-ai-tok-resp"
echo "══════════════════════════════════════════════════════════════"

if [[ -z "$ISSUER_VIP" ]]; then
  echo "${YLW}JWT issuer not deployed. Skipping Part C (A2A).${RST}"
  exit 0
fi


# ══════════════════════════════════════════════════════════════════════════════
banner "PART C — A2A Gateway: agent-to-agent task routing"
# ══════════════════════════════════════════════════════════════════════════════

note "Separate Avi gateway (a2a-gateway) with two agent backends:"
note "  ops-agent.demo.local      → ops-agent-svc      (3 replicas)"
note "  security-agent.demo.local → security-agent-svc (3 replicas)"
note ""
note "What we will show:"
note "  1. Agent card discovery (GET /.well-known/agent.json)"
note "  2. orchestrator sends a task → task ID returned from a random pod"
note "  3. Three follow-up tasks/get → SAME pod every time (task affinity)"
note "  4. security-agent tries tasks/send → 403 (RBAC: only tasks/get allowed)"
note "  5. Unknown agent tries anything   → 403 (not in allow-list)"
pause

# ── Detect A2A gateway VIP ────────────────────────────────────────────────────
A2A_VIP=$(kubectl get gateway a2a-gateway -n "$NAMESPACE" \
  -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || echo "")
if [[ -z "$A2A_VIP" ]]; then
  echo "${RED}❌ a2a-gateway has no VIP yet. Has setup-a2a.sh been run?${RST}"
  exit 1
fi
note "A2A gateway VIP: $A2A_VIP"
note "Using Host headers to select the route (ops-agent.demo.local / security-agent.demo.local)"
note "-k skips TLS verification (self-signed cert in the demo)"

OPS_URL="https://${A2A_VIP}"
SEC_URL="https://${A2A_VIP}"

# ── Mint agent tokens ─────────────────────────────────────────────────────────
note "Minting JWTs with agent_id claim for orchestrator, security-agent, and an unknown agent..."
ORCH_TOKEN=$(curl -sf "${ISSUER_URL}/token?sub=orchestrator&agent_id=orchestrator" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
SEC_TOKEN=$(curl -sf "${ISSUER_URL}/token?sub=security-agent&agent_id=security-agent" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
UNKNOWN_TOKEN=$(curl -sf "${ISSUER_URL}/token?sub=rogue&agent_id=rogue-agent" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
note "Tokens minted (orchestrator / security-agent / rogue-agent)"
pause


# ── 1. Agent card discovery ───────────────────────────────────────────────────
banner "C.1 — Agent card discovery"
note "GET /.well-known/agent.json proxied through the gateway."
note "The gateway passes this straight through (no body inspection, no RBAC)."
run curl -sk -H "Host: ops-agent.demo.local" "${OPS_URL}/.well-known/agent.json" \
  | python3 -m json.tool
note "The 'url' field above should show ops-agent.demo.local (gateway URL, not pod address)."
pause


# ── 2. tasks/send (orchestrator) ─────────────────────────────────────────────
banner "C.2 — orchestrator sends a task"
SEND_PAYLOAD='{"jsonrpc":"2.0","id":1,"method":"tasks/send","params":{"message":{"role":"user","parts":[{"text":"Run diagnostics on cluster"}]}}}'
note "orchestrator JWT has agent_id=orchestrator → allow: tasks/send ✓"
RESP=$(curl -sk -D /tmp/a2a-hdrs.txt \
  -H "Host: ops-agent.demo.local" \
  -H "Content-Type: application/json" \
  -d "$SEND_PAYLOAD" \
  "${OPS_URL}/?jwt=${ORCH_TOKEN}")
echo "$RESP" | python3 -m json.tool
TASK_ID=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['id'])" 2>/dev/null || echo "")
POD_CREATOR=$(grep -i "^x-handled-by:" /tmp/a2a-hdrs.txt | awk '{print $2}' | tr -d '\r')
echo ""
note "Task ID  : ${BOLD}${TASK_ID}${RST}"
note "Created by pod: ${BOLD}${POD_CREATOR}${RST}"
note "The gateway wrote  task-id → $POD_CREATOR  into the Avi VS table (TTL 30 min)."
pause


# ── 3. tasks/get × 3 — task affinity ─────────────────────────────────────────
banner "C.3 — Follow-up tasks/get × 3 (task affinity)"
note "All three calls carry the same task ID in params.id."
note "HTTP_REQ_DATA looks up that ID in the VS table → avi.pool.select(pool, pod-ip)."
note "Expected: SAME pod all three times regardless of round-robin load balancing."
echo ""
GET_PAYLOAD="{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tasks/get\",\"params\":{\"id\":\"${TASK_ID}\"}}"
for i in 1 2 3; do
  RESP=$(curl -sk -D /tmp/a2a-hdrs.txt \
    -H "Host: ops-agent.demo.local" \
    -H "Content-Type: application/json" \
    -d "$GET_PAYLOAD" \
    "${OPS_URL}/?jwt=${ORCH_TOKEN}")
  POD=$(grep -i "^x-handled-by:" /tmp/a2a-hdrs.txt | awk '{print $2}' | tr -d '\r')
  STATE=$(echo "$RESP" | python3 -c \
    "import sys,json; print(json.load(sys.stdin).get('result',{}).get('status',{}).get('state','?'))" \
    2>/dev/null || echo "error")
  if [[ "$POD" == "$POD_CREATOR" ]]; then
    echo "  get #$i → pod: ${GRN}${BOLD}${POD}${RST}  state: $STATE  ✓ same pod"
  else
    echo "  get #$i → pod: ${RED}${BOLD}${POD}${RST}  state: $STATE  ✗ DIFFERENT pod (affinity miss)"
  fi
done
echo ""
note "Green = affinity working. Each follow-up landed on the pod that created the task."
pause


# ── 4. RBAC: security-agent tries tasks/send → denied ────────────────────────
banner "C.4 — Agent RBAC: security-agent tries tasks/send"
note "security-agent JWT has agent_id=security-agent."
note "AIA2ARoutePolicy allows security-agent only tasks/get — tasks/send is not listed."
note "HTTP_REQ_DATA bakes the RBAC table into Lua at policy-apply time; no external lookup."
run curl -sk -w "\nHTTP %{http_code}" \
  -H "Host: ops-agent.demo.local" \
  -H "Content-Type: application/json" \
  -d "$SEND_PAYLOAD" \
  "${OPS_URL}/?jwt=${SEC_TOKEN}"
note "Expected: HTTP 403  body: {\"error\":{\"code\":-32001,\"message\":\"method_not_authorized\"}}"
pause

note "But security-agent CAN read an existing task (tasks/get is in its allow-list):"
run curl -sk -w "\nHTTP %{http_code}" \
  -H "Host: ops-agent.demo.local" \
  -H "Content-Type: application/json" \
  -d "$GET_PAYLOAD" \
  "${OPS_URL}/?jwt=${SEC_TOKEN}"
note "Expected: HTTP 200 with the task result"
pause


# ── 5. Unknown agent → denied ─────────────────────────────────────────────────
banner "C.5 — Unknown agent (not in RBAC rules)"
note "rogue-agent is not listed in agentAccess.rules — default deny."
run curl -sk -w "\nHTTP %{http_code}" \
  -H "Host: ops-agent.demo.local" \
  -H "Content-Type: application/json" \
  -d "$SEND_PAYLOAD" \
  "${OPS_URL}/?jwt=${UNKNOWN_TOKEN}"
note "Expected: HTTP 403  body: method_not_authorized"
pause


echo ""
echo "══════════════════════════════════════════════════════════════"
echo " Demo complete!"
echo ""
echo " Avi UI walkthrough for A2A:"
echo "   Applications → Virtual Services → ops-agent-route VS"
echo "   ├── Security    → SSO Policy (shared with LLM gateway)"
echo "   └── DataScripts → *-ai-a2a-req  *-ai-a2a-reqdata"
echo "                     *-ai-a2a-resp *-ai-a2a-respdata"
echo "══════════════════════════════════════════════════════════════"
