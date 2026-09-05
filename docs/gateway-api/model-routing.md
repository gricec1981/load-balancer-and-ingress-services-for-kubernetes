# AKO AI Gateway — Model-Based (Quality/Cost Tier) Routing

> **Status: Built — verified end-to-end on live clusters (Avi 31.2.2, then
> 31.2.1).** The `AIModelRoutePolicy` CRD routes inference requests to different
> backends based on the requested **model**, organised into **quality/cost
> tiers**. It builds on the [AI Gateway](ai-gateway.md) and
> [Inference Extension](inference-extension.md) and reuses their existing
> machinery (request-body DataScript parsing, OAuth claim access, scraper-weighted
> InferencePool Pool Groups).
>
> A tier can be served by any of **four** backend kinds, all shipped:
>
> | Tier kind | Field | Backend | Shipped |
> |---|---|---|---|
> | Inference pool | `backendRef` (`kind: InferencePool`) | scraper-weighted per-pod members | 2026-06-07 |
> | Core Service | `backendRef` (`kind: Service`) | Service endpoints — including a selectorless Service + hand-written EndpointSlice fronting a GPU VM or bare metal | 2026-06-07 |
> | External provider | `provider` | an OpenAI-compatible vendor API (Gemini) over SE egress | 2026-08-09 |
> | Remote site | `remote` | a **peer AI Gateway in another cluster**, addressed by FQDN | 2026-08-23 |
>
> `spec.discovery` (2026-08-08) additionally lets a labelled model pod register its
> own alias into the model→tier table, so deploying a model and publishing it
> through the gateway become one action.
>
> The control-plane path (CR → AKO → per-tier Avi Pool Groups + model-route
> DataScripts) is verified live; the data-plane primitive
> (`avi.http.get_req_body` + `avi.poolgroup.select` keyed on the body `model`) was
> confirmed by spike on the same build. The standing constraints are the **32 KB**
> request-body buffer and the fact that **tier pools are baked at policy-reconcile
> time** — see [Limitations](#limitations).

---

## Overview

The Inference Extension load-balances **within** a single model fleet: one
`InferencePool` → one Avi Pool Group → one Pool per vLLM pod, with member ratios
driven by scraped metrics. Every request to the route hits the *same* model.

**Model-based routing** adds the missing layer *above* that: it inspects the
OpenAI-style request, reads the requested `model`, and steers it to a **different
backend** — typically a different `InferencePool` running on different hardware.
Organising those backends into **tiers** (e.g. `premium` on H100s, `standard` on
A10s, `economy` on CPU/quantised) turns model routing into a cost-and-quality
control plane:

- **Cost control** — expensive GPU pools serve only the requests that are
  entitled to them; everything else lands on cheaper hardware.
- **Entitlement** — reuse the **verified `group` claim** already produced by
  [`AIGatewayAuthPolicy`](ai-gateway.md#oauthoidc-authentication--aigatewayauthpolicy)
  to decide which callers may reach which tier — no second auth hop.
- **Graceful degradation** — a caller who asks for a tier they aren't entitled
  to (or a tier that is over budget) can be **downgraded** to a cheaper tier
  instead of being rejected.

This is the productised form of the roadmap's `InferenceObjective` /
model-name-based-routing item ([inference-extension.md
Limitations](inference-extension.md#limitations)).

---

## The core constraint

Gateway API `HTTPRoute` matches on **path / header / query / method** only —
never the request body (see [`route_model.go`](../../ako-gateway-api/nodes/route_model.go)
`PathMatch` / `HeaderMatch`). But the `model` field lives in the **POST JSON
body**:

```json
POST /v1/chat/completions
{ "model": "llama-3-70b-instruct", "messages": [ ... ] }
```

So native matching cannot see it. The AI Gateway already solved the equivalent
problem for token accounting — it buffers and parses the **response** body in a
DataScript (`HTTP_RESP_DATA`, see [`datascript.go`](../../ako-gateway-api/aigateway/datascript.go)).
Model routing applies the same technique to the **request** body in `HTTP_REQ`.

The elegant part: **the DataScript does not select the backend directly.** It
extracts `model`, resolves the tier, checks entitlement, and writes an internal
**tier header** (`avi.http.add_header("X-AI-Tier", …)`). AKO then builds one
ordinary content-switch rule per tier — an HTTP request rule that matches the
header and runs `HTTP_SWITCHING_SELECT_POOLGROUP` against that tier's Pool Group
(exactly the action AKO already emits in
[`avi_model_l7_dedicated_translator.go:322`](../../ako-gateway-api/nodes/avi_model_l7_dedicated_translator.go#L322)).
Body→header in Lua; **native content-switch does the routing**. No new Avi
pool-selection primitive is required, and each tier's Pool Group remains
independently metric-weighted by the inference scraper.

---

## Ownership model — backends live in the policy (decided)

There are two places the per-tier `InferencePool` backends could be declared.
**This design uses Option A: the backends are declared in the
`AIModelRoutePolicy` (`tiers[].backendRef`), not on the `HTTPRoute`.**

| | **A — policy owns backends (chosen)** | **B — route owns backends (rejected)** |
|---|---|---|
| Where the `InferencePool`s are listed | in the `AIModelRoutePolicy` | as `HTTPRoute` backendRefs, one rule per tier keyed on `X-AI-Tier` |
| New translator code | `ApplyModelRoutePolicy` invokes the existing inference pool-build per tier | none — reuses the route-backend path verbatim |
| What the policy generates | Pool Groups **+** content-switch rules **+** DataScript | just the DataScript |
| Change tiers without editing the route | yes | no |
| **Behaviour with the policy absent / not yet reconciled** | route keeps its own `backendRef` → all traffic hits the default backend (no tiering, but **works**) | nothing sets `X-AI-Tier` → no rule matches → **404 / outage** (unless a catch-all rule is added) |

The deciding factor is the **failure mode**. Under A, a missing or
not-yet-reconciled policy degrades to "no tiering" — benign. Under B, the
HTTPRoute is incomplete on its own (its header-match rules have nobody producing
the header), so the same condition becomes an outage. For a policy governing
production LLM traffic, benign degradation wins. A also keeps the feature
self-contained in one CRD and lets operators retune tiers without touching the
route.

The cost of A is real translator work: the existing graph layer only builds
inference Pool Groups from a *route's* backendRefs
([`buildInferencePoolMembers`](../../ako-gateway-api/nodes/avi_model_l7_translator.go#L905)),
so that logic must be made callable from the policy path — see
[Implementation outline](#implementation-outline--applymodelroutepolicy).

---

## How It Works

```
┌──────────────────────────────────────────────────────────────┐
│                 Kubernetes Control Plane                      │
│                                                              │
│  AIModelRoutePolicy ──────────────────────────────────┐     │
│  (tiers, model→tier map, group entitlements)          │     │
│                                                        ▼     │
│  AKO (ako-gateway-api)                                       │
│   BuildChildVS → ApplyModelRoutePolicy                       │
│    • one Pool Group per tier  (from each tier's backendRef)  │
│    • DataScriptSet pool_group_refs = the tier Pool Groups    │
│    • HTTP_REQ DataScript     (enable request-body buffering) │
│    • HTTP_REQ_DATA DataScript(parse model → poolgroup.select)│
└───────────────────────────┬─────────────────────────────────┘
                            │ Avi REST API
                            ▼
┌──────────────────────────────────────────────────────────────┐
│  Avi Virtual Service (HTTPS)                                  │
│   HTTP_REQ:      avi.http.set_request_body_buffer_size(32768) │
│   HTTP_REQ_DATA:                                             │
│     1. body  = avi.http.get_req_body(32768)                 │
│     2. model = json_str(body, "model")                      │
│     3. tier  = MODEL_TIERS[model]   (else prefix / default) │
│     4. group = avi.http.oauth_get_claim(0,"group")          │
│     5. if group not entitled to tier → downgrade or reject   │
│     6. avi.poolgroup.select(TIER_PG[tier])                  │
│     7. avi.http.set_reqvar("ai_tier", tier)                 │
└───────┬───────────────┬───────────────┬─────────────────────┘
        ▼               ▼               ▼
  InferencePool   InferencePool   InferencePool
   premium-llm     standard-llm    economy-llm
  (H100 pods)      (A10 pods)      (CPU/quantised)
   ↑ scraper-weighted members per tier (unchanged)
```

**Control flow**

1. User creates an `AIModelRoutePolicy` targeting an `HTTPRoute`. Each tier names
   a `backendRef` — currently an **`InferencePool`** (Service backends are
   accepted by the schema but not yet built; see [Limitations](#limitations)).
2. AKO's informer parses the CR, caches it, and re-enqueues the targeted route.
3. During `BuildChildVS`, `ApplyModelRoutePolicy` builds **one Pool Group per
   tier** (reusing the existing InferencePool→Pool Group translation, so each
   tier keeps its metric-weighted members), **one content-switch rule per tier**,
   and **one `HTTP_REQ` DataScript** generated from the model→tier map and the
   entitlement table.
4. At request time the SE runs the DataScript, tags the request with `X-AI-Tier`,
   and the content-switch rule routes it to the matching tier's Pool Group.

---

## Prerequisites

- Everything in the [AI Gateway prerequisites](ai-gateway.md#prerequisites)
  (`featureGates.GatewayAPI: true`, `aiGateway.enabled: true`).
- The **inference extension enabled** (`inferenceExtension.enabled: true`) — tier
  backends are `InferencePool`s, so the scraper must be running.
- **Install the CRD**:
  ```bash
  kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aimodelroutepolicies.yaml
  ```
- **RBAC**: the `ako` ClusterRole must allow `aimodelroutepolicies` (+ `/status`).
  The Helm chart grants this automatically when `aiGateway.enabled: true`; if you
  are running an AKO whose ClusterRole predates this feature, the informer logs
  `aimodelroutepolicies ... is forbidden` until you add the grant.
- One **`InferencePool` per tier** deployed (each selecting that tier's pods) and
  named in the policy's `tiers[].backendRef`.
- For **group-based entitlement**: an `AIGatewayAuthPolicy` on the same route, so
  the SE-verified `group` claim is available via `avi.http.oauth_get_claim()`.
  Without it, entitlement is skipped (route by model only).

> **Note on the InferencePool API group.** AKO selects tier backends by `kind:
> InferencePool` (the `group` field is informational). Use whichever group your
> installed InferencePool CRD serves — e.g. `inference.networking.x-k8s.io`.

---

## CRD — `AIModelRoutePolicy`

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIModelRoutePolicy
metadata:
  name: llm-tiers
  namespace: inference
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route

  # The JSON body field to route on. Default "model" (OpenAI-compatible).
  modelField: model

  # Tiers, ordered most→least preferred. Each maps to a backend.
  tiers:
    - name: premium
      backendRef:
        group: gateway.inference.x-k8s.io
        kind: InferencePool
        name: premium-llm          # e.g. H100 pods, full-precision 70B
    - name: standard
      backendRef:
        group: gateway.inference.x-k8s.io
        kind: InferencePool
        name: standard-llm         # e.g. A10 pods, 8B
    - name: economy
      backendRef:
        group: gateway.inference.x-k8s.io
        kind: InferencePool
        name: economy-llm          # e.g. quantised / CPU

  # Which requested model maps to which tier. A model not listed here uses
  # defaultTier (below). Supports exact names and a trailing "*" prefix glob.
  modelTiers:
    "llama-3-70b-instruct": premium
    "gpt-oss-120b":         premium
    "llama-3-8b-instruct":  standard
    "mistral-7b*":          standard
    "*-q4":                 economy
  defaultTier: economy

  # Optional: which auth groups may reach which tiers. Requires an
  # AIGatewayAuthPolicy on the same route (verified `group` claim).
  # `groupClaim` names the JWT claim to read (default "group").
  entitlements:
    groupClaim: group
    rules:
      - group: platinum          # may use any tier
        allow: [premium, standard, economy]
      - group: gold
        allow: [standard, economy]
      - group: free
        allow: [economy]

  # What to do when a caller requests a tier they are NOT entitled to,
  # or the requested model is unknown.
  onUnentitled:
    type: Downgrade              # Downgrade | Reject
    # For Downgrade: drop to the highest tier the caller IS entitled to.
    # For Reject: return statusCode.
    statusCode: 403
```

### Tiers that are not local pods

Three of the four tier kinds do not name a Kubernetes backend at all. They are
declared on the same `tiers[]` entry, in place of `backendRef`:

```yaml
spec:
  tiers:
    # 1. External provider — an OpenAI-compatible vendor API over SE egress.
    #    AKO authors an FQDN pool (backend TLS + SNI); the HTTP_REQ_DATA script
    #    rewrites the path and Host and injects the key from a Secret.
    - name: frontier
      provider:
        host: generativelanguage.googleapis.com
        path: /v1beta/openai/chat/completions
        auth:
          secretRef: {name: gemini-key, key: apiKey}
          header: x-goog-api-key
          scheme: ""             # raw key, no "Bearer " prefix

    # 2. Remote site — a PEER AI Gateway in another cluster. Same pool shape as a
    #    provider tier, but nothing is rewritten except Host, and the peer's
    #    response is metered normally because it is ours.
    - name: qwen-antrea
      remote:
        host: llm.siteb.ai.avi.com
        port: 443
        tls: true
        preserveHost: false      # rewrite Host so the peer's EVH child VS matches
        healthPath: /v1/models   # "-" attaches no monitor (and disables failover)

    # 3. Core Service — endpoints, not pods. A selectorless Service plus a
    #    hand-written EndpointSlice fronts serving infrastructure outside the
    #    cluster (a GPU VM, bare metal) as a first-class tier.
    - name: gpu-vm
      backendRef:
        kind: Service
        name: vllm-gpu
```

### Discovery — a model pod registers its own alias

With `spec.discovery.enabled`, AKO merges aliases found on running pods into
`modelTiers` at translation time. A pod carrying the alias **annotation** and a
tier **label** naming a declared tier joins the table; deleting the pods
un-registers the alias and its requests fall back to `defaultTier`. Static
`modelTiers` entries always win over discovered ones, and `namespaces` is the
governance gate — only pods in those namespaces may register.

```yaml
spec:
  discovery:
    enabled: true
    aliasAnnotation: ai.ako.vmware.com/model-alias   # default
    tierLabel:       ai.ako.vmware.com/tier          # default
    namespaces: [inference]                          # default: the policy's own
```

With discovery off (or absent) behaviour is byte-identical to a static-only
policy — see [`modelroute_discovery.go`](../../ako-gateway-api/aigateway/modelroute_discovery.go).

### Field reference

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.targetRef.*` | PolicyTargetRef | yes | Same shape as the other AI Gateway policies; `HTTPRoute` (or `Gateway`). |
| `spec.modelField` | string | no | JSON body key to read. Default `model`. |
| `spec.tiers[].name` | string | yes | Tier identifier; becomes the `ai_tier` reqvar value and part of the Pool Group name. |
| `spec.tiers[].backendRef` | BackendRef | no† | `InferencePool` (keeps per-pod metric weighting) or `Service` that serves this tier. |
| `spec.tiers[].provider` | ModelProvider | no† | External OpenAI-compatible API. `host`, `path` required; `port` 443, `tls` true, `auth.header` `Authorization`, `auth.scheme` `Bearer` by default. |
| `spec.tiers[].remote` | ModelRemote | no† | Peer AI Gateway at another site. `host` required; `port` 443, `tls` true, `preserveHost` false, `healthPath` `/v1/models` by default. |
| `spec.modelTiers` | map[string]string | yes | `model name (or "prefix*")` → tier name. |
| `spec.defaultTier` | string | yes | Tier for models not in `modelTiers`. |
| `spec.discovery.enabled` | bool | no | Merge pod-published aliases into `modelTiers`. Default off. |
| `spec.discovery.aliasAnnotation` | string | no | Pod annotation carrying the alias. Default `ai.ako.vmware.com/model-alias`. |
| `spec.discovery.tierLabel` | string | no | Pod label carrying the tier. Default `ai.ako.vmware.com/tier`. |
| `spec.discovery.namespaces` | []string | no | Namespaces allowed to register aliases. Default: the policy's namespace. |
| `spec.entitlements.groupClaim` | string | no | Claim/header carrying the caller's group. Default `group`. |
| `spec.entitlements.rules[].group` | string | yes* | A group value (required when `entitlements` is present). |
| `spec.entitlements.rules[].allow` | []string | yes* | Tiers this group may reach. |
| `spec.onUnentitled.type` | string | no | `Downgrade` (default) or `Reject`. |
| `spec.onUnentitled.statusCode` | int | no | HTTP status when `type: Reject`. Default `403`. |

† Exactly one of `backendRef`, `provider` or `remote` per tier.

---

## Avi object mapping

| Policy element | Avi mechanism |
|---|---|
| Each `tiers[]` entry with a `backendRef` | One **Pool Group** (`<vsname>-tier-<name>-pg`) built through the node graph. An `InferencePool` backend keeps its scraper-weighted per-pod members; a `Service` backend produces a single pool from the Service's endpoints. |
| Each `tiers[]` entry with a `provider` or `remote` | One **FQDN pool + pool group authored directly over the Avi REST API** (`EnsureProviderTier` / `EnsureRemoteTier`), not through the node graph. Backend TLS + SNI; `resolve_server_by_dns` so the SE owns every re-resolution. A `remote` tier also gets an HTTP health monitor on `healthPath` — which **must send a `Host` header**, or the peer answers 404 and a healthy peer is marked down. |
| `modelTiers` + `defaultTier` + `entitlements` | One **DataScriptSet** (`<vsname>-ai-model-route`) with a `HTTP_REQ` script (enable request-body buffering) and a `HTTP_REQ_DATA` script (read the body, resolve the tier, apply entitlement, **select the tier's Pool Group**). |
| Tier selection | The `HTTP_REQ_DATA` script calls **`avi.poolgroup.select("<vsname>-tier-<name>-pg")`** directly. Every selectable Pool Group must be listed in the DataScriptSet's **`pool_group_refs`** (Avi rejects the script otherwise). |

> **Verified mechanism (spike, Avi 31.2.2).** The DataScript selects the Pool
> Group *directly* — no `X-AI-Tier` header and no `HTTP_SWITCHING_SELECT_POOLGROUP`
> content-switch rules are needed. See
> [Feasibility — verified](#feasibility--verified-on-avi-3122). The header +
> content-switch approach remains a valid fallback but is unnecessary now that
> `avi.poolgroup.select()` is confirmed callable from the body event.

The model→tier map and entitlement table are **baked into the generated Lua** as
constant tables (the Avi sandbox has no external lookups), the same way the
token-budget DataScript bakes in `groupBudgets`. A policy change regenerates the
DataScript and re-pushes it on the next reconcile.

---

## The generated DataScript (two events)

The body is only readable in `HTTP_REQ_DATA`, so the work is split across two
events (the same shape as the token-accounting scripts). The generator follows
the patterns in [`datascript.go`](../../ako-gateway-api/aigateway/datascript.go)
(sandbox-safe `string.find` + quote scan, `pcall`-guarded claim access, no
`string.match`):

```lua
-- Event 1 — HTTP_REQ: enable request-body buffering (max 32 KB on this build)
pcall(function() avi.http.set_request_body_buffer_size(32768) end)
```

```lua
-- Event 2 — HTTP_REQ_DATA: read body, resolve tier, select the Pool Group
-- 1. Read the buffered request body (verified function name: get_req_body)
local ok, body = pcall(function() return avi.http.get_req_body(32768) end)
body = (ok and body) or ""

-- 2. Extract the model with a sandbox-safe scan for `"model"`
local model = extract_json_string(body, "model")    -- generated helper

-- 3. Resolve the tier from the baked-in map (exact, then prefix glob, then default)
local tier = MODEL_TIERS[model] or match_prefix(model) or DEFAULT_TIER

-- 4. Entitlement: read the verified group claim, downgrade or reject
local group = jwt_claim("group")                     -- shared helper, AI Gateway
if not entitled(group, tier) then
  local downgraded = best_allowed(group)             -- highest allowed tier
  if downgraded then
    tier = downgraded
  else
    avi.http.response(403, {["Content-Type"]="application/json"},
      '{"error":"tier_not_entitled"}')
    return
  end
end

-- 5. Route directly to the tier's Pool Group (verified: avi.poolgroup.select).
--    TIER_PG[tier] is the baked-in "<vsname>-tier-<name>-pg" name; every value
--    must also appear in the DataScriptSet's pool_group_refs.
avi.poolgroup.select(TIER_PG[tier])
```

`extract_json_string`, `match_prefix`, `entitled`, `best_allowed`, `MODEL_TIERS`,
and `TIER_PG` are emitted as constant tables/helpers from the CR. `jwt_claim` is
the **same helper the token policy already generates**
([`datascript.go`](../../ako-gateway-api/aigateway/datascript.go)
`jwtClaimHelper`), so auth-aware entitlement reuses proven code.

---

## Composition with auth and token budgets

All three policies attach to the same `HTTPRoute` via `targetRef` and are applied
in the same `BuildChildVS` cycle. Event ordering on the VS:

1. **OAuth/OIDC** (`AIGatewayAuthPolicy`) — validates the token; claims become
   readable via `oauth_get_claim`.
2. **Model routing** (`AIModelRoutePolicy`, `HTTP_REQ` → `HTTP_REQ_DATA`) — reads
   the `group` claim, resolves the tier, and selects the Pool Group. It also
   records the chosen tier with `avi.http.set_reqvar("ai_tier", tier)`.
3. **Token budget** (`AITokenRateLimitPolicy`, `HTTP_REQ` / `HTTP_RESP_DATA`) —
   enforces per-consumer/per-group budgets.

Two powerful combinations fall out:

- **Per-tier token budgets (implemented).** A token limit selects its budget
  ceiling by tier using `groupHeader: "reqvar:ai_tier"` + `groupBudgets`
  (e.g. `{premium: 500, economy: 100000}`). Because the tier is only known after
  the body is read, such limits are **enforced in `HTTP_REQ_DATA`** (the token
  policy emits a dedicated `-ai-tok-reqdata` script), which runs *after* the
  model-route script set `ai_tier`. This relies on two facts confirmed by spike
  on Avi 31.2.2: a reqvar set in one DataScriptSet is **visible to another** in
  the same event, and execution follows **DataScript index order** — so AKO gives
  the model-route DataScript a lower index than the token enforcement one. The
  counter stays per-consumer; only the ceiling is tier-dependent.

  ```yaml
  # AITokenRateLimitPolicy on the same route — per-tier ceilings
  limits:
    - name: per-tier-budget
      key: consumer
      tokens: total
      window: 1h
      groupHeader: "reqvar:ai_tier"      # read the tier set by model routing
      groupBudgets: { premium: 500, economy: 100000 }
      budget: 0                          # unknown tier -> 403
  ```

  > Ordering caveat: the **classic** `HTTP_REQ`-phase budget check cannot see
  > `ai_tier` (set later in `HTTP_REQ_DATA`). That is exactly why tier-dependent
  > limits are emitted in the `HTTP_REQ_DATA` enforcement script instead; limits
  > that don't reference a reqvar keep enforcing in `HTTP_REQ` unchanged.

- **Budget-aware downgrade (future).** If a caller is over their premium budget,
  the model-route script could downgrade them to economy instead of a hard 429 —
  a natural Phase 2.x extension once both scripts share the `ai_tier`/counter
  state.

---

## Examples

### Pure model routing (no auth)

Route by model name only; unknown models fall to economy.

```yaml
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route }
  tiers:
    - { name: premium,  backendRef: { group: gateway.inference.x-k8s.io, kind: InferencePool, name: premium-llm } }
    - { name: economy,  backendRef: { group: gateway.inference.x-k8s.io, kind: InferencePool, name: economy-llm } }
  modelTiers:
    "llama-3-70b-instruct": premium
  defaultTier: economy
```

### Tier entitlement by group (with auth)

`free` users are silently downgraded to economy even if they ask for a 70B model.

```yaml
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route }
  tiers:
    - { name: premium,  backendRef: { group: gateway.inference.x-k8s.io, kind: InferencePool, name: premium-llm } }
    - { name: economy,  backendRef: { group: gateway.inference.x-k8s.io, kind: InferencePool, name: economy-llm } }
  modelTiers: { "llama-3-70b-instruct": premium }
  defaultTier: economy
  entitlements:
    groupClaim: group
    rules:
      - { group: paid, allow: [premium, economy] }
      - { group: free, allow: [economy] }
  onUnentitled: { type: Downgrade }
```

---

## Implementation outline — `ApplyModelRoutePolicy`

> **Shipped as described.** The entry point is
> [`ApplyModelRoutePolicy`](../../ako-gateway-api/nodes/avi_model_route.go) in
> `ako-gateway-api/nodes/avi_model_route.go`, called from
> [`avi_model_l7_translator.go`](../../ako-gateway-api/nodes/avi_model_l7_translator.go)
> alongside `ApplyAuthPolicy` / `ApplyTokenRateLimitPolicy`. Two details differ
> from the sketch below: pod-discovered aliases are merged in first
> (`WithDiscoveredModels`), and `provider` / `remote` tiers bypass the node graph
> entirely — `EnsureProviderTier` / `EnsureRemoteTier` author their FQDN pool and
> pool group directly over the Avi REST API, and the DataScript selects that pool
> group by name.

Following the pattern of the existing two AI Gateway policies, model routing
plugs into the same per-child-VS hook in
[`avi_model_l7_translator.go`](../../ako-gateway-api/nodes/avi_model_l7_translator.go#L175)
where `ApplyAuthPolicy` / `ApplyTokenRateLimitPolicy` are already called:

```go
if lib.IsAIGatewayEnabled() {
    ...
    for _, mrp := range ps.GetModelRoutePoliciesForRoute(routeNsName) {
        aigateway.ApplyModelRoutePolicy(key, mrp, childNode, parentNs, parentName, routeModel)
    }
}
```

`ApplyModelRoutePolicy(key, policy, vsNode, parentNs, parentName, routeModel)`
does four things, all on the **child** VS node:

1. **Build one Pool Group per tier.** For each `tiers[].backendRef`, resolve the
   `InferencePool` and build its Pool Group + per-pod pools by reusing the
   existing inference path. This is the one piece of new wiring: today
   [`buildInferencePoolMembers`](../../ako-gateway-api/nodes/avi_model_l7_translator.go#L905)
   is driven from `BuildPGPool` over a *route's* backends, so it must be lifted
   into a helper that `ApplyModelRoutePolicy` can call per tier. Each tier's pool
   must also be **registered with the inference controller** (the same
   `SharedInferenceController()` consumer registration the route path does at
   [line 917](../../ako-gateway-api/nodes/avi_model_l7_translator.go#L917)) so the
   scraper polls it and the `GlobalWeightStore` is populated per tier.
2. **Generate the DataScriptSet** from `modelTiers` + `defaultTier` +
   `entitlements` — a `HTTP_REQ` script (enable request-body buffering) and a
   `HTTP_REQ_DATA` script (read body, resolve tier, `avi.poolgroup.select`) — and
   attach it on `vsNode` the same way `ApplyTokenRateLimitPolicy` attaches the
   `-ai-tok-*` scripts. Name it `<vsname>-ai-model-route`.
3. **Populate `pool_group_refs`** on that DataScriptSet with every tier's Pool
   Group. Avi statically validates `avi.poolgroup.select("name")` against this
   list and **rejects the script (HTTP 400) if a selected Pool Group is not
   referenced** — confirmed in the spike. No HTTP content-switch rules are needed;
   the script selects the Pool Group directly.
4. **Ordering.** The model-route scripts must run after the auth DataScript (so
   `oauth_get_claim` is populated). The body read happens in `HTTP_REQ_DATA`, so
   tier selection naturally lands after any `HTTP_REQ`-phase auth work; set the
   DataScript indices so auth < model-route < token within each event.

Supporting work, mirroring the existing CRDs:

- **CRD + Go types** (`pkg/apis/...`/`ai.ako.vmware.com_aimodelroutepolicies.yaml`)
  with deepcopy, plus an informer and a `PolicyStore` entry
  (`GetModelRoutePoliciesForRoute`) in [`aigateway`](../../ako-gateway-api/aigateway/).
- **Re-enqueue on CR events** so an `AIModelRoutePolicy` add/update/delete
  re-runs `BuildChildVS` for the targeted route (the auth/token policies already
  do this through the shared `PolicyStore`).
- **Validation** that every `modelTiers` value and `defaultTier` names a real
  `tiers[].name`, and that `entitlements.rules[].allow` reference real tiers.

> **Status semantics.** Because backends live in the policy (Option A), the
> policy's `status.conditions` reports unresolved/ missing tier `InferencePool`s
> — not the HTTPRoute's `ResolvedRefs`.

---

## Feasibility — verified on Avi 31.2.2

The build-specific unknowns were settled with a live spike on the demo controller
(Avi **31.2.2**, build 9059) using a throwaway VS + DataScriptSet + two pool
groups, since torn down. Results:

1. **Request body IS readable at request time. ✅** There is no `get_request_body`
   /`get_body`; the actual API is:
   - `HTTP_REQ`: `avi.http.set_request_body_buffer_size(N)` — **enables buffering,
     N capped at 32768 (32 KB)**; passing 65536 errors `buf size should between 0
     and 32768`.
   - `HTTP_REQ_DATA`: **`avi.http.get_req_body(N)`** returns the buffered body.
   A `{"model":"llama-3-70b-instruct",…}` POST yielded `body_len=137` and the
   model extracted cleanly with the existing sandbox-safe `string.find` + quote
   scan. The event `VS_DATASCRIPT_EVT_HTTP_REQ_DATA` exists and is accepted.
2. **Pool-group selection from the body event works at runtime. ✅**
   `avi.poolgroup.select(name)` and `avi.pool.select(name)` are present and
   callable in `HTTP_REQ_DATA`. End-to-end test: requests identical except the
   `model` field routed deterministically — `model` containing `prem` reached the
   vLLM Pool Group (200, mock `chat.completion`), other models reached the second
   Pool Group. So routing is driven entirely by the body. **No `X-AI-Tier` header
   or content-switch rule is required.**
   - **Constraint:** a DataScriptSet that calls `select("pg")` must list that Pool
     Group in its **`pool_group_refs`**, or Avi rejects it with HTTP 400.
3. **`set_reqvar`/`get_reqvar` and `add_header` confirmed** in `HTTP_REQ_DATA`
   (round-tripped a value) — these back the per-tier-budget `ai_tier` reqvar and
   any header tagging. (`avi.http` exposes `method`/`status`/`oauth_get_claim`
   etc.; note `get_method` does **not** exist — it's `method()`, and it is
   `nil` in `HTTP_REQ`, matching why the token script pcall-guards it.)

### Residual item to validate during implementation

- **Body larger than the 32 KB buffer.** The spike confirmed extraction for a
  small body; it did **not** confirm that an oversized request (long RAG / 128K
  context prompt) still returns the buffered **head** from `get_req_body` (where
  `model` sits at the JSON start) rather than nil/empty — the response-side
  `get_response_body` fails closed when the body exceeds the buffer because
  `usage` is at the *end*. Since `model` is at the *start*, head-buffering would
  suffice, but this must be tested. Mitigation if it fails: require clients to
  send `model` early (it already is) and, worst case, fall back to a `X-Model`
  header for requests that exceed the buffer.

---

## Limitations

- **Tier pools are baked at policy-reconcile time.** A tier's pool members are
  resolved when the `AIModelRoutePolicy` is translated, not when its backend's
  endpoints change. Editing an `EndpointSlice` behind a `Service` tier — even
  deleting and recreating it — does **not** push new members, and neither does
  annotating the policy. The only reliable way to force a re-resolve is to patch
  the policy **spec** (adding then removing a throwaway `modelTiers` key is
  enough). The failure mode is a **hang, not a 4xx**: the SE keeps sending to a
  stale member. Read the pool's real members through the console's
  `/api/aimetrics?tier=` rather than trusting the CR.
- **A tier that resolves to nothing is skipped, not failed.** If a tier's pool
  group ends up with no members, `ApplyModelRoutePolicy` logs a warning, drops
  the tier and carries on. A `modelTiers` entry pointing at a dropped tier
  therefore falls through to `defaultTier` — which is how a mis-set
  `defaultTier` silently sends every unmapped model to a dangling backend.
- **32 KB request-body buffer.** `avi.http.set_request_body_buffer_size` caps at
  32768 on this Avi build. `model` sits at the JSON start so the buffered head is
  enough; behaviour for a request body larger than 32 KB (does `get_req_body`
  return the head, or fail closed?) is not yet validated.
- **Per-tier budget enforcement runs in `HTTP_REQ_DATA`.** The classic
  `HTTP_REQ`-phase token check cannot see `ai_tier` (set later), so tier-keyed
  limits use `groupHeader: "reqvar:ai_tier"` and enforce in the body phase. See
  [Composition](#composition-with-auth-and-token-budgets).
- **A `remote` tier names exactly one peer.** Its pool group has one member, so
  peer down means tier down. Naming a *set* of sites is designed but not built —
  see [ai-gateway-datacenter.md §6](ai-gateway-datacenter.md).
- **Provider-tier responses are not metered.** External providers answer
  chunked/streamed, which the metering DataScript cannot read. Remote-site tiers
  *are* metered normally — the peer is ours, and its response is buffered like
  any local one.
- **Eventually-consistent counters / soft RPS** — inherited from the token policy.

---

## Roadmap

| Phase | Feature | Status |
|---|---|---|
| 2.x | `AIModelRoutePolicy` — model→tier routing via request-body DataScript + `avi.poolgroup.select` | ✅ Done — verified end-to-end on a live cluster |
| 2.x | Group-based tier entitlement (reuse verified `group` claim) | ✅ Done |
| 2.x | Per-tier token budgets (`groupHeader: "reqvar:ai_tier"`, enforced in `HTTP_REQ_DATA`) | ✅ Done |
| 2.x | Service (non-InferencePool) tier backends — endpoints, so a selectorless Service + hand-written EndpointSlice fronts out-of-cluster serving infrastructure | ✅ Done |
| 2.x | Pod-label model **discovery** (`spec.discovery`) — a labelled model pod publishes its own alias into the model→tier table; deleting the pods un-registers it | ✅ Done — 2026-08-08 |
| 2.x | External-provider tiers (`tier.provider`) — route to an OpenAI-compatible API (Gemini) over SE egress; AKO authors an FQDN pool (backend TLS/SNI) + the DataScript rewrites path/Host and injects the key from a Secret; token metering skipped (chunked responses). **Verified live on Avi 31.2.1 — real Gemini answers through the governed front door, injection still 403.** | ✅ Done — 2026-08-09 |
| 2.x | **Remote-site tiers** (`tier.remote`) — a tier whose backend is a peer AI Gateway in another cluster, reached by FQDN over SE egress with a Host-bearing health monitor. **Verified cross-cluster 2026-08-23**: `qwen-antrea` on the `vks-ai-01` front door is served by the `k8s-antrea` cluster, proven by the server fingerprint. | ✅ Done — 2026-08-23 |
| 3 | A tier that names a **set** of sites, not one peer (`tiers[].sites[]` with priority/weight/drain) | Designed — [datacenter §6](ai-gateway-datacenter.md) |
| 3 | Re-resolve a tier's pool on endpoint change, without a spec patch (see Limitations) | Open bug |
| 3 | Budget-aware downgrade (over-budget premium → economy instead of 429) | Idea |
| 3 | LoRA-adapter-aware routing (route to pods with a matching adapter) | Idea |
| 3 | Weighted canary within a tier (split a model name across two versions) | Idea |

---

## Related docs

- [AI Gateway](ai-gateway.md) — OAuth/OIDC auth + token rate limiting (the
  claim source and DataScript patterns this design reuses)
- [Inference Extension](inference-extension.md) — metric-weighted load balancing
  within each tier's `InferencePool`
- [Inference Install Guide](inference-install.md) — cluster setup walkthrough
