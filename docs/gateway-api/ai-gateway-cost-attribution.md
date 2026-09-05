# AI Gateway — cost attribution across user, agent and tool

Status: designed 2026-09-05; **phase 1 LIVE and verified the same day** (chain id on the record, console → hub → agent propagation, Chains view; a coordinator turn as `alice` joined as 4 hops across agent-hub and load-controller). Phases 2-4 open. Companion to
[ai-gateway-token-ledger.md](ai-gateway-token-ledger.md) (the record and the collector) and
[ai-gateway-agentminder-pdp.md](ai-gateway-agentminder-pdp.md) §9 (the audit join).

## 1. Two questions the ledger cannot answer today

The ledger holds one row per metered LLM response:

```
ts | identity | model | tier | prompt | completion | cached | reasoning | quality | route
```

and the console prices each row from a per-model rate table. That answers *who called the model
and what it cost*. It does not answer:

1. **What did this user's request cost in total?** A chat turn fans out — the persona's own model
   call, the hub's planning calls, each delegated agent's calls, each tool call — and every hop is
   billed to whichever identity made it. The rows exist; nothing links them.
2. **What does a token cost on *this* tier?** Price lives in a UI env var keyed by model name, is
   unset on the live console (everything is priced at the default), and has no notion of tier —
   yet tier is the thing the estate actually chooses per request.

## 2. The chain id

One id, minted where a request enters the estate, carried on every hop, written into every record.

- **Format.** W3C `traceparent` (`00-<32 hex trace-id>-<16 hex span-id>-<flags>`). The trace id is
  the chain id. A standard header means any client or proxy that already traces will join up
  without us asking; our own components mint one when none arrives.
- **Propagation.** The console mints it per chat turn and sends it to the front door and to the
  hub. The hub forwards it on its model calls, its MCP calls and every `tasks/send`. agent-runtime
  reads it from the inbound A2A request and forwards it on its model and MCP calls. MCP servers
  only receive it.
- **The record.** `chain` is appended as an eleventh field. The format is positional, so the
  collector accepts ten or eleven fields and an SE running an older script still drains. The SE
  reads the header in `HTTP_REQ`, keeps the trace id in the `ai_chain` reqvar, and writes it from
  `HTTP_RESP_DATA` beside the tokens.
- **Tool calls become rows.** The MCP DataScript already parses `tools/call` to enforce the
  per-tool gate; the same place appends a row with `identity = sub`, `model = tool name`,
  `tier = mcp`, zero tokens, `quality = exact`. A tool's cost is then the model tokens of the turns
  that used it, computed at read time from the chain — never guessed at the SE.

```
console  (persona alice, chain C1)
  ├─ POST llm-route              alice          | qwen  | quality | C1
  └─ POST hub /chat ──── agent-hub
       ├─ POST llm-route         agent-hub      | qwen  | gpu     | C1
       └─ tasks/send ──── load-controller
            ├─ POST llm-route    load-controller| qwen  | gpu     | C1
            └─ tools/call ─────  load-control-am
                                 a61cd13c…      | load_status | mcp | C1
```

## 3. Who the user is — verified, not asserted

No hop claims an originator in a header; a header would be trusted text from an agent. The
chain's root row is the persona's own SE-verified call, so every later row in the chain is
attributed to that persona at read time. For coordinator chats, where the console hands the turn
to the hub before any model call, the console registers the root (chain id → persona) itself at
chat start; it is the component that authenticated the user.

The verified form is delegation in the token — AgentMinder's `act` chain via RFC 8693 token
exchange — read by the SE. That waits on the SE exposing non-`sub` claims from a bearer
([ai-gateway-auth.md](ai-gateway-auth.md)); the chain join is the out-of-band form of the same
fact and is what the pdp design's §9 already prescribes.

## 4. Price on the tier

`AIModelRoutePolicy` is where `fast`, `standard`, `quality` and `premium` are defined, so it is
where the price belongs:

```yaml
modelTiers:
- name: premium
  backendRef: {name: vllm-gpu, port: 8000}
  pricing:
    currency: USD
    promptPerMillion: 10000     # "10c per token" — demo pricing, labelled as configured
    completionPerMillion: 10000
```

- AKO bakes the rate into the model-route DataScript beside the tier choice; the metering script
  multiplies and writes `cost` into the record as a twelfth field. The SE then meters money, not
  only tokens, and the console stops needing a rate table of its own.
- **Cost-weighted budgets.** The native budget charges the Avi rate limiter with a token count.
  Charging it with `tokens × weight`, where weight is the tier's price relative to the cheapest,
  turns a token budget into a spend cap: premium burns it ten times faster than standard, and the
  429 names the tier. Same knob, same limiter, no new object.
- The console reads the price list from the policy (via the Kubernetes API it already watches),
  shows it as *configured*, and shows the effective cost per token per identity — the number that
  exposes who is living on premium.

## 5. The console

Under **Dashboard ▸ Tokens**:

- **Chains** — one row per chain in the window: persona, total tokens, total cost, hop count,
  duration. Expand to the tree: each hop's identity, model or tool, tier, tokens, cost. Chains with
  no root persona are shown under *unattributed*, not dropped.
- **Roll-ups** — cost per user (own calls plus everything their chains caused), cost per agent,
  cost per tool (chains that used it, and their model spend), all from the same rows.
- **Price list** — per model and tier, from the policy, labelled configured. Effective cost per
  token per identity beside the existing cost column.
- The raw-record ring the console already keeps for drill-down (20 000 records) is what chains are
  built from; rollups stay hourly. A chain older than the ring is gone from the tree but still in
  the totals — say so on the page.

## 6. Phasing

| Phase | Change | Where | Risk |
|---|---|---|---|
| 1 | chain id end to end; eleventh record field; Chains view | agent-runtime, hub, console, one AKO script line | none to enforcement |
| 2 | `pricing` on the tier; `cost` in the record; cost-weighted native budgets | AKO CRD + DataScripts, sandbox-tested | changes what a budget means — flag per policy |
| 3 | tool rows from the MCP gate | AKO MCP DataScript | small |
| 4 | verified originator (AgentMinder `act`) | blocked on the claims RFE | — |

## 7. Caveats that carry over

- Streaming responses still cannot be metered ([ai-token-streaming-limit]); a chain through a
  streaming turn shows a gap, not an estimate.
- Prices are demo prices. Every cost figure is labelled *configured*, never *observed*.
- Hub and agents mint their own tokens; the chain does not change who is *authorised* for a hop,
  only who is *charged* for it in the report.
