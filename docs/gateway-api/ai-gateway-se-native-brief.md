<!-- One-page brief for a PM Senior Director. Companion to rfe-se-ai-native-callout.md. -->

# AI Gateway on the Avi Data Plane — Proven in Demo, Ready to Make Native

**Bottom line.** The demo proves the use case: a complete AI Gateway — identity, usage
governance, model routing, and guardrails — running **entirely on the Avi Service Engine, with
no in-cluster proxy or sidecar.** The architecture is right and it works. The ask is narrow:
the capabilities we proved with **repurposed primitives** (DataScript, ICAP, an external
classifier) should become **first-class SE features**. Same architecture we demo today — just
executed natively, so it streams, scales, and stands on its own.

**Why now.** Every AI-gateway competitor adds a *new inline proxy* (a Rust/Envoy data plane,
sidecars). Our differentiator is that the governance runs on the **load balancer the customer
already operates** — one identity, one policy plane, zero proxy sprawl. The demo validates that
claim. Making these capabilities SE-native is what turns it from "works, with caveats" into a
**first-class platform claim** — and closes the one surface (response-side) where a bolt-on AI
proxy can still out-feature us.

## Request path — one picture

```
    Client
      │  TLS
      ▼
┌─────────────────────  Avi Service Engine — the only proxy  ─────────────────────┐
│                                                                                  │
│   Auth          Guardrails                Routing            Metering            │
│  (OAuth)  ──▶   WAF signatures ✅   ──▶   model-tier ✅  ──▶  non-stream ✅       │
│    ✅           Semantic ⚠️ (ICAP)                           stream ⚠️           │
│                                                                                  │
└───────────────────────────┬───────────────────────────────┬─────────────────────┘
              │ (semantic callout)                           │
              ▼                                               ▼
      external classifier  ← the only "brain"        Model / MCP / A2A backends
                                                                                   
   ✅ already native on the SE        ⚠️ works via workaround → move to the SE
```

The whole path already runs on the SE. The ⚠️ marks are the work to move into the data-plane
core — and they cluster in **guardrails (semantic)** and **metering (streaming)**.

## What runs today vs. what we move to the SE

| AI Gateway capability | How the demo runs it today | Target: native on the SE | Status |
|---|---|---|---|
| **Authentication (OAuth/OIDC)** | SE-native Avi OAuth/SSO; verifies token, resolves `sub`+`group` | — (already native) | ✅ on SE |
| **Identity propagation** (`x-ai-consumer`) | DataScript header inject | Native identity forwarding | ✅ on SE |
| **Inference load balancing** (metric-weighted pools) | SE-native Inference Extension (KV-cache/queue-weighted) | — (already native) | ✅ on SE |
| **Model-tier routing** (quality/cost, entitlement-gated) | DataScript parses `model` from body → pool-group select | Native model-aware routing (read `model` without scripting) | ✅ works → native |
| **Signature guardrails / DLP** (secrets, PII, known injection) | SE-native WAF (ModSecurity SecRules) | — (already native) | ✅ on SE |
| **Request rate limiting** | DataScript soft token bucket | Native distributed rate limiter | ✅ works → native |
| **Token metering — non-streaming** | DataScript counters in SE shared state | Native per-response token accounting | ✅ works → native |
| **Token metering — streaming** | **Not possible** — `RESP_DATA` is buffer-complete; metering a stream collapses it | **Native per-chunk metering** that taps the live stream | ⚠️ **move to SE** |
| **Multi-cluster / global budgets** | Per-SE eventually-consistent string table | **Native distributed token counter** (global budget across the fabric) | ⚠️ **move to SE** |
| **Semantic prompt-injection** (novel/paraphrase) | **ICAP callout** to an external classifier — request-only, block-only | **Native streaming-aware AI callout** (request + response) | ⚠️ **move to SE** |
| **Response-side guardrails** (output DLP, indirect injection, system-prompt leak) | **Not feasible** — response inspection breaks streaming | **Native per-chunk response inspection** | ⚠️ **move to SE** |
| **Redaction / masking** (vs. blunt block) | Block-only (WAF/ICAP reject the whole request) | **Native modify** — strip/mask and continue | ⚠️ **move to SE** |
| **Token ledger — accounting** | DataScript writes one usage record per metered response into an SE table; an out-of-band collector drains it. Recording is exact; the drain caps out near 200 rps | **Native usage export** (an event per metered response, pushed) | ✅ works → native |
| **MCP governance** (session pinning, tool authz) | Native MCP application profile (32.1.1) + AKO DataScripts. ⚠️ Avi's own `System-Standard-MCP` session script **raises on an EVH child VS behind a PoolGroup**, so AKO writes its own equivalent | Native MCP session pinning that works on the EVH topology AKO builds + per-role tool authz | ⚠️ partial → native |
| **A2A** (agent↔agent, body-derived affinity) | DataScript captures the task id from the response and keys Avi persistence on it — **built and verified**, not design | Native body-derived session affinity | ✅ works → native |
| **Semantic caching** | Not built (needs response synthesis) | Native response serve/capture at the callout | ⚠️ **future, on SE** |
| **Usage observability** (live counters) | Admin counters endpoint + DataScript | Native usage telemetry | ✅ works → native |

> **The one thing that stays off the SE — honestly.** The classifier/LLM *model* remains an
> external callable service (you don't run ML on a load balancer). What moves to the SE is the
> **inspection/enforcement/metering hook** — the data-plane work — so the model is *called*
> natively instead of through a hand-built ICAP shim. Likewise the **config/policy plane**
> (CRDs via AKO, multi-cluster via vDefend SSP) is control-plane by design, not SE work.

## The efficient path: one primitive unlocks most of the ⚠️ rows

Six of the seven "move to SE" rows trace to **one missing capability**: a **streaming-aware,
bidirectional, AI-aware external-processing callout** on the SE (request *and* response,
per-chunk, allow/block/**modify**). It is the root-cause fix that subsumes streaming metering,
response-side guardrails, redaction, indirect-injection defense, and the semantic-injection
callout — replacing three repurposed primitives (WAF signatures-only, ICAP off-label,
DataScript no-regex/buffer-complete) with one first-class feature. Detail and live spike
evidence: **[rfe-se-ai-native-callout.md](rfe-se-ai-native-callout.md)**.

## The ask

Prioritize the **native streaming-aware AI callout** (+ native distributed token counter) on the
SE roadmap. The demo de-risks it — every capability is proven working; we are asking to move the
proven workarounds into the data-plane core so the AI Gateway streams, scales, and wins the
response-side surface, on the load balancer customers already run.
