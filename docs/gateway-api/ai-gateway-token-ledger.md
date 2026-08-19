# AI Gateway token ledger — true consumption, per user and per agent

> **Status: DESIGNED (2026-08-19).** Step 0 (the accuracy fix) is **BUILT** —
> commit `711e8293`. Companion to
> [ai-gateway-auth.md](ai-gateway-auth.md) (where identity comes from),
> [model-routing.md](model-routing.md) (where `ai_tier` comes from), and
> [rfe-se-ai-native-callout.md](rfe-se-ai-native-callout.md) (the streaming ask).
>
> **Decisions taken:** the ledger is an **out-of-band collector with its own
> store** (not Avi client logs, not the SE table as a store), and the accuracy
> bar is **demo-honest** — exact where we can measure, badged where we cannot,
> never a fiction presented as a measurement.

## 1. The problem: one number doing two jobs

Today the dashboard renders the *enforcement* counter as if it were an
*accounting* figure. They want opposite things:

| | Enforcement | Accounting |
|---|---|---|
| Wants | fast, SE-local, **pessimistic** | exact, durable, **attributable** |
| Bounded overage | acceptable | a lie |
| Over-charging | safe | a lie |
| Lifetime | one window | forever |
| Cardinality | one scalar per identity | one record per request |

`avi.vs.table_*` is the right structure for the first and the wrong structure
for the second. This design keeps the SE counter exactly as it is for budgets,
and adds a ledger beside it.

### What the current mechanism gets wrong

| # | Defect | Where |
|---|---|---|
| 1 | Penalty charged whenever `usage` was absent → one `GET /v1/models` = 65 536 tokens | `datascript.go` — **fixed**, `711e8293` |
| 2 | Streaming counts **zero** (`text/event-stream` never buffered) → silent budget bypass | `buildBufferEnableBlock` |
| 3 | No history — key is `floor(now/window)*window` with a TTL; at the boundary the number drops to 0 and the bucket is gone | `counterKeyExpr` |
| 4 | Per-SE **and** per-VS — two SEs hold two half-counts; LLM / mcp-gateway / a2a VSes keep separate tables, so there is no estate total | `avi.vs.table_*` |
| 5 | No enumeration — `?users=a,b,c` only shows identities you already guessed | `buildCountersEndpointBlock` |
| 6 | Lost increments — `lookup → remove → insert` is not atomic | `buildRespLimitBlock` |
| 7 | One dimension (`sub` × limit) — no model, tier, surface, or cached/reasoning split, so no cost and no "which model burned it" | schema |
| 8 | Mixed provenance — hand-built agents fall back to pod-env figures with nothing saying so | `ai-gateway-ui/counters.go` |

## 2. The record

One immutable row per metered response. Everything the console wants falls out
of this being a **table with columns** rather than a scalar in a hashtable.

```
ts          unix millis, when the response completed
identity    the JWT sub the SE keyed on
kind        user | agent | mcp | unknown
surface     llm | mcp | a2a          -- which gateway VS
route       HTTPRoute name
model       resolved model / alias
tier        ai_tier reqvar (premium|standard|embed|provider:*)
prompt      tokens
completion  tokens
cached      prompt_tokens_details.cached_tokens
reasoning   completion_tokens_details.reasoning_tokens
quality     exact | estimated | penalty | none   -- ai_meter_quality
se          which Service Engine measured it
cost        derived at read time from (model, tier) rates
```

- **users vs agents is `WHERE kind = …`** — one component, two panels. It stops
  being two different mechanisms, which is what makes the current agent-card
  badges and the user counters disagree.
- history is `GROUP BY` on `ts`; enumeration is `SELECT DISTINCT`; cross-SE and
  cross-surface totals are just rows.
- `quality` is the honesty column. Reported consumption sums `exact` only.
  `penalty` rows are shown as budget events, never as measured tokens.

`kind` is derived, not guessed: the factory issues per-agent ServiceAccounts and
`POST /exchange` mints `sub` from a TokenReview, so an agent sub is
recognisable. Console personas from `POST /persona` are users. Anything else is
`unknown` and is shown as such.

## 3. Getting a record off the SE

The collector owns the store, but the SE still has to hand a record over. The SE
table is a poor **store** and a perfectly good **short transport queue** once
there is a durable consumer draining it.

```
HTTP_RESP_DATA                    collector (out of path)          console
──────────────                    ───────────────────────          ───────
parse usage                       every ~5s, for each SE:          GET /api/usage
 ├─ increment budget counter        GET https://<se-ip>/v1/admin/    ?from&to&kind
 │  (unchanged, enforcement)          usage?after=<seq>              &groupBy
 └─ append record to ring          Host: llm.ai.avi.com
    ai_usage:<seq>  TTL 120s       persist rows, advance <seq>
```

- **Sequence + ring.** The script writes `ai_usage:<n>` with a short TTL and
  bumps a per-SE `ai_usage_seq`. The collector asks for everything after the
  sequence it last stored, so a drain is idempotent and a missed poll is
  recoverable inside the TTL.
- **Per-SE addressing is the point.** A GET on the VIP lands on whichever SE the
  LB picks, which is exactly why today's counters under-report on a multi-SE
  group. The collector instead enumerates SEs from
  `/api/serviceengine?page_size=200` — the console already does this for the
  topology view (`avi.go`) — and drains **each SE's data-plane IP directly**
  with the VS `Host` header. Deterministic, and the `se` column falls out.
- **Drain, don't accumulate.** The TTL only has to outlive the poll interval.
  Nothing long-lived is kept on the SE, so defect 3 and 4 both go away without
  the SE holding any history.
- **No new hop in the data path.** The collector is a consumer of the SE, not a
  proxy in front of it. The no-in-path-proxy constraint is about enforcement and
  is untouched: the budget counter and the 429 stay entirely on the SE.

**Where it runs.** A mode inside `ai-gateway-ui` (it already holds the Avi
client, the SE list, and the JWT minter) with an embedded store, rather than a
new deployment. Same auth as the counters endpoint — the minted-JWT claim gate,
not the shared secret.

**Falls back cleanly.** If the ring write is unavailable on a given build, the
collector still records what the existing `/v1/admin/counters` scalar reports as
a single `quality=estimated` row per poll. Degraded, labelled, never silent.

## 4. Streaming

The one thing the ledger does not fix. `HTTP_RESP_DATA` is buffer-complete
(probed live): reading the body forces buffering, which collapses the stream.
So a streamed response cannot be measured in a DataScript today.

Three moves, in order:

1. **Inject `stream_options:{include_usage:true}`** into the request body in
   `HTTP_REQ_DATA` — the model-route script already rewrites bodies there. This
   makes the final SSE frame actually carry usage, and is a prerequisite for
   anything below.
2. **Spike response trailers.** Can a DataScript read a trailer in a post-body
   event without buffering? If yes, the backend emits `X-AI-Usage`, streams
   meter exactly, and the whole model above works unchanged. **This single
   unknown sets the ceiling** — it is the make-or-break spike.
3. **Until then: reserve, and say so.** Pre-flight estimate from prompt chars/4
   plus `max_tokens`, written as `quality=estimated`. It charges the budget
   (closing today's bypass, defect 2) and appears in the console as an estimate,
   never summed into the exact figure.

The rule that makes the console trustworthy: **never render an estimate as a
measurement.** "1.2 M tokens · 94 % exactly metered" is a stronger claim than a
bare 1.2 M that is quietly wrong.

## 5. The console

A **Tokens** tab beside Agents and Flow Map.

- **KPI strip** — tokens (24 h) · cost · requests · **% exactly metered**.
- **History** — stacked area over time, split prompt / completion / cached, or
  by tier. The thing that is impossible today, and the most convincing single
  visual that this is a real meter.
- **Users | Agents** — two panels, one component, `kind` filter. Row = identity,
  sparkline, tokens, cost, budget bar (used/limit from the live SE counter), and
  a provenance dot: solid = SE-measured exact, hollow = estimated, amber =
  penalty. Three states, one glyph, no legend needed.
- **Drill-down** — click an identity for its per-model split, its surfaces
  (llm / mcp / a2a), and recent requests.

The budget bar stays wired to the live SE counter, not the ledger: that is the
number enforcement will actually act on, and showing anything else would make
a 429 look unexplained.

## 6. Sequence

| Step | Work | Gated on |
|---|---|---|
| 0 | **Accuracy fix** — penalty on evidence only, `meter_quality` published | ✅ `711e8293` |
| 1 | Record emit: ring + sequence in `HTTP_RESP_DATA`; dimensions (model, tier, surface, cached, reasoning) | — |
| 2 | Collector + store in `ai-gateway-ui`; per-SE drain; `/api/usage` read API | 1 |
| 3 | Cost table (model × tier × prompt/completion rates) | 2 |
| 4 | Tokens tab | 2 |
| 5 | Streaming: `include_usage` injection, then the trailer spike | independent |

## 7. Known adjacent risk

`a2aroute_datascript.go` and `mcproute_datascript.go` still use the bare
`pcall(avi.http.X)` / `pcall(avi.pool.X)` form throughout. Per
`avi-datascript-gotchas`, that protects nothing on this sandbox — the field
access is evaluated before `pcall` runs, and an unknown name raises. Those
scripts work today, so the names they use evidently exist; but any future rename
or build change fails hard rather than degrading. `TestNoBareAviHTTPProbes`
guards the token-accounting scripts only. Worth a sweep, not part of this work.
