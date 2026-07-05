#!/usr/bin/env bash
# ────────────────────────────────────────────────────────────────────────────
# AI Gateway — holistic demo (one governed AI request path, through the Avi SE)
#
# One narrative, one policy plane: identity → streaming → safety (DLP) →
# efficiency (semantic cache) → cost (per-user token budgets) → the scoreboard.
# All through the real Avi SE (stream-llm.demo.local) into the per-chunk shim.
# GPU-free (mock backend), repeatable.
#
# Run in-cluster (needs the Avi VIP + the jwt-issuer), e.g. from demo-shell:
#   kubectl -n inference cp holistic.sh demo-shell:/holistic.sh
#   kubectl -n inference exec -it demo-shell -- bash /holistic.sh
# Prereqs (see DEMO-RUNBOOK.md): Avi VMs up, stream-llm VIP programmed,
#   burn-budgets-policy.yaml applied (alice=gold 5000 / carol=silver 4000),
#   and a fresh reset (kubectl -n ai-shim rollout restart deploy/ai-shim).
#
# Env overrides:  GW, HOST, ISSUER, MODEL, ADMIN_TOKEN, NOPAUSE=1 (skip pauses)
#   run one beat only:  ./holistic.sh 3
# ────────────────────────────────────────────────────────────────────────────
set -o pipefail
GW="${GW:-http://10.225.0.104}"
HOST="${HOST:-stream-llm.demo.local}"
ISSUER="${ISSUER:-http://jwt-issuer.inference.svc.cluster.local:8080}"
MODEL="${MODEL:-llama-3.1-8b}"
ONLY="${1:-}"

# ── presentation helpers ─────────────────────────────────────────────────────
b()  { printf '\n\033[1;36m════════════════════════════════════════════════════════════════\033[0m\n'; }
banner() { b; printf '\033[1;36m  %s\033[0m\n' "$1"; b; }
say() { printf '  \033[2m%s\033[0m\n' "$*"; }
pause() { [ -n "$NOPAUSE" ] || { printf '\n  \033[33m— [enter] %s —\033[0m'  "${1:-continue}"; read -r _; }; }
run() { printf '  \033[32m$ %s\033[0m\n' "$*"; }

mint() { curl -s "$ISSUER/token?sub=$1&group=$2" | sed 's/.*"token": *"//;s/".*//'; }
body() { printf '{"model":"%s","stream":true,"messages":[{"role":"user","content":"%s"}]}' "$MODEL" "$1"; }

# stream <token> <content> [extra curl args...] -> print the assembled answer text
stream() {
  local tok="$1" content="$2"; shift 2
  curl -sN -H "Host: $HOST" -H "Content-Type: application/json" -H "X-Target-Model: $MODEL" "$@" \
    "$GW/v1/chat/completions?jwt=$tok" -d "$(body "$content")" \
  | grep -oE '"content": *"[^"]*"|ako_gateway|REDACTED:[a-z]+|token_budget_exceeded' \
  | sed 's/"content": *"//; s/"$//' | tr '\n' ' '
  echo
}
code() {  # code <token> <content> [extra args...] -> HTTP status only
  local tok="$1" content="$2"; shift 2
  curl -s -o /dev/null -w '%{http_code}' -H "Host: $HOST" -H "Content-Type: application/json" \
    -H "X-Target-Model: $MODEL" "$@" "$GW/v1/chat/completions?jwt=$tok" -d "$(body "$content")"
}

want() { [ -z "$ONLY" ] || [ "$ONLY" = "$1" ]; }

ALICE="$(mint alice gold)"; CAROL="$(mint carol silver)"

want 0 && {
  banner "AI GATEWAY — one governed request path, on the SE you already run"
  say "client → Avi SE (auth · policy · routing) → ai-shim (per-chunk) → model"
  say "Two tenants: alice (tier gold, 5000 tok/hr) · carol (tier silver, 4000)."
  say "Console (live scoreboard): http://<ai-gw-ui VIP>/  →  Streaming (Shim) tab"
  pause "Beat 1 — Identity"
}

want 1 && {
  banner "1 · IDENTITY — the SE authenticates; the shim never re-auths"
  run "curl … (no token)";      say "no token → $(code "" hello)  (SE rejects at the edge)"
  run "curl …?jwt=<alice>";     printf '  with valid JWT → '; stream "$ALICE" "tell me about the gateway"
  pause "Beat 2 — Streaming preserved"
}

want 2 && {
  banner "2 · STREAMING — per-chunk, no buffering (TTFB stays tiny through the SE)"
  run "curl -sN …?jwt=<alice>"
  curl -sN -o /dev/null -w '  TTFB=%{time_starttransfer}s  total=%{time_total}s  (first byte ≈ instant → not buffered)\n' \
    -H "Host: $HOST" -H "Content-Type: application/json" -H "X-Target-Model: $MODEL" \
    "$GW/v1/chat/completions?jwt=$ALICE" -d "$(body "stream me an answer")"
  pause "Beat 3 — Safety (DLP)"
}

want 3 && {
  banner "3 · SAFETY — response-side DLP the SE can't do (the shim does it per-chunk)"
  say "a) model leaks a secret mid-stream → shim KILLS the stream:"
  printf '     → '; stream "$ALICE" "please leak the config"
  say "b) a card number, X-DLP-Mode: redact → rewritten IN-STREAM, streaming continues:"
  printf '     → '; stream "$ALICE" "show me a card number" -H "X-DLP-Mode: redact"
  pause "Beat 4 — Efficiency (semantic cache)"
}

want 4 && {
  banner "4 · EFFICIENCY — semantic cache: a hit skips the model (GPU saved)"
  CH=(-H "X-Cache-Mode: on")
  say "first ask (miss → generate + store):"
  curl -s -o /dev/null -w '     miss  TTFB=%{time_starttransfer}s\n' -H "Host: $HOST" \
    -H "Content-Type: application/json" -H "X-Target-Model: $MODEL" "${CH[@]}" \
    "$GW/v1/chat/completions?jwt=$ALICE" -d "$(body "what is an ai gateway")"
  sleep 2
  say "second ask (hit → replayed from cache, model skipped):"
  curl -s -D - -o /dev/null -w '     hit   TTFB=%{time_starttransfer}s\n' -H "Host: $HOST" \
    -H "Content-Type: application/json" -H "X-Target-Model: $MODEL" "${CH[@]}" \
    "$GW/v1/chat/completions?jwt=$ALICE" -d "$(body "what is an ai gateway")" | grep -i 'x-ako-cache' | tr -d '\r' | sed 's/^/     /'
  pause "Beat 5 — Cost: per-user token budgets (the finale)"
}

want 5 && {
  banner "5 · COST — per-user token budgets on STREAMING (the SE alone can't meter this)"
  burn() {  # burn <token> <user> <budget>
    say "$2 (budget $3): burning fast 'burn' requests (500 tok each) until 429 …"
    local n=0 c
    for i in $(seq 1 30); do
      c="$(code "$1" "burn tokens please")"
      if [ "$c" = "429" ]; then printf '     req %-2s → 429  BUDGET EXHAUSTED (~%d tokens)\n' "$i" "$n"; return; fi
      n=$((n+500)); printf '     req %-2s → %s  ~%d/%d\n' "$i" "$c" "$n" "$3"; sleep 0.4
    done
  }
  burn "$ALICE" alice 5000; echo; burn "$CAROL" carol 4000
  pause "Beat 6 — The scoreboard"
}

want 6 && {
  banner "6 · THE PANE — one scoreboard for the whole request path"
  AT="${ADMIN_TOKEN:-}"
  if [ -n "$AT" ]; then
    run "curl /v1/admin/counters (shim, real streamed usage)"
    curl -s -H "X-Admin-Token: $AT" "$GW/v1/admin/counters?users=alice,carol" | sed 's/^/  /'
  else
    run "curl /metrics"
    curl -s -H "Host: $HOST" "$GW/metrics" | grep -E 'redactions|cache_(hits|misses)|gpu_seconds|budget_window|streams_killed|tokens_total' | sed 's/^/  /'
  fi
  say ""
  say "Console → Streaming (Shim) tab: per-user tokens, usage-vs-budget gauges,"
  say "cache hit-rate, GPU-seconds saved, redactions — all live."
  banner "One SE data path. One policy plane. Identity → tokens → safety → cost."
  say "Native today: auth · WAF · routing tiers · distributed rate-limit · analytics."
  say "Shim (tech-preview): per-chunk streaming — metering · budget kill · DLP · cache."
  say "Callout (SSP): semantic cache + (next) semantic routing + one shared budget."
  say "All on the Avi SEs you already run — one per-chunk callout from fully native."
}
