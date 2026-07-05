# Streaming token-budget enforcement + dashboard integration — design

**Status:** CRD + AKO wiring **implemented and verified end-to-end through the
Avi SE** (`perRequest` mode), and **dashboard integration implemented** (option
B — the shim serves `/v1/admin/counters`, the console reads it). Does **not**
modify the auth path.

**Dashboard (B) — implemented + verified 2026-07-04:** the SE can't meter a
streamed response, so the shim is the source of truth. It serves
`/v1/admin/counters` (same JSON contract, gated by `X-Admin-Token`) with REAL
per-consumer streamed tokens, keyed by the identity the SE now forwards
(`add_header x-ai-consumer`, from the token DataScript's resolved identity). The
`ai-gateway-ui` backend reads it (`SHIM_COUNTERS_URL` + `SHIM_ADMIN_TOKEN`) and a
"Streaming (Shim)" console tab renders per-user streamed tokens + tiles
(redactions, cache hit-rate, GPU-seconds saved). Verified: alice's real streamed
tokens + non-zero redaction/cache/GPU tiles surface in the console. No Redis
counter needed — the shim's metering dict is the store (single-replica keeps it
coherent; multi-replica would need the shared-store variant below).

**Verified 2026-07-04** on AKS `aks-inference-demo`: an `AITokenRateLimitPolicy`
with `streaming: {enforce, mode: perRequest, perRequestCap: 15}` on the
`stream-llm` route drives the SE HTTP_REQ DataScript to inject `X-Token-Budget:
15`; the shim truncates the stream at exactly 15 tokens with **no client-set
header** (`token_budget_exceeded`, `tokens_delivered:15`), while an
unauthenticated request is rejected 401 by the SE. Implemented in
`ako-gateway-api/aigateway/{types,controller,datascript}.go` + the CRD schema;
`buildStreamingHeaderBlock` emits the header (remove-then-add so policy overrides
any client value). `remaining` mode is implemented but reads the full budget
until the shim feeds streamed counts back into the counter (§3) — streaming
responses aren't buffered/metered by the SE, so that counter stays 0.

**Cumulative budget for streaming (option 1) — shim-side, verified shim-direct
2026-07-04:** the SE's native cumulative gate is inert on a streaming route (it
never meters the stream). So `buildStreamingHeaderBlock` also forwards the
resolved cumulative budget + window (`X-Budget-Total` / `X-Budget-Window`) to the
shim; the shim (`request_guard.lua`) keeps a windowed per-consumer counter
(`budget:<consumer>:<window>`, incremented in `metering_flush.lua`, TTL'd) and
rejects **429** at admission once at/over budget — all-or-nothing, like the SE
native limiter. Verified shim-direct: budget 30, 40-token responses → req1 200,
req2/3 429 (`{"used":40,"budget":30,...,"enforced_by":"ako-ai-shim"}`). This keeps
counters at the shim (no SE→store call, no Redis on the data path); the SE stays
the policy source (it forwards the budget), the shim is the meter+enforcer for
streaming. The AKO `add_header` forwarding is built + `go build`-clean but its
through-SE path is **pending next Avi VM-up** (not deployed to avoid an
unverified DataScript re-wedging the VS). Chosen over the Redis shared-store
variant (which would put Redis in the SE request-admission path).

**Gotcha (found + worked around):** a token policy requires the route to have an
`AIGatewayAuthPolicy`. Without one, AKO defaults `claimMode` to OAuth and the
token DataScript calls `avi.http.oauth_get_claim`, which wedges a non-OAuth VS
(empty replies on every request). Attaching a jwtQuery `AIGatewayAuthPolicy` to
`stream-llm` (auth on the SE, never on the shim) flips `claimMode` to JWTQuery
and resolves identity correctly. See `k8s/stream-llm-auth.yaml`.

Companion to the per-chunk streaming shim
([`examples/ai-shim-demo/`](examples/ai-shim-demo/)). Where that kit proves the
shim can meter and truncate a streamed response, this doc sketches how the
existing `AITokenRateLimitPolicy` drives that truncation from policy — and how
the result surfaces in the dashboard.

## Problem

`AITokenRateLimitPolicy` today enforces **cumulative** per-consumer / per-group
token budgets via the Avi SE native rate limiter (the deferred-carry design).
Two structural limits fall out of that:

- **One request behind** — actual token counts are only known *after* a
  response, so the budget gate charges the *previous* request's usage at the
  *next* request's admission.
- **Non-streaming** — it cannot stop a single in-flight response that is
  itself blowing the budget; it can only reject the next request.

The shim already meters tokens **per chunk** and can truncate a stream at a
given ceiling (`X-Token-Budget` → clean `[DONE]` at token N). The gap is that
the ceiling is a raw client-set header — not derived from policy, and the shim's
live counts don't feed the cumulative accounting or the dashboard.

## Design overview

```
   AITokenRateLimitPolicy (CRD)
        │  AKO translate
        ├──► SE native limiter          → admission gate (429)         [cumulative]
        └──► SE DataScript: X-Token-Budget = remaining(consumer) ──┐
                                                                   ▼
                                                            shim (SE pool member)
                                                        • kill mid-stream at N     [real-time]
                                                        • meter REAL streamed tokens
                                                                   │
                              consumed counter ◄── shim /metrics ───┘
                                     │
                                     ▼
                            /v1/admin/counters ──► ai-gateway-ui
                            (usage vs budget · kills · cache · GPU saved)
```

The CRD stays the single policy source; the SE keeps the admission gate; the
shim makes the *same* budget enforce in real time and returns accurate counts.
The shim's per-chunk metering is precisely what removes the one-behind
limitation.

## 1. CRD — a `streaming` block on `AITokenRateLimitPolicy`

Extend each limit with an optional real-time enforcement stanza. Existing fields
are unchanged; absence of `streaming` = today's behavior.

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AITokenRateLimitPolicy
metadata: { name: stream-llm-limits, namespace: ai-shim }
spec:
  targetRef: { kind: HTTPRoute, name: stream-llm }
  limits:
  - name: hourly-group-budget
    key: consumer
    groupHeader: group
    groupBudgets: { group1: 500, group2: 1000 }
    tokens: total
    window: 1h
    action: { type: Reject, statusCode: 429 }   # admission gate (existing)

    streaming:                    # NEW — real-time enforcement on the response
      enforce: true
      mode: remaining             # remaining = window budget left | perRequest = flat cap
      perRequestCap: 512          # used when mode: perRequest
      onExceed: truncate          # clean [DONE] mid-stream | error
      header: X-Token-Budget      # header the shim reads (default)
```

Two modes, answering different questions:

- **`perRequest`** — a flat ceiling on any single response. Static.
- **`remaining`** — pass the consumer's *remaining* window budget as the
  per-request ceiling, so a stream is truncated the moment it would exceed the
  cumulative budget. This is the mode that closes the one-behind / non-streaming
  gap.

## 2. AKO — three touch points

The same path the native-budget work uses:

- **CRD schema:** add `streaming` to
  `helm/ako/crds/ai.ako.vmware.com_aitokenratelimitpolicies.yaml`.
- **Struct + converter:** add a `Streaming` field to the `TokenLimit` type and
  wire it in the hand-rolled `unstructuredToTokenRateLimitPolicy` converter
  (schema alone is not enough — the converter is the second required edit).
- **Translator emits the ceiling two ways:**
  - `mode: perRequest` → a **RequestHeaderModifier** on the route
    (`X-Token-Budget: <cap>`). No DataScript.
  - `mode: remaining` → a **DataScript** at `HTTP_REQ` that computes
    `remaining = groupBudget − consumed(consumer)` from the existing
    deferred-carry table and sets `X-Token-Budget` per request before the
    request reaches the shim pool member. A small addition to the carry logic,
    not a new mechanism.

The shim itself needs no change: it already reads `X-Token-Budget` and truncates
at N with a clean `[DONE]`.

## 3. Close the loop — shim feeds the counter

The SE learns token counts post-response (hence one-behind); the shim knows them
live, so it becomes the source of truth for consumed tokens.

- The shim already exposes `ai_shim_tokens_total{key="consumer:model"}`,
  `ai_shim_streams_killed_total`, redaction and cache counters at `/metrics`.
- Feed the streamed token totals into the same **consumed counter** that backs
  `/v1/admin/counters`, so the next request's `remaining` reflects real usage.

## 4. Dashboard (ai-gateway-ui)

The console already reads real SE counters via `/v1/admin/counters` (the
admin-token endpoint AKO generates from the
`ai.ako.vmware.com/admin-token-secret` annotation). Extend it with a shim
source:

- **Backend:** add a `/metrics` scrape of the shim (same shape as the other
  registry/list sources) and merge into the counters payload.
- **View:** per-consumer **usage vs budget** using the live streamed count, plus
  tiles the shim uniquely provides — **budget-kill events, redactions, cache
  hit-rate, GPU-seconds saved**.

## Reuse

`mode: remaining` bolts directly onto the deferred-carry table already built for
native token budgets — instead of only gating at admission, emit `remaining` as
the per-request header so the shim enforces it live. No new state machine.

## Non-goals / open questions

- **Not** an auth-path change; the shim never re-authenticates.
- Assumes the shim is in the response path as an SE pool member. It does **not**
  compose with inference-aware InferencePool selection in the same path — that
  requires per-endpoint co-location or SE-native processing (out of scope here).
- Cross-pod shim state: a single shim replica keeps `/metrics` coherent;
  multi-replica needs the counter fed to a shared store (the SE counter) rather
  than read per-replica.
- Exact vs approximate token counts: the shim uses a delta≈token heuristic
  reconciled by the provider usage frame; good enough for budget truncation,
  same caveat as the shim demo.
