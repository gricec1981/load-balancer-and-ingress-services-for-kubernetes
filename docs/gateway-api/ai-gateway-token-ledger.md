# AI Gateway token ledger — true consumption, per user and per agent

> **Status: BUILT (2026-08-19), not yet exercised on the live estate.**
> Steps 0-4 are in: the SE emits usage records and serves a drain endpoint, the
> console collects them into a durable ledger, and Dashboard ▸ Tokens renders
> users and agents from it. Streaming (step 5) remains unsolved. Companion to
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

The SE writes it as a pipe-delimited line — the field order is the contract
between the two repos, and every value is reduced to a safe character set first,
so there is nothing to escape when the drain embeds it in JSON:

```
ts | identity | model | tier | prompt | completion | cached | reasoning | quality | route
```

The collector adds what only it can know:

```
inst        which SE ring the record came from
kind        user | agent | mcp | unknown   -- resolved at READ time
cost        derived at read time from (model → rate) and the cached discount
```

- **users vs agents is `WHERE kind = …`** — one component, two panels. It stops
  being two different mechanisms, which is what makes the current agent-card
  badges and the user counters disagree.
- history is `GROUP BY` on `ts`; enumeration is `SELECT DISTINCT`; cross-SE and
  cross-surface totals are just rows.
- `quality` is the honesty column. Reported consumption sums `exact` only.
  `penalty` rows are shown as budget events, never as measured tokens.

`kind` is resolved at READ time, from the issuer's persona list and the agent and
MCP registries — not stamped at ingest. An agent registered after its first call
is then classified correctly for its whole history, rather than being stuck as
`unknown` forever. Anything no registry knows is shown as `unknown` in its own
panel rather than dropped, so the kinds still partition the estate total.

## 3. Getting a record off the SE

The collector owns the store, but the SE still has to hand a record over. The SE
table is a poor **store** and a perfectly good **short transport queue** once
there is a durable consumer draining it.

```
HTTP_RESP_DATA                       collector (out of path)        console
──────────────                       ───────────────────────        ───────
parse usage                          every ~5s:                     GET /api/usage
 ├─ increment budget counter           GET https://llm.ai.avi.com/    ?window&kind
 │  (unchanged, enforcement)             v1/admin/usage?after=<seq>
 └─ append record to ring             → {inst, head, next, lost,
    ai_urec:<seq>   TTL 300s              records:[…]}
    ai_useq = "<inst>:<seq>"          cursor[inst] = next
```

- **Sequence + ring.** The script writes `ai_urec:<n>` with a 5-minute TTL and
  bumps the head. The collector asks for everything after the sequence it last
  stored, so a drain is idempotent and a missed poll is recoverable inside the
  TTL.
- **The instance id and the head live in ONE key** (`ai_useq = "<inst>:<seq>"`)
  so they expire together. If the instance could lapse while the sequence kept
  climbing, the collector would see a new instance, reset its cursor, and re-read
  every record still in the ring as if it were new.
- **Cursors are per RING INSTANCE, not per SE address.** The first plan here was
  to enumerate SEs from `/api/serviceengine` and drain each SE's own IP directly.
  **That does not work:** an Avi SE answers for a VS on its VIP, not on its
  management or data-interface address, so there is nothing to address. Instead
  each SE mints an instance id into its own table on first use and reports it in
  every drain response. The collector keeps one cursor per instance, so being
  bounced between SEs by the load balancer neither double-counts nor misses —
  and **keep-alives are disabled** on the collector's client, because the LB only
  gets to re-pick an SE on a new connection. A pooled connection would pin every
  poll to one SE and let the others age out of their rings unread.
- **Gaps are reported, not closed.** A cursor further behind than the scan window
  returns `lost: <n>`. A ledger that quietly drops rows is worse than one that
  admits it did; the console surfaces the count.
- **Drain, don't accumulate.** The TTL only has to outlive the poll interval.
  Nothing long-lived is kept on the SE, so defects 3 and 4 both go away without
  the SE holding any history.
- **No new hop in the data path.** The collector is a consumer of the SE, not a
  proxy in front of it. The no-in-path-proxy constraint is about enforcement and
  is untouched: the budget counter and the 429 stay entirely on the SE.

**Where it runs.** A mode inside `ai-gateway-ui` (it already holds the Avi
client, the SE list, and the JWT minter), not a new deployment. Same admin
credentials as the counters endpoint — shared secret or the minted-JWT claim
gate, whichever is configured.

**Two storage levels**, because they answer different questions. Hourly rollups
per (identity, model, tier, quality, route) are small and survive a pod restart
in a ConfigMap; the raw records stay in memory, capped, for the drill-down.
Dropping raw records is not dropping accounting — the rollups hold their sums.

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

- **KPI strip** — tokens measured · cost · metered requests · **% measured
  exactly**. The last tile is the one that makes the other three believable.
- **History** — stacked area over the window. Three bands: cached prompt tokens,
  fresh prompt tokens, completion tokens, with a crosshair and tooltip. Cached is
  drawn as a **texture inside the prompt band**, not as a third hue, because that
  is what it is — a portion of the prompt count the backend already reported. A
  third colour would read as "prompt + cached" and overstate every cached turn.
- **Users | Agents** — two panels, one component, `kind` filter. Row = provenance
  glyph, identity, sparkline, tokens, cost. MCP servers and anything
  unclassified get their own panels below, so nothing is silently dropped.
- **Provenance, on every row** — solid dot = every request measured exactly;
  hollow = partly unmeasured, with the percentage in the title; amber triangle =
  a budget was charged without a measurement. Trust in a number should not
  require reading a footnote.
- **Drill-down** — click a row for prompt/completion/cached/reasoning, request
  count, measured share, the penalty line if any, and its model and tier splits.
- **Export** — `GET /api/usage/export?format=csv` hands back the hourly rollups.
  Whatever the console draws, the underlying numbers can be taken out and
  checked; that is the difference between a dashboard people believe and one
  they merely look at. It also discharges the contrast WARN the palette
  validator raises for the light theme.

**Colour.** The two series carry their own CSS tokens rather than reusing the
console's shared `--chart-1`/`--chart-2` ramp. Those two ramp steps are
near-identical blues in the dark themes (ΔE 9.6 for *normal* vision, under the
15 floor), and prompt vs completion is exactly the pair a reader has to separate.
Each theme's pair was run through the palette validator for lightness band,
chroma floor, CVD separation and contrast against that theme's surface — do not
swap one without re-running that check.

The old **Live Counters** tab stays as it is, wired to the live SE counter. That
is the number enforcement will actually act on, and a budget bar showing anything
else would make a 429 look unexplained.

## 6. Sequence

| Step | Work | State |
|---|---|---|
| 0 | **Accuracy fix** — penalty on evidence only, `meter_quality` published | ✅ `711e8293` |
| 1 | Record emit: ring + sequence in `HTTP_RESP_DATA`; dimensions (model, tier, cached, reasoning, route); `GET /v1/admin/usage` drain | ✅ `ceed2d26` |
| 2 | Collector + store in `ai-gateway-ui`; per-instance cursors; `/api/usage`, `/api/usage/recent`, `/api/usage/export` | ✅ `155b6d2` (ui) |
| 3 | Cost table (`USAGE_RATES`, cached discount) | ✅ `155b6d2` (ui) |
| 4 | Dashboard ▸ Tokens | ✅ `155b6d2` (ui) |
| 5 | Streaming: `include_usage` injection, then the trailer spike | not started |

**Verification.** The generated Lua is executed, not string-matched:
`testdata/se_stub.lua` reproduces the SE sandbox — `tonumber(nil)` raises, Lua
patterns are restricted, an unknown `avi.*` field raises, `table_insert` does not
overwrite, and the body-buffer calls take kilobytes — and
`testdata/ledger_spec.lua` runs all three phases against it. A completion charges
exactly its `total_tokens` and records its dimensions; a read-only GET costs
nothing; an over-buffer response is still fail-closed but recorded as a penalty;
an over-budget consumer is still 429'd; a second drain at the returned cursor
repeats nothing. Skipped where no Lua interpreter is installed.

**Not yet run against the live estate.** The units question in §7 and the drain's
behaviour on a genuinely scaled-out VS are the two things the stub cannot settle.

## 7. Open risk: the body-buffer units

`avi.http.set_response_body_buffer_size` and `avi.http.get_response_body` are
called with `RespBodyBufferKB` (256) raw, and the truncation test compares Lua's
byte-count `#_body` against 262 144. That is correct **if both take kilobytes**,
which the production evidence supports — counters show figures like 607 and 9.3k
rather than multiples of 65 536, so real completions are parsing, and a real
completion is far longer than 256 bytes.

It is worth confirming directly, because being wrong is silent in both
directions: if the buffer were really 256 *bytes*, no `usage` would ever parse and
the penalty would never fire either, leaving completions unmetered.

## 8. Known adjacent risk

`a2aroute_datascript.go` and `mcproute_datascript.go` still use the bare
`pcall(avi.http.X)` / `pcall(avi.pool.X)` form throughout. Per
`avi-datascript-gotchas`, that protects nothing on this sandbox — the field
access is evaluated before `pcall` runs, and an unknown name raises. Those
scripts work today, so the names they use evidently exist; but any future rename
or build change fails hard rather than degrading. `TestNoBareAviHTTPProbes`
guards the token-accounting scripts only. Worth a sweep, not part of this work.
