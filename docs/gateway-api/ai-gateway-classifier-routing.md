<!--
  DESIGN DRAFT. Mirrors model-routing.md / ai-gateway-guardrails-semantic.md conventions.
  Stretch companion to ai-gateway-guardrails-semantic.md: reuses the SAME ICAP
  classifier hook (already proven, §8 of the semantic doc) to emit a CLASS signal
  that feeds tier selection alongside AIModelRoutePolicy — without adding a second
  pool selector. NOT built. The classifier + ICAP shim it depends on ARE built and
  live (prompt-injection scoring); this adds a second head + a routing input.
-->

# AKO AI Gateway — Classifier-Based Routing (intent/complexity → tier)

> **Status: the SE-native ICAP-header mechanism below (§2–§4) is BLOCKED on Avi
> 31.2.1 — SPIKE-DISPROVEN 2026-08-09. Implemented instead as UI-orchestrated
> classification (§0), which is BUILT + live on the lab cluster.** This design routes a
> request to a different model based on what the prompt *is* — its **intent class**
> (`code` vs everything else) — scored by the on-cluster classifier. The motivating
> example: **send code prompts to Claude, everyday prompts to a local model, with
> Gemini selectable.**

---

## 0. What was built — UI-orchestrated classification (the ordering finding)

> **Make-or-break, run live on Avi 31.2.1 (2026-08-09): the ICAP-injects-a-header,
> DataScript-reads-it mechanism (§2) does NOT work on this build — the routing
> DataScript runs BEFORE the ICAP check.** Spike: the shim was modified to inject
> `X-AI-Class` on allow (REQMOD 200-with-modified-req-hdr), and a debug DataScript
> on the child VS read the header. Result: the DataScript saw `X-AI-Class=(none)`
> **and the ICAP shim was never called** — because a body-phase (`HTTP_REQ_DATA`)
> DataScript short-circuits *before* the pipeline reaches the ICAP security stage.
> Observed order: `DataScripts (HTTP_REQ → HTTP_REQ_DATA, routing) → ICAP check →
> pool → backend`. ICAP is a good late-stage **gate** (block/allow — which is why
> guardrails still work), but it cannot feed an **earlier** routing decision. The
> DataScript sandbox also can't embed or call the classifier itself. Streaming does
> not help: it changes body *delivery*, not stage *order*, and request-body
> buffering (which routing depends on) is the opposite of streaming.
>
> **Implemented pivot (BUILT + verified live):** compute the class at the gateway
> **edge** — the AI-Gateway UI backend — *before* the request reaches the SE, and
> express it via the `model` field the existing model-route DataScript already
> routes on. No new pool selector, no ICAP dependency, all SE enforcement (auth,
> guardrails, budgets) unchanged.
>
> ```
> prompt → UI /api/chat classifies (HTTP → prompt-injection-icap /classify → nomic-embed)
>        → code?  model="claude"  :  model="qwen-fast"
>        → governed front door (?jwt=)   → auth ✓ guardrails/ICAP ✓ budgets ✓
>        → model-route DataScript reads "model" → tier/pool
> ```
>
> - **Classifier head:** an embedding code-vs-general intent scorer added to the
>   shim (reuses the running `nomic-embed`; `POST /classify {text}` → `{label,
>   score}`). Validated live: code prompts 0.88–0.99, general 0.00–0.29.
> - **Routing:** the UI's `model="auto"` path (`chat.go`) calls `/classify`, sets
>   `model` to `AUTO_CODE_MODEL` (default `claude`) or `AUTO_DEFAULT_MODEL`
>   (default `qwen-fast`); classifier-down fails safe to local. The reply shows the
>   route chip `✦ code (0.99) → claude`.
> - **Verified 2026-08-09:** "write a python function…" → code(0.99) → `claude`;
>   "recipe for banana bread" → other(0.00) → `qwen-fast`; both 200 through the
>   front door. (`claude` falls through to the default local tier until the real
>   provider backend — LiteLLM egress + keys — is wired; that is a backend swap, no
>   UI/classifier change.)
>
> **Trade-off vs the SE-native ideal:** the routing *decision* is computed at the
> edge (the UI, which is gateway infrastructure — not the end user's client) rather
> than inside the SE. The classifier is still on-cluster/private and all policy is
> still SE-enforced. A fully SE-native version needs a build whose content
> inspection runs *before* pool selection — re-test the §2 spike on a 32.1.1
> upgrade. The rest of this document (§2–§4) is retained as the SE-native design
> for that future path.

---

## 1. Why this rides on the guardrail classifier

The semantic-guardrail layer already puts a classifier in the request path over
ICAP REQMOD (proven; [guardrails-semantic §3/§8](ai-gateway-guardrails-semantic.md)).
That call embeds the prompt and scores it for injection. **The same embedding
answers a second question for almost free** — "what kind of request is this?" —
because the prototype classifier already computes similarity to labelled
exemplar sets. Adding a `code` / `general` / `simple` exemplar set and returning
the best-matching label alongside the injection score is one extra `max`-cosine
over vectors we've already produced (sub-millisecond; no second model call).

So classifier-based routing is **not** a new data-plane primitive. It is:

1. a second **head** on the existing shim (intent/complexity label), and
2. a way to feed that label into the **existing** tier selector
   ([`AIModelRoutePolicy`](model-routing.md)) — *not* a competing one.

That second point is the whole design. See §4.

---

## 2. The no-second-selector thesis (the overlap, resolved)

`AIModelRoutePolicy` already owns pool selection: its `HTTP_REQ_DATA` DataScript
reads the body `model`, resolves a tier, and calls **`avi.poolgroup.select()`**
directly ([model-routing.md](model-routing.md#the-generated-datascript-two-events)).
**There must be exactly one caller of `avi.poolgroup.select` per request** — two
scripts selecting different Pool Groups is a last-writer-wins race and an
operability nightmare.

Therefore the classifier does **not** select a pool. Instead, ICAP REQMOD (which
is *designed* to modify the request it inspects) has the shim **inject a request
header** — `X-AI-Class: code` (and optionally `X-AI-Complexity: high`) — into the
encapsulated request before the SE forwards it. The **model-route DataScript**
then reads that header exactly the way it reads `model`, and folds it into the
one tier decision it already makes. One selector, one decision, an extra input.

```
                 ┌───────────────── one classifier call ─────────────────┐
client ─▶ SE ──ICAP REQMOD──▶ shim ──▶ embed prompt ──▶ injection score ─┤─▶ block? (guardrail)
              (buffered body)   │                       intent label ────┘      │
              ◀── 204 + inject ──┘   X-AI-Class: code                           │
                                                                                ▼
              SE HTTP_REQ_DATA: AIModelRoutePolicy DataScript                (allow)
                tier = classTier[X-AI-Class]  or  modelTiers[model]  or  defaultTier
                avi.poolgroup.select(TIER_PG[tier])       ◀── the ONLY selector
```

Guardrail block still short-circuits first: a blocked request never reaches the
routing decision.

---

## 3. What the classifier returns

The shim's HTTP contract to the classifier gains two fields (backward compatible):

```json
{ "score": 0.11, "label": "benign",
  "class": "code", "class_scores": {"code":0.71,"general":0.42,"simple":0.20} }
```

- **`class`** — the argmax over the routing exemplar sets, emitted only when its
  margin over the runner-up clears a confidence floor; otherwise `class` is
  omitted and the router falls back to model-name/default tiering (no header
  injected). Low-confidence classification must **degrade to the existing
  behaviour**, never guess.
- Routing classes are **operator-defined** by the exemplar sets shipped with the
  shim (a ConfigMap), e.g. `code` (write/debug/review code), `general` (everyday
  Q&A, drafting), `simple` (greetings, one-liners). No retraining — add exemplars.

Trust boundary: the SE must **strip any client-supplied `X-AI-Class` header
before ICAP** so a caller can't spoof their way onto the premium tier. The header
is only trustworthy because the shim (not the client) sets it. This mirrors the
token-in-URL caution in [ai-gateway-auth](ai-gateway-auth.md) — the SE owns the
signal, not the client.

---

## 4. CRD — extend `AIModelRoutePolicy`, don't add a CRD

Routing stays in one policy. Add an optional `classify` block and let `modelTiers`
keys carry a `class:` selector alongside model names:

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIModelRoutePolicy
metadata: { name: llm-tiers, namespace: inference }
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route }

  tiers:
    - { name: claude-code, backendRef: { kind: InferencePool, name: claude-egress } }   # code → Claude
    - { name: gemini-gen,  backendRef: { kind: InferencePool, name: gemini-egress } }    # general → Gemini
    - { name: local-fast,  backendRef: { kind: InferencePool, name: pool-fast } }        # simple → local

  # NEW: consume the classifier's X-AI-Class header (set by the ICAP shim).
  classify:
    enabled: true
    header: X-AI-Class          # what the shim injects; SE strips client copies
    classTiers:                 # class → tier (evaluated when body `model` is unset/ambiguous)
      code:    claude-code
      general: gemini-gen
      simple:  local-fast

  # Existing explicit model→tier map still wins when the caller names a model.
  modelTiers:
    "claude-*": claude-code
    "gemini-*": gemini-gen
  defaultTier: gemini-gen
```

**Precedence (baked into the generated Lua, deterministic):**

1. explicit **`model`** in the body matches `modelTiers` → that tier (caller intent
   is explicit; honour it);
2. else **`class`** header present and in `classTiers` → that tier;
3. else **`defaultTier`**.

Entitlements (`entitlements`, [model-routing.md](model-routing.md#tier-entitlement-by-group-with-auth))
apply **after** this resolution, unchanged — a `free` group asking for the
`claude-code` tier still downgrades. Classifier routing chooses a *candidate*
tier; entitlement remains the gate.

| New field | Type | Default | Description |
|---|---|---|---|
| `classify.enabled` | bool | `false` | Consume the classifier class header for tiering. |
| `classify.header` | string | `X-AI-Class` | Request header the ICAP shim injects. |
| `classify.classTiers` | map[string]string | — | class → `tiers[].name`. Validated against real tiers. |

---

## 5. The Claude/Gemini example end-to-end

The `claude-egress` / `gemini-egress` tiers are ordinary backends whose members
are the provider endpoints reached over the SE's egress path (resolve the public
host → selectorless Service + manual EndpointSlice — see the AKO ExternalName
egress note; **don't** use `ExternalName`, it yields no endpoints). A
`RouteBackendExtension` supplies the upstream TLS + the provider API key/header
rewrite. Then:

- `"write a python function to reverse a string"` → shim `class=code` → header
  `X-AI-Class: code` → tier `claude-code` → Claude.
- `"what's a good recipe for banana bread"` → shim `class=general` → tier
  `gemini-gen` → Gemini.
- `"hi"` → shim `class=simple` → tier `local-fast` → the on-cluster fast model
  (no egress cost at all).

One front door, one policy; the routing brain is the classifier you already run
for guardrails.

---

## 6. Honest ceiling

- **Latency is already paid if guardrails are on.** The class label is computed in
  the same ICAP call as the injection score, so with semantic guardrails enabled
  classifier routing adds ~0 ms. **Standalone** (routing without guardrails) it
  costs one ICAP round-trip (~145 ms p50 measured on nomic-embed) — cheaper than a
  wrong route to a slow/expensive tier, but not free. Gate it behind the WAF like
  the guardrail path so cheap/known traffic skips the classifier.
- **Misclassification routes, it doesn't break.** A wrong class sends a good
  request to a suboptimal tier (a Claude prompt to Gemini), never to failure.
  Low-confidence → no header → existing tiering. This is why classification is a
  *routing hint layered on* model/default tiers, not a hard gate.
- **One selector, enforced by design.** The classifier never calls
  `avi.poolgroup.select`; only the model-route DataScript does. If a future
  feature wants the classifier to select directly, that supersedes this design —
  it must not run alongside it (two selectors race). Documented so the two never
  fight.
- **Header trust.** Relies on the SE stripping client `X-AI-Class` before ICAP; if
  a build can't strip it, use an SE-internal reqvar set from the ICAP response
  instead of a header (same idea, not client-reachable).
- **Class taxonomy is operator-owned.** Exemplar drift = routing drift; ship a
  `class_scores` shadow-log mode (like guardrail `action: Log`) to tune the
  exemplar sets against real traffic before enabling `classify`.

---

## 7. Build outline (if promoted from stretch)

1. **Shim head** — add routing exemplar sets (ConfigMap) + argmax-with-margin;
   return `class` when confident; inject `X-AI-Class` via ICAP REQMOD header
   modification. (Shim + classifier already deployed; this is a second scoring
   pass + a header write.)
2. **Types/CRD** — `classify` block on `AIModelRoutePolicy`
   ([`modelroute_types.go`](../../ako-gateway-api/aigateway/), the CRD YAML), with
   validation that `classTiers` values are real tiers.
3. **DataScript generator** — extend the model-route `HTTP_REQ_DATA` script: after
   the `model` lookup and before `defaultTier`, consult the class header
   (`avi.http.get_header`). No new `avi.poolgroup.select` call site.
4. **SE header hygiene** — ensure the child VS strips inbound `X-AI-Class` before
   the ICAP security rule runs (an HTTP request rule removing the header, authored
   next to the REQUEST_CHECK_ICAP rule).
5. **Shadow mode** — `classify.mode: Log` emits `class_scores` to analytics
   without changing tiers, for taxonomy tuning.

---

## 8. Related docs

- [Semantic Guardrails](ai-gateway-guardrails-semantic.md) — the ICAP classifier + shim this reuses
- [Model-Based Routing](model-routing.md) — the single pool selector this feeds (the overlap resolved in §2)
- [Auth](ai-gateway-auth.md) — the SE-owns-the-signal trust-boundary precedent
- [Backend mTLS](ai-gateway-backend-mtls.md) — the egress/upstream path for external provider tiers (Claude/Gemini)
