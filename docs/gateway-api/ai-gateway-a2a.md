<!--
  A2A (Agent2Agent) is the third governance surface after inference and MCP.
  CRD, controller, translator, and demo manifests implemented on feature/ai-a2a-gateway.
  Drafted 2026-06-07. Updated 2026-06-18 to reflect implemented state.
  NOTE: unlike MCP (native in Avi 32.1.1), A2A has NO native Avi support — it is built from
  generic Avi primitives + DataScripts, closer to model-routing than to the MCP design.
-->

# AKO AI Gateway — A2A Gateway & Agent-to-Agent Routes

> **Status: Implemented (feature/ai-a2a-gateway).** The `AIA2ARoutePolicy` CRD, controller,
> DataScript generator, and translator are implemented and end-to-end verified on the demo
> cluster (orchestrator→200, security-agent send→403/get→200, rogue→403). This document
> covers the **A2A Gateway** — a dedicated Gateway API entry point for **Agent2Agent**
> (agent↔agent) traffic — and the `AIA2ARoutePolicy` CRD that governs it: shared-IdP
> authentication, **per-skill** authorization, task/context **session affinity**, and
> **push-notification egress control**. It completes the "govern the whole agent loop under
> one identity" thesis (inference → tools → agents) alongside [ai-gateway.md](ai-gateway.md),
> [model-routing.md](model-routing.md), and [ai-gateway-mcp.md](ai-gateway-mcp.md), and reuses
> their machinery (OAuth/OIDC object graph, request-body DataScript parsing, per-rule Pool
> Groups).
>
> **Key difference from MCP:** Avi 32.1.1 added *native* MCP features (session profile,
> MCP-aware persistence). **A2A has no native Avi support.** Everything here is built from
> generic Avi primitives + DataScripts — so A2A is architecturally closer to model routing
> than to the MCP design. Body-derived session affinity (Spike-A) is resolved: the DataScript
> stamps `X-A2A-Context` and Avi custom-header persistence keys on it (verified end-to-end).

---

## 1. Overview — the third leg of the agent loop

The AI Gateway governs an agent's calls to **models** (inference) and is being extended to
its calls to **tools** ([MCP](ai-gateway-mcp.md)). The remaining surface is agents calling
**other agents** — delegation and task hand-off between independent agentic systems. That is
**A2A (Agent2Agent)**, the open protocol Google donated to the Linux Foundation in 2025.

Three protocols, three surfaces — and our positioning is to govern all three under **one
verified identity**:

| Surface | Protocol | Governed by |
|---|---|---|
| agent ↔ model | OpenAI-style HTTP | [ai-gateway.md](ai-gateway.md) + [model-routing.md](model-routing.md) — ✅ done |
| agent ↔ tool | MCP (JSON-RPC + Streamable HTTP) | [ai-gateway-mcp.md](ai-gateway-mcp.md) — design |
| **agent ↔ agent** | **A2A (JSON-RPC over HTTPS)** | **this document** — design |

### What A2A looks like on the wire

A2A is, like MCP, **JSON-RPC 2.0 over HTTPS** — which is exactly why our SE-native model
fits it. The shape (per the A2A spec, which is still evolving):

- **Discovery via an Agent Card.** Each agent publishes a JSON **Agent Card** (served today at
  `/.well-known/agent.json`; the A2A spec is migrating the convention to `/.well-known/agent-card.json`)
  describing its `name`, `version`, `url`, `capabilities`
  (`streaming`, `pushNotifications`), **`skills[]`** (`id`, `name`, `description`, `tags`),
  input/output modes, and **`securitySchemes`** (OAuth2/OIDC, API key, …). The Agent Card is
  the A2A analog of MCP's `server.json` — the discovery + registry primitive (§11).
- **JSON-RPC methods** on a single endpoint: `message/send` (sync), `message/stream` (SSE),
  `tasks/get`, `tasks/cancel`, `tasks/resubscribe`, `tasks/pushNotificationConfig/set`.
- **Tasks are long-running and stateful.** A request opens or continues a **task** with a
  `taskId` and a `contextId`; the task has a lifecycle (`submitted → working → input-required
  → completed/failed/canceled`), carries messages and artifacts, and **must keep landing on
  the backend agent that holds its state**.
- **Auth is standard.** The Agent Card declares OAuth2/OIDC bearer auth, so the SE's existing
  OAuth resource-server validation applies directly — same IdP, same token, as the LLM and
  MCP gateways.

Two properties make A2A different from inference traffic and drive this design — and one of
them is *harder* than MCP:

- **Stateful, but keyed in the body.** MCP carries its session in the `Mcp-Session-Id`
  **header** (so Avi's native custom-header persistence pins it). A2A carries `taskId` /
  `contextId` in the **JSON-RPC body**. Header-based persistence does not see it — see §7,
  the make-or-break design problem.
- **Per-*skill* authorization.** Like MCP's per-tool model: two callers may both reach an
  agent, but only one may invoke a given **skill**. The unit of access control is the
  **skill/method inside the JSON-RPC body**, not the URL.

---

## 2. What is and isn't native

| Concern | MCP (32.1.1) | **A2A (this design)** |
|---|---|---|
| Auth at the LB | native OAuth resource-server | **reused** — same `AUTH_PROFILE_OAUTH` graph ([oauth_rest.go](../../ako-gateway-api/aigateway/oauth_rest.go)) |
| Session persistence | **native** MCP profile keyed on `Mcp-Session-Id` header | **not native** — DataScript extracts `contextId` from body, stamps `X-A2A-Context` header; Avi custom-header persistence keys on it (Spike-A passed — §7) |
| Per-unit authorization | DataScript on JSON-RPC `method`/tool | **reused** — DataScript on JSON-RPC `method`/skill (§6) |
| Streaming | SSE proxy (MCP `GET` channel) | SSE (`message/stream`) — same pass-through, same metering limit ([[streaming]]) |
| Push notifications | n/a | **A2A-specific** — async webhook callbacks → egress/SSRF governance (§8) |

**Honesty marker.** A2A is newer than MCP and the protocol is still moving (JSON-RPC core,
with REST/gRPC bindings added over time). Field names here (`taskId`, `contextId`, Agent Card
path) track the spec as of this draft and must be re-confirmed before they are hard-coded —
see §12.

---

## 3. The A2A Gateway

Like the MCP Gateway, the A2A Gateway is an ordinary Gateway API **`Gateway`** dedicated to
A2A traffic — its own HTTPS listener, its own Avi VS — kept **separate** from the LLM and MCP
gateways so the three scale, secure, and fail independently. A Gateway (or listener) is
designated A2A with an annotation:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: a2a-gateway
  namespace: inference
  annotations:
    ai.ako.vmware.com/a2a: "true"        # attach task/context persistence (§7) to the VS
spec:
  gatewayClassName: avi-lb
  listeners:
    - name: a2a-https
      protocol: HTTPS                     # OAuth/OIDC requires TLS termination
      port: 443
      tls: { mode: Terminate, certificateRefs: [ { name: a2a-tls } ] }
```

A2A routes attach as normal `HTTPRoute`s (one per fronted agent, e.g. `/a2a/<agent>`), and an
`AIA2ARoutePolicy` (§4) governs each. No new data-plane primitive is introduced; the A2A
specifics live in the DataScript + persistence config AKO attaches.

Why a separate Gateway: different protocol shape (stateful tasks + SSE + push callbacks),
blast-radius isolation, and an independent auth/authorization surface — the same rationale as
the MCP Gateway.

---

## 4. CRD — `AIA2ARoutePolicy`

Shaped like the other AI Gateway policies (`targetRef` → an `HTTPRoute`). It carries four
concerns: **which auth to inherit**, **task/context session affinity**, **per-skill
authorization**, and **push-notification egress control**.

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIA2ARoutePolicy
metadata:
  name: agents-policy
  namespace: inference
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: a2a-route                       # the /a2a/<agent> route on the A2A Gateway

  # ── Auth: tie A2A to the SAME IdP as the LLM/MCP gateways ─────────────────────
  authRef:
    name: llm-auth                        # an AIGatewayAuthPolicy in this namespace (§5)

  # ── Task/context session affinity (the hard part — §7) ───────────────────────
  session:
    keySource: body                       # "body" (extract from JSON-RPC) | "header"
    bodyField: contextId                  # JSON-RPC field that identifies the task/session
    headerName: X-A2A-Context             # the synthetic header the DataScript stamps
    timeout: 30m

  # ── Per-skill authorization (the AKO-programmed layer — §6) ──────────────────
  skillAccess:
    roleClaim: role                       # verified JWT claim carrying the caller's role
    rules:
      - role: orchestrator                # an orchestrator agent may delegate anything
        allow: ["*"]
      - role: analyst                      # analysts: read/analyze skills only
        allow: ["search.*", "report.generate", "data.read"]
      - role: guest
        allow: ["search.query"]

  # ── Push-notification egress control (A2A-specific — §8) ─────────────────────
  pushNotifications:
    mode: AllowList                       # AllowList (default-deny) | Deny | Allow
    allowedHosts:                         # webhook hosts the gateway will permit
      - "callbacks.internal"
      - "*.trusted-partner.example"

  onUnauthorized:
    type: Reject                          # Reject (default) | Log
    statusCode: 403
```

### Field reference

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.targetRef.*` | PolicyTargetRef | yes | The A2A `HTTPRoute` (or `Gateway`). Same shape as the other AI Gateway policies. |
| `spec.authRef.name` | string | no | An `AIGatewayAuthPolicy` whose IdP this A2A route shares (§5). Omit to run unauthenticated (not recommended; §13 fails closed). |
| `spec.session.keySource` | string | no | `body` (default — extract the task/context id from the JSON-RPC body) or `header`. |
| `spec.session.bodyField` | string | no | JSON-RPC field naming the session (e.g. `contextId` or `taskId`). Used when `keySource: body`. |
| `spec.session.headerName` | string | no | Synthetic header the DataScript stamps with the extracted id, on which Avi persistence then keys. Default `X-A2A-Context`. |
| `spec.session.timeout` | duration | no | Idle task/session lifetime. Default `30m`. |
| `spec.skillAccess.roleClaim` | string | no | Verified JWT claim carrying the caller's role. Default `role`. |
| `spec.skillAccess.rules[].role` | string | yes\* | A role value (required when `skillAccess` is present). |
| `spec.skillAccess.rules[].allow` | []string | yes\* | Skill ids / JSON-RPC methods this role may invoke. `"*"` = all; trailing `"*"` = prefix glob (e.g. `"search.*"`), matching the glob semantics in [model-routing.md](model-routing.md). |
| `spec.pushNotifications.mode` | string | no | `AllowList` (default-deny except `allowedHosts`), `Deny` (block all push config), or `Allow` (no egress control — not recommended). |
| `spec.pushNotifications.allowedHosts` | []string | no | Webhook hosts permitted in `tasks/pushNotificationConfig/set` (exact or trailing-`*` glob). |
| `spec.onUnauthorized.type` | string | no | `Reject` (default) or `Log`. |
| `spec.onUnauthorized.statusCode` | int | no | HTTP status on reject. Default `403`. |

Go types, deepcopy, informer, `PolicyStore` entry, and validation mirror
[`modelroute_types.go`](../../ako-gateway-api/aigateway/modelroute_types.go) /
[`controller.go`](../../ako-gateway-api/aigateway/controller.go) one-for-one (see §13).

---

## 5. Auth model — same IdP, separate enforcement

Identical to the MCP design ([ai-gateway-mcp.md §5](ai-gateway-mcp.md)): `authRef` points at
an existing `AIGatewayAuthPolicy`; AKO **reuses** that policy's issuer `Pool` +
`AUTH_PROFILE_OAUTH` (one IdP, one login, one JWKS path) and emits **only** a separate
`SSO_TYPE_OAUTH` policy for the A2A VS (`<policy>-a2a-sso`). A token minted for the agent is
valid at the LLM, MCP, and A2A gateways; enforcement is per-VS so A2A's skill-authorization
rules never perturb the others. The same **reference-counting** lifecycle applies — the
shared OAuth graph is not torn down while any LLM/MCP/A2A policy still references it (§13).

> **jwtQuery mode (implemented).** When the route's `authRef` policy is configured with
> `jwtQuery` mode (JWT delivered in `?jwt=` query param rather than `Authorization:` header),
> `ApplyA2ARoutePolicy` detects this from the policy spec and switches the DataScript claim
> accessor from `avi.http.oauth_get_claim` (which returns nil under jwtQuery and denied every
> agent with 403) to `jwtQueryClaimHelper`, which decodes the query-param JWT directly. The
> mode is derived automatically — no extra field in `AIA2ARoutePolicy` is required.

> **Identity propagation across agent hops (deferred).** When orchestrator agent **A** calls
> worker agent **B** through the gateway, the gateway validates A's token and authorizes A's
> role. Multi-hop chains (A→B→C) where B should act **on behalf of** A raise delegation
> questions (OAuth token exchange, RFC 8693). **v1 treats each hop at face value** — the
> gateway governs the *immediate* caller's verified identity on each leg. On-behalf-of
> delegation chains are a §15 roadmap item, not v1.

---

## 6. Per-skill authorization (reused from model routing / MCP)

OAuth proves *who* the caller is; the *skill* decision needs the JSON-RPC body — the same
problem `AIModelRoutePolicy` solved for `model` and the MCP design solved for the tool name.
Mechanism reused verbatim from
[`modelroute_datascript.go`](../../ako-gateway-api/aigateway/modelroute_datascript.go).

An A2A `message/send` body (simplified, per the evolving spec):

```json
POST /a2a/research-agent
{ "jsonrpc": "2.0", "id": 11, "method": "message/send",
  "params": { "message": { "role": "user", "parts": [ ... ] },
              "contextId": "ctx-abc",
              "metadata": { "skill": "report.generate" } } }
```

```lua
-- Event 1 — HTTP_REQ: enable request-body buffering (32 KB cap; method/skill/contextId
-- sit at the JSON head, so the buffered head suffices — same as model routing).
pcall(function() avi.http.set_request_body_buffer_size(32768) end)
```

```lua
-- Event 2 — HTTP_REQ_DATA: read body, extract method + skill, authorize by role.
local ok, body = pcall(function() return avi.http.get_req_body(32768) end)
body = (ok and body) or ""

local method = extract_json_string(body, "method")            -- generated helper
local skill  = extract_nested_string(body, "metadata", "skill") -- generated helper

-- Only message/send|stream is per-skill gated; tasks/get, tasks/cancel, etc. are
-- task-lifecycle plumbing and pass the OAuth check alone.
if method == "message/send" or method == "message/stream" then
  local role = jwt_claim(ROLE_CLAIM)                           -- shared AI Gateway helper
  if not allowed(role, skill) then                             -- baked-in ALLOW table + globs
    avi.http.response(403, {["Content-Type"]="application/json"},
      '{"jsonrpc":"2.0","error":{"code":-32001,"message":"skill_not_authorized"}}')
    return
  end
end
```

`extract_json_string`, `extract_nested_string`, `allowed`, the baked-in `ALLOW` table, and
`ROLE_CLAIM` are generated from the CR exactly like `MODEL_TIERS` / `TIER_PG`. `jwt_claim` is
the **same** sandbox-safe accessor the other scripts use.

> **Skill field path (verified — Spike-C passed).** The skill is carried at
> `params.metadata.skill` in the JSON-RPC body. The DataScript extracts it via a
> quote-scan over this known path. When the skill cannot be extracted (oversized or
> garbled body), `message/*` is denied and task-lifecycle methods (`tasks/get`,
> `tasks/cancel`) pass unaffected.

---

## 7. Statefulness — task/context affinity (implemented)

A2A tasks are stateful and must keep landing on the agent backend that holds the task.
Unlike MCP, which carries its session in the `Mcp-Session-Id` **header**, A2A carries
`taskId`/`contextId` in the **JSON-RPC body** — so Avi's native custom-header persistence
cannot see it directly.

**Spike-A passed.** Avi custom-header persistence *does* observe headers added by a
`HTTP_REQ_DATA` DataScript. The implemented approach:

1. In `HTTP_REQ_DATA`, the DataScript extracts `contextId` from the JSON-RPC body and calls
   `avi.http.add_header("X-A2A-Context", contextId)`.
2. An Avi **custom-header persistence profile** keys on `X-A2A-Context`.
3. Subsequent requests with the same `contextId` are pinned to the same backend agent.

This is verified end-to-end on the demo cluster with a scaled-out pool. Stateless A2A calls
(`message/send` that completes in one shot) work with or without affinity — pinning only
matters for multi-turn tasks with long-running state.

---

## 8. Push-notification egress control (A2A-specific)

A2A lets a client register a **webhook** (`tasks/pushNotificationConfig/set`) so the agent
**POSTs task updates back** to a client-provided URL asynchronously. That is an **outbound
call the gateway must govern** — an unconstrained webhook target is a classic **SSRF /
data-exfiltration** vector (an attacker registers an internal URL and the agent posts task
data to it).

`AIA2ARoutePolicy.spec.pushNotifications` governs it at the same `HTTP_REQ_DATA` DataScript:

- On `method == "tasks/pushNotificationConfig/set"`, extract the webhook URL from the body
  and check its host against `allowedHosts` (exact / trailing-`*` glob).
- `mode: AllowList` (default) → reject any host not listed; `Deny` → reject all push config;
  `Allow` → no check (explicit opt-out, not recommended).
- Reject shape: `{"jsonrpc":"2.0","error":{"code":-32002,"message":"webhook_host_not_allowed"}}`.

This only validates **registration** the gateway sees. As with the MCP egress story, making
it *inescapable* (the agent cannot post anywhere except via approved hosts) needs the security
fabric — Kubernetes **NetworkPolicy** / **NSX-vDefend DFW** forcing agent egress through
governed paths. The policy is the allow-list; the fabric makes it binding (the same vDefend
tie-in as [ai-gateway-mcp.md §9.6](ai-gateway-mcp.md)).

---

## 9. Composition with the existing policies

`AIA2ARoutePolicy` is additive and orthogonal. On the A2A child VS the event order mirrors
the LLM/MCP VSes:

1. **OAuth/OIDC** — the A2A VS's own `SSO_TYPE_OAUTH` (§5); claims become readable.
2. **Skill authorization + push-egress check** — the `HTTP_REQ` → `HTTP_REQ_DATA` scripts
   (§6/§8), indexed after auth so the role claim is populated.
3. **Session affinity** — header/member selection (§7) once `contextId` is extracted.
4. **Call budgets (optional, reuse [`AITokenRateLimitPolicy`](ai-gateway.md))** — an
   `AITokenRateLimitPolicy` on the same A2A route puts per-consumer/per-role **call-count**
   budgets on agent invocations (A2A has no "tokens"; the natural limit is request-rate /
   per-skill call count, which `requestRateLimit` + the counter keys already support).

The result: **one verified identity** governs an agent's *entire* loop — model calls, tool
calls, **and agent-to-agent calls** — authenticated by the same IdP and budget-governed by
the same policy family.

---

## 10. UI integration (specification — UI lives in an external repo)

A new **"A2A Gateways"** section beside the LLM/MCP views, sharing the same OIDC login:

| UI element | Reads / writes | Notes |
|---|---|---|
| **A2A Gateway list** | `Gateway`s annotated `ai.ako.vmware.com/a2a: "true"` + their `/a2a/*` routes | Create/inspect. |
| **Auth binding** | `AIA2ARoutePolicy.spec.authRef` | Dropdown of LLM auth policies (shared IdP). |
| **Skill-access matrix** | `spec.skillAccess.rules` | A role × skill grid, pre-filled from the agent's **Agent Card `skills[]`** (§11) — the key UX win. |
| **Push-notification allow-list** | `spec.pushNotifications.allowedHosts` | Webhook host governance. |
| **Session settings** | `spec.session` | Key source/field + timeout. |
| **Call counters** | reuse `GET /v1/admin/counters` | If an `AITokenRateLimitPolicy` is attached, show per-role agent-call volume + `Log`-mode denials. |

---

## 11. Agent Card registry & onboarding — the approved allow-list

Operators browse a **catalog of approved agents** and onboard with a click — the direct A2A
analog of the MCP registry ([ai-gateway-mcp.md §9](ai-gateway-mcp.md)). The catalog entry is
the agent's **Agent Card** (served at `/.well-known/agent.json`). This catalog is implemented
today as the `agent-registry` ConfigMap + console view + `/.well-known/agents` federation —
see [ai-gateway-agent-registry.md](ai-gateway-agent-registry.md).

| Agent Card field | Gateway relevance |
|---|---|
| `name`, `version`, `description` | provenance + browse view |
| **`url`** | the agent endpoint → an Avi **Pool** backend (`ExternalName` Service / `Endpoints`) |
| **`skills[]`** (`id`, `name`, `tags`) | **pre-fills the role × skill access matrix** — operators check boxes against the real skill list (§10) |
| `securitySchemes` | confirms the agent speaks the shared OAuth/OIDC IdP (or flags a mismatch) |
| `capabilities` (`streaming`, `pushNotifications`) | drives which `session`/`pushNotifications` controls to surface |

On **Add**, the console writes (in-cluster, same authed path as today): a backend
Pool/Service, an `HTTPRoute` (`/a2a/<agent>`) on the A2A Gateway, and an `AIA2ARoutePolicy`
(`authRef`, `session`, `skillAccess`, `pushNotifications`) — all stamped with catalog
provenance annotations (`ai.ako.vmware.com/a2a-catalog-name|version|registry`), mirroring the
MCP onboarding so a later reconciling controller can adopt them.

**Approved-registry governance** is identical in spirit to MCP §9.6: the gateway materializes
a route **only** for an approved Agent Card, so an unapproved agent has **no path through the
gateway** (structural default-deny); true egress lockdown needs NetworkPolicy / vDefend DFW.
This is the **single auditable place that decides which agents exist in the fleet** — the
north-star governance object, now spanning tools *and* agents.

---

## 12. Feasibility — spike results

All make-or-break spikes passed. The implementation is verified end-to-end on the demo
cluster (orchestrator→200, security-agent send→403/get→200, rogue→403).

| # | Question | Result |
|---|---|---|
| **A ✅** | Does Avi **custom-header persistence** observe a header added by a `HTTP_REQ_DATA` DataScript? | **Passed.** DataScript stamps `X-A2A-Context` in `HTTP_REQ_DATA`; Avi custom-header persistence keys on it. Multi-turn tasks with the same `contextId` land on the same backend. |
| **B (not needed)** | DataScript-driven consistent-hash member selection as fallback for A. | Spike-A passed; Spike-B not required. |
| **C ✅** | A2A body readable in `HTTP_REQ_DATA`; `method` + skill path extract via sandbox-safe scan. | **Passed.** `message/send` denied for unauthorized roles; `tasks/get` passes for all authenticated callers; body > 32 KB fails closed on `message/*`. |
| **D ✅** | Push-notification egress check rejects unlisted webhook hosts. | **Passed.** `tasks/pushNotificationConfig/set` to an unlisted host returns the JSON-RPC error; listed hosts pass. |
| **E ✅** | Shared-IdP reference-counted lifecycle (§5). | **Passed.** Token accepted at LLM+MCP+A2A VSes; deleting A2A policy leaves others intact. |
| **F** | SSE `message/stream` proxies through the VS alongside affinity. | Not yet validated in demo — open for follow-up. |

> **Note on jwtQuery mode:** when the `authRef` policy uses `jwtQuery` (JWT in `?jwt=`),
> `avi.http.oauth_get_claim` returns nil and denied every agent (403). The translator now
> detects this mode from the policy spec and switches to `jwtQueryClaimHelper` automatically.
> See §5 for details.

---

## 13. Implementation

Implemented across three commits on `feature/ai-a2a-gateway` (`72f6fd2a` → `80d36e07` → `5fbb575e`),
mirroring how `AIModelRoutePolicy` was wired (`c05fc5bc` → `a2e7b995` → `404775d4`).

1. **CRD + Go types** ✅ — `helm/ako/crds/ai.ako.vmware.com_aia2aroutepolicies.yaml` and
   `AIA2ARoutePolicy` types/deepcopy/validation in
   [`ako-gateway-api/aigateway/a2aroute_types.go`](../../ako-gateway-api/aigateway/a2aroute_types.go).
2. **RBAC** ✅ — `aia2aroutepolicies` (+ `/status`) added to
   [`helm/ako/templates/clusterrole.yaml`](../../helm/ako/templates/clusterrole.yaml) in the
   same commit (lesson from the model-routing miss at `404775d4`).
3. **Informer + handlers** ✅ — dynamic informer + `SetupA2ARoutePolicyEventHandlers` in
   [`gateway_crd_controller.go`](../../ako-gateway-api/k8s/gateway_crd_controller.go), started
   in [`gateway_controller.go`](../../ako-gateway-api/k8s/gateway_controller.go), plus a
   `PolicyStore` entry (`GetA2ARoutePoliciesForRoute`) in
   [`ako-gateway-api/aigateway/a2aroute_controller.go`](../../ako-gateway-api/aigateway/a2aroute_controller.go).
4. **Translator** ✅ — `ApplyA2ARoutePolicy(...)` in
   [`ako-gateway-api/nodes/avi_a2a_route.go`](../../ako-gateway-api/nodes/avi_a2a_route.go),
   wired into the per-child-VS loop in
   [`avi_model_l7_translator.go`](../../ako-gateway-api/nodes/avi_model_l7_translator.go)
   immediately after the MCP block. Resolves + ref-counts the OAuth graph, emits the A2A SSO
   policy, stamps `X-A2A-Context` for session affinity, and generates the skill-authz +
   push-egress DataScript set. jwtQuery mode is detected from the `authRef` policy spec and
   switches the claim accessor to `jwtQueryClaimHelper` (see §5).
5. **DataScript generator** ✅ — `GenerateA2AScripts` in
   [`ako-gateway-api/aigateway/a2aroute_datascript.go`](../../ako-gateway-api/aigateway/a2aroute_datascript.go),
   mirroring `GenerateModelRouteScripts`.
6. **Demo manifests** ✅ — complete A2A demo in
   [`docs/gateway-api/examples/ai-gateway-demo/`](../examples/ai-gateway-demo/) including
   `a2a-gateway.yaml`, `a2a-gateway-policies.yaml`, `mock-a2a-agent.yaml`, `setup-a2a.sh`,
   and extended `demo.sh`.

> **Status semantics.** `AIA2ARoutePolicy.status.conditions` reports an unresolved `authRef`
> and unknown skills/roles in `skillAccess`, the same validation shape as `AIModelRoutePolicy`.

---

## 14. Failure modes (graceful degradation)

| Condition | Behaviour |
|---|---|
| `AIA2ARoutePolicy` absent / unreconciled | A2A route serves as a plain HTTPRoute — no affinity, no skill-authz, no egress control. Reachable but ungoverned (benign, like a missing `AIModelRoutePolicy`). |
| `authRef` unresolved | A2A VS has no SSO policy → **fail closed** (401). Status flags the dangling ref. |
| Skill unextractable (oversized/garbled body) | `message/*` denied (fail closed); task-lifecycle methods unaffected. |
| `contextId` absent or unextractable from body | Affinity header not stamped; request routes without pinning. Task may land on a different backend (degraded for stateful tasks). Fail-closed on skill-authz still applies. |
| Push webhook host not allow-listed | `pushNotificationConfig/set` rejected; other methods unaffected. |
| Shared OAuth graph deleted under A2A | Prevented by ref-counting (§5/§13). |

---

## 15. Roadmap

| Phase | Feature | Status |
|---|---|---|
| 3/4 | `AIA2ARoutePolicy` CRD + A2A Gateway annotation; shared-IdP/separate-SSO auth | ✅ Implemented (`72f6fd2a`) |
| 3/4 | Per-skill authorization DataScript | ✅ Implemented (`72f6fd2a`) |
| 3/4 | Task/context session affinity via `X-A2A-Context` synthetic header | ✅ Implemented + verified (`80d36e07`) |
| 3/4 | Push-notification egress allow-list | ✅ Implemented (`72f6fd2a`) |
| 3/4 | jwtQuery mode claim accessor fix | ✅ Implemented (`5fbb575e`) |
| 3/4 | Demo manifests + `setup-a2a.sh` | ✅ Implemented (`5fbb575e`) |
| 3/4 | Per-skill call budgets via `AITokenRateLimitPolicy` | Planned (reuse — no new work) |
| 3/4 | SSE `message/stream` end-to-end affinity validation | Open (Spike-F, see §12) |
| 3.x | UI "A2A Gateways" section (external repo) | Spec (§10) |
| 3.x | Agent Card registry onboarding + approved allow-list (default-deny) | Spec (§11) |
| 4 | On-behalf-of delegation across agent hops (OAuth token exchange, RFC 8693) | Idea (§5) |
| 4 | `AgentServer`/registry CRD — reconciled catalog; version/withdrawal tracking | Idea |
| 4 | Egress lockdown via NetworkPolicy / NSX-vDefend DFW; cross-site A2A via GSLB | Idea |

---

## 16. Related docs

- [AI Gateway](ai-gateway.md) — the IdP this design shares and the DataScript/claim patterns it reuses
- [Model-Based Routing](model-routing.md) — the body-inspection + `oauth_get_claim` mechanism reused for skill authorization
- [MCP Gateway](ai-gateway-mcp.md) — the sibling design; shared auth model, registry/allow-list, and UI patterns. A2A is the agent↔agent counterpart to MCP's agent↔tool.

## Sources (A2A protocol)

- A2A specification & Agent Card schema — the A2A Project (Linux Foundation). *Confirm field
  names (`taskId`/`contextId`, Agent Card path, skill metadata) against the current spec
  revision before implementation — §12.*
