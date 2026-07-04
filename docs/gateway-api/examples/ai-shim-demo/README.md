# AKO AI Gateway — Per-Chunk Streaming Shim (Demo Kit)

An OpenResty shim that sits **behind the Avi SE as a pool member** and provides
the one primitive the SE data path lacks today: per-chunk inspection /
transformation of streamed LLM (SSE) traffic without buffering it.

**Architecture rule (load-bearing):** the SE stays the *only* policy point. The
shim executes policy passed to it as request headers (`X-DLP-Mode`,
`X-Cache-Mode`, `X-Token-Budget`, `X-Auth-Sub`, `X-Target-Model`) — it never
decides identity, routing, or entitlement itself. No cosockets in the
`body_filter` phase; async work runs in timers.

## Capabilities

| Capability | How | Maturity |
|---|---|---|
| Streaming preserved | `proxy_buffering off`, per-chunk `body_filter` | Working, demo-grade |
| Per-consumer token metering | delta≈token + usage-frame reconcile | Working, demo-grade |
| Mid-stream token budget kill | `X-Token-Budget`; kill at token N | Working, demo-grade |
| Output DLP — **kill** | rule scan of accumulated output → terminate | Working, demo-grade |
| Output DLP — **redact** *(new)* | rewrite spans to `[REDACTED:<type>]` in-stream | **Tech-preview, rule-based** |
| Request-phase injection block | 403 in access phase, synchronous | Working, demo-grade |
| Async semantic guardrail | timer scanner, one-chunk-lag verdict | Working, one-chunk-lag by design |
| **Semantic cache** *(new)* | exact sha256 + MiniLM/RediSearch KNN serve+capture | **Tech-preview** |
| Metrics | Prometheus text at `/metrics` | Working |

### Verified — `test.sh`, 16/16 GREEN

Run in-cluster (tester pod → `svc/ai-shim:8080`) on AKS `aks-inference-demo`,
mock backend, no GPUs. In-cluster because a laptop `kubectl port-forward`
tunnels through the API server and inflates TTFB (~290 ms) — pod→service is the
honest vantage for the buffering guard.

| Assertion | Result |
|---|---|
| Streaming TTFB < 100 ms (buffering guard) | **PASS — 7.2 ms** |
| Budget kill at exactly N=15 tokens | **PASS** (`tokens_delivered:15`) |
| Redacted stream contains no card digits | **PASS** (`[REDACTED:pan]`, no `4242…`) |
| Cache second-hit TTFB < 200 ms + `X-AKO-Cache: hit` | **PASS — 5.2 ms** |
| Metrics counters line up | **PASS** (redactions 1, cache hit 1 / miss 1, killed 2, GPU-sec saved 3.22) |

> **Through-SE topology** (`stream-llm.demo.local` via the Avi VIP): the
> Gateway/HTTPRoute is valid and AKO-Accepted, but VIP programming requires the
> Avi Controller to be reachable. Verify end-to-end when the Avi VMs are up:
> `GW=http://<vip> HOST=stream-llm.demo.local ./test.sh`.

## Feature 1 — DLP redaction mode

`X-DLP-Mode` selects the output-DLP behavior (default `kill`):

- **`kill`** — original behavior: any secret in the output terminates the stream.
- **`redact`** — matched spans are rewritten to `[REDACTED:<type>]` inside the
  SSE frame JSON and re-serialized (valid JSON), so the client keeps a usable
  answer minus the secret.
- **`off`** — no output DLP.

[`lua/dlp.lua`](lua/dlp.lua) carries the pattern table — credit card (Luhn),
AWS access key, `sk-`/`SECRET-API-KEY` api keys, bearer tokens, SSN (with
validity-range exclusions, matching the built-in `ssn-strict` WAF rule), and
email. Detection runs over an **accumulating window**, not a single delta, so a
secret split across SSE frame boundaries is still caught: the shim withholds
only the trailing incomplete-match run (≈ one frame of latency) and redacts the
rest. Each redaction increments `ai_shim_redactions_total`.

Cross-boundary catch is guaranteed for **contiguous** secrets up to
`MAX_TOKEN` (48) chars. Secrets interrupted by whitespace (a spaced-out card, a
`Bearer <token>` pair) are caught only when both halves land in the same scan
window — see the caveat in `dlp.lua`. Rule-based: no semantic PII.

## Feature 2 — semantic cache (capture + serve)

A sidecar ([`cache/cache_service.py`](cache/cache_service.py), FastAPI :9200)
backed by **redis-stack** (RediSearch HNSW, cosine) and
**all-MiniLM-L6-v2** embeddings.

- **Serve** (access phase, `X-Cache-Mode: on`): the shim `ngx.location.capture`s
  `/lookup`. On a hit it replays the cached text as OpenAI-style SSE delta
  frames from Lua (~4 words/frame, 20 ms apart), emits a usage frame + `[DONE]`,
  adds `X-AKO-Cache: hit`, meters the hit, and **skips the model entirely**.
- **Capture** (on EOF, in a zero-delay timer → raw cosocket `/store`): only if
  the stream was **NOT killed and NOT redacted**. *DLP-before-cache is an
  invariant — a response that tripped DLP is never cached.*
- **Two-stage match, strictly tenant-scoped:** exact `sha256(prompt)` first,
  then semantic KNN — both filtered to a `model + tenant + system_hash`
  namespace. A lookup can never return another tenant's response.
- **GPU-seconds saved:** the shim keeps a running average generation time per
  model; each cache hit credits that average to `ai_shim_gpu_seconds_saved`.

**Caveats (honest):** `CACHE_THRESHOLD` default `0.95` needs tuning per corpus;
semantic hits are unsafe at `temperature > 0` (non-deterministic generations —
prefer exact-match or cache only `temperature=0`); embeddings add latency on a
miss (the hit path uses exact-match and stays fast). Ephemeral Redis (no PVC).

New metrics: `ai_shim_redactions_total`, `ai_shim_cache_hits_total`,
`ai_shim_cache_misses_total`, `ai_shim_gpu_seconds_saved`,
`ai_shim_avg_generation_seconds{model}`.

## Layout

```
nginx.conf              shim config (rendered: RESOLVER, MODEL_BACKEND, CACHE_BACKEND, CLASSIFIER_BACKEND)
lua/stream_filter.lua   THE CORE: per-chunk reassembly, meter, kill, redact, cache-capture
lua/dlp.lua             rule-based secret/PII detection + redaction (sliding window)
lua/request_guard.lua   request-phase guardrail + semantic-cache serve path
lua/cache_client.lua    cache STORE over a cosocket (timer only)
lua/scan_timer.lua      async semantic scanner (one-chunk-lag verdicts)
lua/header_filter.lua   drops Content-Length (we may rewrite the body)
lua/metering_flush.lua  log-phase flush to shared dict
lua/metrics.lua         /metrics exposition
mock/mock_vllm.py       GPU-free SSE backend (triggers: leak, scam, card, long)
cache/                  cache_service.py + Dockerfile + requirements.txt
k8s/shim.yaml           ai-shim ns: shim + mock + redis-stack + cache-service + stream-llm Gateway/HTTPRoute
docker-compose.yml      laptop run (openresty + mock + redis-stack + cache-service)
deploy.sh               ACR build (cache-service) + render + configmaps + apply
demo.sh                 nine-phase narrated demo
test.sh                 nine-phase headless assertions
```

## Deploy (AKS + ACR — the real topology)

Builds the cache image server-side in ACR (no local Docker), deploys the whole
stack into an isolated `ai-shim` namespace behind a **dedicated Avi LLM
gateway** (`stream-llm.demo.local`, its own VIP) so it never touches the live
8-test demo or the auth path.

```bash
./deploy.sh                    # az acr build cache-service:latest + apply
# swap the mock for real vLLM by editing MODEL_BACKEND in deploy.sh when GPUs are up
```

Topology:

```
client → Avi VS (stream-llm.demo.local, own VIP) → HTTPRoute stream-llm → ai-shim:8080 → mock/vLLM
                                                                              ↕
                                                              cache-service :9200 → redis-stack
```

The shim reads SE-derived context from request headers (in prod, set by the SE
from JWT claims / model-routing DataScript / per-tenant policy). It never
re-authenticates — the SE stays the single policy point. Scrape `/metrics` into
the Avi analytics join and feed token totals back into the SE rate limiter:
sensor at the shim, enforcement at the SE.

## Laptop run (no k8s, no GPUs)

```bash
docker compose up --build      # openresty + mock + redis-stack + cache-service
GW=http://localhost:8080 ./demo.sh      # or ./test.sh
```

## Demo / test (mock backend prompt triggers)

```bash
kubectl -n ai-shim port-forward svc/ai-shim 8080:8080 &
GW=http://localhost:8080 ./demo.sh      # narrated, 9 phases
GW=http://localhost:8080 ./test.sh      # headless asserts, exit 0 = green
# through the SE:  GW=http://<vip> HOST=stream-llm.demo.local ./demo.sh
```

| Prompt contains | Mock behavior | Shows |
|---|---|---|
| anything | 40-token stream | pass-through, TTFB intact |
| `long` | 200 tokens | budget kill mid-stream (`X-Token-Budget`) |
| `leak` | `SECRET-API-KEY-123` at token ~12 | inline output DLP kill |
| `card` | valid-Luhn PAN split across two frames | DLP redact (`X-DLP-Mode: redact`) / cross-boundary catch |
| `scam` | "wire the funds" | async scanner kill |
| `Ignore all previous instructions` | n/a | request-phase 403 |

## Honest labels (for slides)

- **Working today, demo-grade:** streaming metering, budget kill, DLP kill,
  request-phase blocking.
- **Tech-preview, rule-based:** DLP **redaction** — regex/Luhn, no semantic PII,
  ≈one-frame hold window, contiguous-secret guarantee only.
- **Tech-preview:** semantic **cache** — threshold tuning required,
  `temperature>0` caveat, DLP-before-cache invariant, ephemeral state.
- **One-chunk-lag by construction:** async semantic guardrails (body_filter
  forbids cosockets → verdict lands on the next chunk).
- **Not production:** delta≈token heuristic, regex delta extraction, no mTLS
  shim↔sidecars, no HA/shared state (single replica keeps metering coherent).

