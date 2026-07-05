# Holistic Demo — Runbook

One narrative for the whole AI-gateway token-governance story, driven by `holistic.sh`
through the real Avi SE. Two tenants (**alice**/gold, **carol**/silver), GPU-free,
repeatable. The console is the live scoreboard.

**The arc (6 beats):** Identity → Streaming preserved → Safety (DLP kill + redact) →
Efficiency (semantic cache) → Cost (per-user token budgets, the finale) → The scoreboard + maturity map.

---

## 0. Prereqs (once)

| Thing | Value |
|---|---|
| kube context | `aks-inference-demo` |
| shim stack ns | `ai-shim` (shim, mock-vllm, redis-stack, cache-service) |
| stream-llm VIP | `10.225.0.104` · host `stream-llm.demo.local` |
| console (UI) | VIP `10.225.0.101` · host `ai-gw-ui.demo.local` → **Streaming (Shim)** tab |
| gateway image | `ako-gateway-api:streambudget-20260704c` on `ako-0` (budget + identity forwarding) |
| issuer | `jwt-issuer.inference…:8080/token?sub=&group=` |
| admin token | secret `ai-admin-token` (ns `inference` and `ai-shim`) |

**Bring Avi up** (VMs are deallocated between demos):
```bash
az vm start -g RG-AKO-INFERENCE -n avi-controller --no-wait
az vm start -g MC_RG-AKO-INFERENCE_AKS-INFERENCE-DEMO_EASTUS -n Avi-se-rmrzb --no-wait
# wait ~8-12 min, then confirm the VIP is programmed:
kubectl -n ai-shim get gateway stream-llm-gateway   # ADDRESS=10.225.0.104, PROGRAMMED=True
```

## 1. Reset (before each run — makes the numbers crisp)
```bash
# per-user budgets active (alice=gold 5000 / carol=silver 4000):
kubectl apply -f k8s/burn-budgets-policy.yaml

# fresh shim counters (clears streamed-token + budget counters):
kubectl -n ai-shim rollout restart deploy/ai-shim && kubectl -n ai-shim rollout status deploy/ai-shim

# OPTIONAL cold cache so Beat 4 shows a real miss→hit:
kubectl -n ai-shim exec deploy/redis-stack -- redis-cli FLUSHALL
```

## 2. Run
```bash
ADMIN=$(kubectl -n inference get secret ai-admin-token -o jsonpath='{.data.token}' | base64 -d)
kubectl -n inference cp holistic.sh demo-shell:/holistic.sh
kubectl -n inference exec -it demo-shell -- env ADMIN_TOKEN="$ADMIN" bash /holistic.sh
```
- `demo-shell` must have `bash` + `curl` (if not: `kubectl -n ai-shim run tester --image=alpine:3.19 --restart=Never --command -- sleep 100000`, then `apk add bash curl`, `cp`, `exec`).
- Press **enter** to advance each beat. Set `NOPAUSE=1` for an unattended dry-run. Run a single beat: `bash /holistic.sh 3`.
- Have the **console open** (`ai-gw-ui.demo.local` → Streaming (Shim)) so the numbers move live as you go.

---

## 3. Talk track (per beat)

- **1 · Identity** — "The SE is the single policy point: it authenticates the JWT at the edge; no token → 401. The shim behind it never re-authenticates — it trusts the identity the SE forwards."
- **2 · Streaming** — "First byte is essentially instant through the SE — the stream is delivered per-chunk, never buffered. That's the primitive everything else rides on."
- **3 · Safety** — "The model leaks a secret mid-generation → the shim kills the stream the moment it appears. A card number → rewritten to `[REDACTED:pan]` in-stream while the answer keeps flowing. This is response-side inspection the SE data path can't do today without destroying the stream."
- **4 · Efficiency** — "Same prompt again → served from the semantic cache, the model never runs. Watch GPU-seconds-saved tick up. Cost control that's invisible to the client."
- **5 · Cost (finale)** — "alice and carol each have their own token budget. They burn them and get 429'd at 5000 and 4000 — per-user budgets enforced on **streaming** traffic, which the SE alone can't meter. Same policy governs both users."
- **6 · The pane** — "One scoreboard for the whole path: per-user tokens, usage-vs-budget, cache hit-rate, GPU-seconds saved, redactions. All on the Avi SEs you already run — no new proxy fleet."

## 4. The close (maturity map — say it plainly)
- **Native on the SE today (GA/alpha):** auth, WAF guardrails, model-routing tiers, distributed rate-limit, analytics.
- **Shim (tech-preview):** per-chunk streaming — token metering, mid-stream budget kill, DLP kill + redact, semantic-cache serve/capture.
- **External callout (SSP):** semantic cache, and next: a semantic-routing classifier + one shared token counter across streaming & non-streaming.
- **The line:** "It's all one SE-native per-chunk callout away from fully native (RFE-4761). The shim proves it's a small lift."

## 5. Honest asterisks (if asked)
- **Two counters, not one shared budget yet.** Streaming (shim) and non-streaming (SE) each enforce the same ceiling against their own counter — a single shared budget across both is the SSP/shared-store step.
- **Semantic routing isn't live** — routing today is rule-based tiers (`llm-tiers`); the embedding infra (cache-service/MiniLM) is the piece to build it on.
- **DLP is rule-based** (regex/Luhn), tech-preview — not semantic PII.
- **One request behind** — budgets charge at response, gate at admission (same as the SE native limiter).

## 6. Troubleshooting
- **VIP `000` / not programmed** — Avi still booting or deallocated: `az vm start` both, wait, recheck `kubectl -n ai-shim get gateway`.
- **Every request `000` on `.104` but other VIPs answer** — the `stream-llm` VS wedged: `kubectl -n ai-shim delete httproute stream-llm && kubectl apply -f k8s/shim.yaml` (rebuilds the child VS).
- **Budgets don't trip** — `burn-budgets-policy.yaml` not applied, or shim not restarted (stale counters). Re-run §1.
- **Beat 4 shows hit on the first ask** — warm cache from a prior run: `redis-cli FLUSHALL` (see §1).
- **Wrong tenant in the counters** — the SE must forward `x-ai-consumer` (needs the `streambudget-20260704c` gateway image + an auth policy on the route).
