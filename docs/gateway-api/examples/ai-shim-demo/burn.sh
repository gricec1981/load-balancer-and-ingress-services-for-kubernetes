#!/usr/bin/env bash
# Burn demo for per-user streaming token budgets (burn-budgets-policy.yaml):
# alice (group gold, 5000) and carol (group silver, 4000) each hammer the
# stream-llm route with fast "burn" requests (500 tokens each) until the shim's
# cumulative budget gate returns 429 — showing each user's budget enforced
# independently, for STREAMING traffic the SE itself can't meter.
#
# Run in-cluster (demo-shell / a curl pod), through the Avi SE:
#   GW=http://<vip> HOST=stream-llm.demo.local ./burn.sh
set -o pipefail
GW="${GW:-http://10.225.0.104}"
HOST="${HOST:-stream-llm.demo.local}"
ISSUER="${ISSUER:-http://jwt-issuer.inference.svc.cluster.local:8080}"
MODEL="llama-3.1-8b"
BODY='{"model":"llama-3.1-8b","stream":true,"messages":[{"role":"user","content":"burn tokens please"}]}'

mint() { curl -s "$ISSUER/token?sub=$1&group=$2" | sed 's/.*"token": *"//;s/".*//'; }

burn() {  # burn <user> <group> <budget>
  local user="$1" group="$2" budget="$3"
  local tok; tok="$(mint "$user" "$group")"
  echo "== $user (group=$group, budget=$budget tokens/hr) =="
  local burned=0 code
  for i in $(seq 1 40); do
    code="$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $HOST" \
      -H "Content-Type: application/json" -H "X-Target-Model: $MODEL" \
      "$GW/v1/chat/completions?jwt=$tok" -d "$BODY")"
    if [ "$code" = "429" ]; then
      echo "  req $i -> 429  BUDGET EXHAUSTED (burned ~$burned tokens)"; return
    fi
    burned=$((burned + 500))
    printf '  req %-2s -> %s  ~%d/%d tokens\n' "$i" "$code" "$burned" "$budget"
    sleep 0.4
  done
  echo "  (did not hit 429 in 40 requests)"
}

burn alice gold 5000
echo
burn carol silver 4000
