<!--
  RFE (Request for Enhancement) to Broadcom/VMware for the Avi Load Balancer Service Engine.
  Authored from the AKO AI Gateway work (docs/gateway-api/ai-gateway-*.md) and the live
  feasibility spikes on demo Avi 31.2.2 → 32.1.1. Engineering-grade; adapt tone for the
  customer-sponsored RFE channel as needed.
-->

# RFE — Streaming-aware, AI-native external-processing callout for the Service Engine

> **The ask, in one line.** Give the Service Engine a **bidirectional, streaming-aware,
> AI-aware external-processing callout** (request *and* response, per-chunk, allow/block/**modify**,
> with structured AI context) — a first-class primitive to replace the three general-purpose
> features (WAF, ICAP, DataScript) we currently **repurpose** for AI-gateway policy, each of
> which hits a documented wall that all traces back to the same gap.

---

## 1. Why (the root cause)

We have built an AI Gateway entirely on the Avi data plane — authentication, token budgets,
model-tier routing, MCP governance, WAF/DLP guardrails, and semantic prompt-injection
enforcement — with **no proxy and no sidecar** (see [ai-gateway.md](ai-gateway.md) and siblings).
The thesis works *because* the SE is programmable. But every AI-specific policy required
**repurposing a primitive built for something else**, and each one hits the **same** limit:
**there is no streaming-aware, AI-aware programmable callout on the SE.**

| Primitive | Built for | What we forced it to do | Where it walls |
|---|---|---|---|
| **WAF** (ModSecurity) | Web-attack signatures | Secrets/PII DLP + known-phrase injection | Signature/regex only — **no semantic** detection (a paraphrase bypasses it; spike-proven). |
| **ICAP** | 2003 antivirus / content-adaptation | Request-side semantic injection via an external classifier | Legacy protocol off-label; **request-only**; **block-only**; can't touch the response without **breaking streaming**; bespoke unmaintained shim; silent two-object trigger. |
| **DataScript** (Lua) | Lightweight L7 scripting | Body parsing, model routing, token metering | Sandboxed (**no regex**); **`RESP_DATA` is buffer-complete** → metering a streamed response **collapses the stream** (spike-proven). |

Three repurposed tools, three walls, **one root cause**. This RFE asks for the missing
primitive so AI-gateway policy is *first-class* instead of duct-taped.

---

## 2. Evidence — what we built, and exactly where the platform fell short

These are not hypotheticals; each was run live and torn down on the demo controller.

- **Semantic prompt-injection over ICAP (32.1.1, verified).** We stood up an ICAP classifier
  callout: SE → REQMOD → shim → model → allow(`204`)/block(`403`), enforced by the SE. It
  **works** — but only request-side, only block (no redaction), and:
  - **It silently does nothing until you also attach an `HTTP_SECURITY_ACTION_REQUEST_CHECK_ICAP`
    security-policy rule** — the `icapprofile` alone yields zero ICAP connections (a
    misconfiguration footgun on a *security* control).
  - **Response-side is unusable for streaming.** RESPMOD (or any full-response inspection)
    must buffer the completion — the same wall that breaks token metering — so indirect-injection
    scanning, output filtering, and redaction are off the table for SSE traffic.
  - The shim is a **bespoke 2003-protocol translator** (the only maintained Python ICAP lib
    needed a `collections.Callable` monkeypatch to run on Python 3.11).
- **Streaming token metering (DataScript).** `HTTP_RESP_DATA` fires **once, buffer-complete**;
  forcing the body read collapses a 5-chunk SSE backend into a single delivery. **Streamed LLM
  responses cannot be metered without destroying streaming** — so per-token budgets silently
  under-count streamed traffic today.
- **WAF semantic ceiling.** Custom prompt-injection SecRules block *known* phrases
  ("ignore all previous instructions" → 403) but a novel paraphrase → 200. Signatures cannot
  close the semantic gap; that is what drove us to the ICAP classifier in the first place.
- **Classifier-based routing blocked by pipeline stage order (31.2.1, spike-disproven
  2026-08-09).** We extended the ICAP classifier with an intent head (code vs. general) to
  route prompts to different model tiers. The mechanism — ICAP REQMOD injects `X-AI-Class`,
  the routing DataScript reads it — **cannot work**: the observed pipeline order is
  `DataScripts (routing) → ICAP → pool`, so the classifier verdict arrives **after** the
  routing decision it was meant to inform, and a body-phase DataScript short-circuits before
  the ICAP stage fires at all. ICAP is structurally a late-stage *gate*; it can never be a
  routing *input*. We shipped the workaround — classification at the gateway edge, upstream
  of the SE ([ai-gateway-classifier-routing.md](ai-gateway-classifier-routing.md) §0) — which
  works but moves an AI decision off the data plane the thesis says should own it.

Every one of these compromises disappears with a single primitive: a **streaming-aware,
bidirectional, transformative AI callout.**

---

## 3. What we're asking for

A native SE feature (working name **AI Inspection Callout** / SE ExtProc) with these properties:

1. **Bidirectional.** Invokable on the **request** *and* the **response**.
2. **Streaming-aware / per-chunk (the keystone requirement).** Deliver request and response
   bodies to the external service **incrementally, per chunk**, and let it inspect / meter /
   transform each chunk **without forcing full-body buffering**. This is precisely what ICAP
   RESPMOD and DataScript `RESP_DATA` cannot do today, and it is what unblocks streaming
   metering, output guardrails, and response-side injection scanning all at once.
3. **Transformative, not just allow/block.** Permit **modify** (redact a secret, strip an
   injected instruction, mask PII) and continue — not only reject. Enables redaction.
4. **AI-context-aware.** Pass structured context to the callout — matched route, target model,
   verified identity/claims, surface (`inference`/`mcp`/`a2a`) — so the external service does
   not re-parse provider JSON. Optionally native body-field extraction.
5. **Modern transport, ideally `ext_proc`-compatible.** A gRPC streaming API aligned with the
   widely-adopted Envoy `ext_proc` contract, so the **existing ecosystem of external-processing
   services works unmodified** (instead of a bespoke ICAP shim per deployment).
6. **Production-grade callout plumbing.** Pooled + health-monitored callout targets, bounded
   concurrency, tunable timeout, and an explicit **fail-open / fail-closed** policy (default
   **closed** for security controls).
7. **Native metering events.** Emit per-chunk token/byte counters natively (subsumes the
   streaming-metering RFE — see §4).
8. **CRD / Gateway-API attachable.** Configurable as a referenceable object the way WAF and
   ICAP profiles are, so AKO can author and attach it (and author it *correctly* — no silent
   two-object trigger).
9. **Placeable before routing, with the verdict visible to routing.** The callout must be
   invokable at a request stage that runs **before pool/pool-group selection**, and its
   verdict (class label, score, arbitrary key/values) must be readable by the routing
   decision (DataScript reqvar / policy match criteria). Without this, every property above
   still leaves intent/complexity-based model routing impossible on the SE — the exact wall
   the 2026-08-09 spike hit (§2). This is a *placement* requirement, orthogonal to transport
   and streaming: verdict-feeds-routing is what turns the callout from a gate into a signal.

A complementary/alternative form: **native AI-guardrail and LLM-inspection objects** (an
`llmprofile` / guardrail object that runs a classifier natively) — but the callout above is more
general and unblocks more, so it is the primary ask.

---

## 4. What this single primitive subsumes

| Existing gap / RFE | How the callout closes it |
|---|---|
| Streaming token metering (`RESP_DATA` buffer-complete) | Per-chunk response events meter the stream without buffering it. |
| Semantic prompt-injection (ICAP, request-only) | Request-side callout, but now with a first-class transport and correct config. |
| **Indirect injection** in tool results / RAG / A2A messages | Response-side, per-chunk inspection — currently impossible. |
| **Output guardrails / system-prompt-leak detection** | Response-side inspection. |
| **Redaction / masking** (vs. block-only) | Transformative `modify` on request and response. |
| Semantic caching response synthesis | Foundation for serving/capturing responses at the callout. |
| **Classifier/intent-based model routing** (blocked by stage order; today edge-orchestrated) | Pre-routing callout stage (§3.9) exposes the class verdict to tier selection — the routing decision returns to the data plane. |
| ICAP misconfiguration footgun, bespoke shim, legacy protocol | Replaced by a single, correctly-attachable, `ext_proc`-style object. |

One feature, the whole response-side AI surface — the exact surface where the no-proxy thesis
currently strains (and where inline AI-native proxies otherwise win).

---

## 5. Impact & priority

- **Strategic:** it is what makes "the Avi data plane *is* the AI Gateway" a *first-class*
  claim rather than a clever set of workarounds — and it closes the response-side gap that is
  today the strongest argument for adopting a separate AI-native proxy.
- **Breadth:** unblocks streaming metering, response-side guardrails, redaction, indirect-injection
  defense, and caching — from one primitive.
- **Risk reduction:** removes a bespoke, security-critical ICAP shim and a silent-fail trigger
  from the enforcement path.
- **Ecosystem:** `ext_proc` compatibility means customers reuse existing external-processing
  services instead of writing protocol shims.

**Until it ships,** the documented workarounds stand: WAF for signatures, a **narrow,
fail-closed, AKO-authored** ICAP callout for request-side injection
([ai-gateway-guardrails-semantic.md](ai-gateway-guardrails-semantic.md)), and non-streaming
metering — each carrying the limits in §1. This RFE is the path from "works, with caveats" to
"native."

---

## 6. Related

- [Semantic Guardrails (ICAP)](ai-gateway-guardrails-semantic.md) — the workaround this replaces, and its spike evidence
- [Classifier-Based Routing](ai-gateway-classifier-routing.md) — the stage-order disproof (§0) and the edge-orchestration workaround this returns to the SE
- [Guardrails & DLP (WAF)](ai-gateway-guardrails.md) — the signature layer and its semantic ceiling
- [Token ledger §4 — Streaming](ai-gateway-token-ledger.md) — the metering wall this subsumes
- [AI Gateway](ai-gateway.md) — the policy family that rides on these primitives
