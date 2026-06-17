# Design: token budgets on the native Avi rate limiter

**Status:** proposal — for sign-off before implementation.
**Goal:** enforce AITokenRateLimitPolicy **token budgets** with the native Avi
rate limiter (`avi.vs.ratelimit.exceed`) instead of the per-SE DataScript counter,
so budgets are **consistent across SEs / VS scale-out**. Request-rate is out of
scope here (already native; not the priority).

## 1. Why

Today token budgets are counted in SE shared-state tables
([`datascript.go`](native-token-budget-design.md) — `buildReqLimitBlock` /
`buildRespLimitBlock`, `avi.vs.table_lookup/remove/insert`). Each SE counts
independently → a budget of 2347/hr can effectively become `2347 × #SEs`. The
native limiter keeps each bucket consistent across VS scale-out (verified on the
31.2.2 controller: `rate_limiters` + `ratelimit.exceed` accepted and live). This
is the "native distributed token counter" gap from the SE-native brief.

## 2. Mechanics

The native limiter is a token bucket: `count` tokens per `period` seconds, `burst_sz`
capacity, and `ratelimit.exceed(name, request_key, consume)` checks-and-consumes.

Map a token budget onto it:

| Budget concept | Native limiter |
|---|---|
| budget `B` (e.g. 2347) | `count = B`, `burst_sz = B` |
| window `W` (e.g. 1h) | `period = W` seconds |
| per-consumer | `request_key = consumer identity` |
| tokens used by a response | `consume = total_tokens` (or prompt/completion) |

Two-phase, mirroring today's split (request = enforce, response = account):

- **HTTP_REQ (or HTTP_REQ_DATA for tier limits): gate via a `consume=1` probe.**
  `ratelimit.exceed(name, consumer, 1)` — if the bucket is empty this trips and we
  reject. Costs 1 token per admitted request (negligible vs a thousands-token
  budget) and avoids depending on undocumented `consume=0` peek behavior, which the
  Broadcom docs do NOT confirm (verified 2026-06). `consume=0` peek can be
  confirmed empirically later as an optional accuracy refinement.
- **HTTP_RESP_DATA: consume.**
  Parse `usage` from the body exactly as today, then
  `avi.vs.ratelimit.exceed(name, consumer, total_tokens)` to drain the bucket.

```lua
-- request phase (gate)
if avi.vs.ratelimit.exceed(LIMITER, consumer, PROBE) then  -- PROBE: see OQ1
  avi.http.response(429, {... Retry-After ...}, '{"error":"token_budget_exceeded",...}')
  return
end
-- response phase (account), HTTP_RESP_DATA, after usage parse
avi.vs.ratelimit.exceed(LIMITER, consumer, total_tokens)
```

## 2a. Chosen approach: HYBRID (native enforce + DataScript display)

Decision: native limiter is the **authoritative gate**; the existing DataScript
table counter is kept **for display only** so the admin/dashboard endpoint still
works. Concretely, per native-backed limit:

- **Request phase:** native gate only — `ratelimit.exceed(name, consumer, 1)` →
  reject if exceeded. (The old table-lookup-and-reject block is REMOVED — the table
  no longer enforces.)
- **Response phase (HTTP_RESP_DATA):**
  1. `ratelimit.exceed(name, consumer, total_tokens)` — authoritative consume.
  2. `table_remove` + `table_insert(counter+total_tokens)` — display counter
     (unchanged from today), so `buildCountersEndpointBlock` keeps returning usage.

Consequence to accept: the **displayed** number is the old per-SE-approximate value
and will not exactly match the globally-enforced bucket. Enforcement is exact and
cross-SE; the dashboard figure is "approximate per-SE usage." Documented in the UI
copy / endpoint response so it isn't mistaken for the enforced value.

## 3. Per-group / per-tier budgets

The real policy uses `groupBudgets` (`group1: 2347`, `group2: 4500`) keyed on a
claim, and tier budgets keyed on the `ai_tier` reqvar. A native limiter has ONE
`count`, so:

- Generate **one named RateLimiter per distinct budget value**, e.g.
  `<vs>-<limit>-g-group1` (count 2347), `<vs>-<limit>-g-group2` (count 4500).
- At request time, read the group claim / `ai_tier` reqvar → pick the limiter name
  → gate against it. Store the chosen limiter name in a reqvar.
- At response time, read that reqvar back → consume `total_tokens` against the
  same limiter.
- Unknown group with fallback budget 0 → reject (unchanged behavior).

Tier limits already enforce in HTTP_REQ_DATA (after model routing sets `ai_tier`);
the reqvar is still readable in HTTP_RESP_DATA, so the consume picks the right
limiter.

## 4. What changes / what we lose (decisions, §7)

1. **Window semantics change.** A token bucket *refills continuously* (2347/hr ≈
   0.65 tok/s), it does NOT hard-reset on the clock boundary like the current
   fixed-window. Smoother and arguably better, but different — a drained consumer
   regains allowance gradually instead of all at once at the hour.
2. **Admin counters endpoint breaks for native limits — CONFIRMED unavoidable.**
   The dashboard reads running totals from the SE table
   (`buildCountersEndpointBlock`); the native limiter has **no read/query API for
   bucket state** (verified against Broadcom docs, 2026-06). Native-backed limits
   cannot feed the per-user usage UI. Options: (a) accept the loss, or (b) HYBRID —
   native limiter *enforces*, a DataScript table counter still runs for *display
   only* so the dashboard works (double-counting; displayed value is the old
   per-SE-approximate number). → decision in §7.4a.
3. **Bounded overshoot** (unchanged from today): the request that tips a consumer
   over still completes; the *next* one is blocked.
4. **`action: Log`** (count-but-allow) doesn't map cleanly — the limiter enforces.
   Native backend would support Reject only; Log stays DataScript.
5. **Counter epoch reset** (`counter-epoch` annotation) → maps to renaming the
   limiter (new name = fresh buckets). Workable.

## 5. CRD impact — backward compatible

Add an **opt-in, per-limit** backend selector; default preserves today's behavior:

```yaml
limits:
- name: hourly-group-budget
  backend: native      # NEW: "datascript" (default) | "native"
  ...
```

So existing policies are untouched; you switch a limit to `native` to get
cross-SE accuracy, and keep `datascript` where you need the dashboard or `Log`.

## 6. Implementation sketch (once design is signed off)

- `types.go`: add `TokenLimit.Backend` (default datascript).
- `datascript.go`: for native limits, emit the gate (REQ/REQ_DATA) + consume
  (RESP_DATA) blocks instead of the table_* blocks; skip the counters endpoint.
- `translator.go`: build one `*models.RateLimiter` per distinct budget value and
  attach to the relevant DataScript node(s) — reuses the `RateLimiters` plumbing
  already added for request-rate (nodes/rest/cache).
- tests + doc.

## 7. Open decisions for sign-off

1. **Pre-check mechanism — RESOLVED.** Use `consume=1` probe at request time
   (docs don't confirm a `consume=0` peek). No longer a blocker.
2. **Window semantics — DEFAULT ACCEPTED:** rolling token-bucket refill (inherent
   to the native limiter; no fixed-window option exists). Documented as a behavior
   change. Flag if you need a true calendar-window reset (would force a different
   design / stay on DataScript).
3. **CRD shape — PROPOSED:** per-limit `backend: datascript|native`, default
   `datascript` (fully backward compatible).
4a. **Token-count visibility — DECIDED:** HYBRID (native enforce + DataScript
   display counter). See §2a.
5. **Scope — DEFAULT:** Phase 1 = single per-consumer budget, end-to-end on the
   controller, to validate native gate + consume + hybrid display. Phase 2 = the
   per-group / per-tier mapping (§3) the real `llm-limits` policy needs.
