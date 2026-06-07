<!--
  DESIGN + EARLY IMPLEMENTATION. Technical architecture is repo-appropriate (cf. model-routing.md).
  Realizes the "Phase 3 AIMCPPolicy" forward-reference in ai-gateway.md.
  The 32.1.1 MCP object model is VERIFIED on a live controller (§2, §10 Spike-1).
  Drafted 2026-06-06; object model verified + foundation landed 2026-06-07.
-->

# AKO AI Gateway — MCP Gateway & MCP-Specific Routes

> **Status: Phase 3 — design complete, object model verified, foundation landed.** This
> document specifies an **MCP Gateway** — a dedicated Gateway API entry point for **Model
> Context Protocol** (agent↔tool) traffic — and a new `AIMCPRoutePolicy` CRD that attaches
> **MCP-specific routes** to it. It gives MCP traffic **its own auth enforcement tied to the
> same IdP as the LLM gateway** (one login, separate per-tool authorization), built on the
> **native MCP features in Avi Load Balancer 32.1.1** (verified live — §2). It composes with
> the existing per-cluster AI Gateway ([ai-gateway.md](ai-gateway.md)) and model-based routing
> ([model-routing.md](model-routing.md)), reusing their machinery (OAuth/OIDC object graph,
> DataScript body parsing).
>
> **Built:** the CRD, `AIMCPRoutePolicy` types/validation, the tool-authz DataScript
> generator, and RBAC (+ guardrail test) — see §11. **Remaining:** controller wiring and the
> `ApplyMCPRoutePolicy` translator (§11 steps 4–6), plus the spikes in §10 (2–6).

---

## 1. Overview — the second half of the agent loop

The AI Gateway today governs **inference** traffic: an agent (or app) POSTs an
OpenAI-style request to an LLM endpoint, and the gateway authenticates it, meters tokens,
and routes it to a model tier. But an agent run is a **loop** — the model decides to *call
a tool*, and that tool call goes to an **MCP server**, not the LLM. Today that half of the
loop is ungoverned: there is no auth, no per-tool authorization, and no session control on
the agent↔tool path.

**MCP** (Model Context Protocol) is the JSON-RPC 2.0 protocol that carries those tool
calls. Modern MCP uses the **Streamable HTTP** transport: a single endpoint (conventionally
`/mcp`) that takes a `POST` for each JSON-RPC request and serves a `GET`/SSE stream back,
with a session established on `initialize` and carried thereafter in the **`Mcp-Session-Id`**
request header. Two properties make MCP different from inference traffic and drive this
design:

- **It is stateful.** A session that lands on a different backend MCP server mid-run loses
  its context. MCP needs **session persistence**, which inference (stateless per request)
  does not.
- **Authorization is per *tool*, not per *route*.** Two callers may both reach `/mcp`, but
  one may invoke `filesystem.write` and the other only `search.query`. The unit of access
  control is the **tool/method inside the JSON-RPC body**, not the URL.

This design adds an MCP Gateway that handles both, while **sharing the LLM gateway's
identity** so an operator configures one IdP and gets governed inference *and* tool traffic
under the same login.

> **Strategy note (supersedes a now-stale claim).** The internal strategy memo
> ([ai-gateway-multicluster-strategy.md](ai-gateway-multicluster-strategy.md) §4.3, drafted
> 2026-06-06) lists native **MCP** among capabilities "Avi SE does not do natively." **Avi
> Load Balancer 32.1.1 (GA 2026-05-12) closed that gap** — see §2. This document is the
> design that takes advantage of it. The strategy memo's §4.3 should be updated separately.

---

## 2. What Avi 32.1.1 gives us natively

Avi Load Balancer **32.1.1** introduced first-class MCP load-balancing. The object names
below are **verified against a live 32.1.1 controller** (read-only probe,
[hack/spikes/mcp-probe.sh](../../hack/spikes/mcp-probe.sh)):

| 32.1.1 native feature | What it does | Verified Avi mechanism AKO reuses |
|---|---|---|
| **MCP application profile** | Marks a VS as MCP-aware (websockets, HTTP/2). | An `APPLICATION_PROFILE_TYPE_HTTP` profile with the new field **`app_service_type: APP_SERVICE_TYPE_HTTP_MCP`** — shipped as the system profile **`System-Secure-HTTP-MCP`**. *Not* a new profile type. |
| **MCP session persistence** | Pins a stateful MCP session to the same backend pool+server across a scaled-out fleet, with full session lifecycle. | The **built-in DataScriptSet `System-Standard-MCP`** — not a persistence profile. It keys on the `Mcp-Session-Id` header and uses VS persistence tables: `avi.vs.table_lookup("pool_table"/"server_table", id, 600)` + `avi.pool.select` on HTTP_REQ; `avi.vs.table_insert` on HTTP_RESP (create-on-response); `avi.vs.table_remove` on a 2xx `DELETE` (teardown). |
| **OAuth 2.0 authorization at the LB** | Enforces the OAuth 2.x resource-server check *before* traffic reaches the MCP server — the MCP spec's auth model. | The **existing** `AUTH_PROFILE_OAUTH` + `SSO_TYPE_OAUTH` graph the AI Gateway already builds ([oauth_rest.go](../../ako-gateway-api/aigateway/oauth_rest.go)). |

**Per-tool authorization is *not* native — and that is AKO's value-add.** The probe confirmed
32.1.1 has **no** tool-authorization object or field (`toolauthprofile`, `authorizationpolicy`
and friends all 404). The release notes' "JWT authorization for MCP" is **caller
authentication** (OAuth/JWKS), not per-tool authorization. So gating *which role may call
which tool* is done by an AKO-generated DataScript (§6) — there is no native alternative to
fall back to.

**Out of scope: Avi-as-MCP-Server.** 32.1.1 *also* ships an MCP **Server** that exposes
Avi's own LB/WAF operations to agents (so an agent can drive Avi). That is Avi being a tool
*provider*; this document is about Avi being the **gateway in front of someone else's** MCP
tool servers. The two are unrelated and the former is not used here.

---

## 3. The MCP Gateway

The MCP Gateway is an ordinary Gateway API **`Gateway`** dedicated to MCP traffic, with its
own HTTPS listener and therefore its own Avi VS — kept **separate from the LLM gateway** so
the two scale, secure, and fail independently (the same separation rationale as the
cross-site forwarder SE in [ai-gateway-multisite.md](ai-gateway-multisite.md) §4).

A Gateway (or a specific listener) is **designated as MCP** with an annotation. AKO then
configures the resulting VS with Avi's **built-in** MCP objects (verified §2): it sets the
VS `application_profile_ref` to **`System-Secure-HTTP-MCP`** and attaches the
**`System-Standard-MCP`** DataScriptSet for `Mcp-Session-Id` session persistence — AKO
*reuses* these system objects rather than generating session logic of its own:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: mcp-gateway
  namespace: inference
  annotations:
    ai.ako.vmware.com/mcp: "true"        # enable MCP profile + Mcp-Session-Id persistence
spec:
  gatewayClassName: avi-lb
  listeners:
    - name: mcp-https
      protocol: HTTPS                     # OAuth/OIDC requires TLS termination (as for LLM)
      port: 443
      tls: { mode: Terminate, certificateRefs: [ { name: mcp-tls } ] }
```

MCP routes are then attached to this Gateway as normal `HTTPRoute`s matching the MCP path
(e.g. `/mcp`), and an `AIMCPRoutePolicy` (§4) governs each route. Because the MCP Gateway is
just a Gateway, **everything else AKO already does for a Gateway applies** — TLS, VS
placement, status — and no new data-plane primitive is introduced.

Why a separate Gateway rather than reusing the LLM route:

- **Different protocol shape** — MCP is JSON-RPC + SSE on one endpoint with sticky sessions;
  inference is stateless request/response. They want different persistence and timeouts.
- **Blast-radius isolation** — a flood of tool calls cannot starve the inference VS.
- **Independent auth surface** — the MCP VS gets its **own** SSO policy (§5), so MCP-specific
  authorization rules never perturb the LLM route's enforcement.

---

## 4. CRD — `AIMCPRoutePolicy`

`AIMCPRoutePolicy` is a policy-attachment CRD shaped exactly like the other three AI Gateway
policies (`targetRef` → an `HTTPRoute`), realizing the "Phase 3 `AIMCPPolicy`" forward
reference in [ai-gateway.md](ai-gateway.md#overview). It carries three concerns: **which
auth to inherit**, **session persistence**, and **per-role tool authorization**.

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIMCPRoutePolicy
metadata:
  name: tools-policy
  namespace: inference
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: mcp-route                       # the /mcp route on the MCP Gateway

  # ── Auth: tie MCP to the SAME IdP as the LLM gateway ──────────────────────────
  # Reference an existing AIGatewayAuthPolicy. AKO reuses that policy's issuer Pool
  # + OAuth AuthProfile (same issuer/JWKS, same login), and emits a SEPARATE SSO
  # policy for this MCP VS. One IdP, one token, independent enforcement.
  authRef:
    name: llm-auth                        # an AIGatewayAuthPolicy in this namespace

  # ── Session persistence (native, Avi 32.1.1) ─────────────────────────────────
  session:
    header: Mcp-Session-Id                # default; the MCP session header
    timeout: 30m                          # idle session lifetime

  # ── Per-role tool authorization (the AKO-programmed layer) ───────────────────
  # roleClaim names the verified JWT claim carrying the caller's job role.
  toolAccess:
    roleClaim: role                       # default "role"
    rules:
      - role: operator                    # operators get the full toolbox
        allow: ["*"]
      - role: app-owner                   # app owners: read-only tools only
        allow: ["search.query", "docs.read", "metrics.get"]
      - role: guest
        allow: ["search.query"]

  # What to do when a caller invokes a tool their role may not use.
  onUnauthorized:
    type: Reject                          # Reject (default) | Log
    statusCode: 403
```

### Field reference

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.targetRef.*` | PolicyTargetRef | yes | The MCP `HTTPRoute` (or `Gateway`). Same shape as the other AI Gateway policies. |
| `spec.authRef.name` | string | no | An `AIGatewayAuthPolicy` in the same namespace whose IdP this MCP route shares (§5). Omit to run the MCP route unauthenticated (not recommended). |
| `spec.session.header` | string | no | Persistence header. Default `Mcp-Session-Id`. |
| `spec.session.timeout` | duration | no | Idle session lifetime. Default `30m`. |
| `spec.toolAccess.roleClaim` | string | no | Verified JWT claim carrying the caller's role. Default `role`. |
| `spec.toolAccess.rules[].role` | string | yes\* | A role value (required when `toolAccess` is present). |
| `spec.toolAccess.rules[].allow` | []string | yes\* | Tool names / JSON-RPC methods this role may invoke. A single `"*"` allows all; a trailing `"*"` is a prefix glob (e.g. `"docs.*"`), matching `modelTiers` glob semantics in [model-routing.md](model-routing.md). |
| `spec.onUnauthorized.type` | string | no | `Reject` (default) or `Log` (count but allow). |
| `spec.onUnauthorized.statusCode` | int | no | HTTP status on reject. Default `403`. |

The Go types, deepcopy, informer, `PolicyStore` entry, and validation mirror
[`modelroute_types.go`](../../ako-gateway-api/aigateway/modelroute_types.go) /
[`controller.go`](../../ako-gateway-api/aigateway/controller.go) one-for-one (see §11).

---

## 5. Auth model — same IdP, separate enforcement

The requirement is "**its own auth, tied to the same auth as the LLM**." Concretely: the
MCP caller and the LLM caller log in to the **same** OIDC issuer and carry the **same**
token, but the MCP VS enforces it **independently** so its tool-authorization rules are
isolated. AKO achieves this by **reusing the LLM auth's expensive object graph and adding
only a thin per-VS SSO policy**:

```
AIGatewayAuthPolicy "llm-auth"            AIMCPRoutePolicy "tools-policy"
   (targets llm-route)                       (targets mcp-route, authRef: llm-auth)
        │                                              │
        │ AKO builds once (oauth_rest.go):             │ AKO resolves authRef and REUSES:
        ▼                                              ▼
   issuer Pool  ──────────────────────────────────► (same Pool, JWKS endpoint)
   AUTH_PROFILE_OAUTH ───────────────────────────► (same AuthProfile: issuer/JWKS/aud)
   SSO_TYPE_OAUTH "llm-auth-oauth-sso"            SSO_TYPE_OAUTH "tools-policy-mcp-sso"
        │  on LLM child VS                              │  on MCP child VS  ← the ONLY new object
        ▼                                              ▼
   LLM VS enforces OAuth                          MCP VS enforces OAuth (same IdP)
```

- **What is shared:** the **issuer `Pool`** (the SE's path to the JWKS endpoint) and the
  **`AUTH_PROFILE_OAUTH`** (issuer, JWKS URI, audiences). These are exactly what
  [`EnsureIssuerPool`](../../ako-gateway-api/aigateway/oauth_rest.go) and
  [`EnsureOAuthAuthProfile`](../../ako-gateway-api/aigateway/oauth_rest.go) build today.
  Sharing them means **one IdP registration, one JWKS fetch path, one login** — a token
  minted for the agent is valid at both gateways.
- **What is separate:** the **SSO policy** is per-VS. AKO emits a distinct
  `SSO_TYPE_OAUTH` policy for the MCP VS (named `<policy>-mcp-sso`), built the same way as
  [`EnsureOAuthSSOPolicy`](../../ako-gateway-api/aigateway/oauth_rest.go) but with
  MCP-appropriate authentication rules (e.g. its own `SKIP_AUTHENTICATION` carve-outs if an
  MCP discovery/health path must stay open, the same pattern that exempts
  `/v1/admin/counters` today). Because enforcement lives in the MCP VS's own SSO policy, the
  tool-authorization DataScript (§6) and any MCP-specific exemptions never touch the LLM
  route.

**Lifecycle.** The shared issuer Pool/AuthProfile are owned by the `AIGatewayAuthPolicy`;
the MCP policy only *references* them. So `DeleteOAuthObjects` must not run while a
referencing `AIMCPRoutePolicy` still exists — AKO reference-counts the shared graph (delete
the MCP SSO policy with the MCP policy; delete the shared Pool/AuthProfile only when the
last referrer, including `llm-auth` itself, is gone). This is the one new lifecycle wrinkle
versus the existing single-owner model and is called out for the implementation (§11).

> **Why not just attach the same `AIGatewayAuthPolicy` to the MCP route?** That was offered
> and rejected (it gives identical enforcement with no place to hang MCP tool-authz or
> MCP-specific exemptions). Referencing the auth policy — rather than re-attaching it —
> keeps the IdP shared while giving MCP its own enforcement surface.

---

## 6. Role-based tool authorization (the AKO-programmed layer)

OAuth at the VS proves *who* the caller is; it does not decide *which tool* they may call.
The 32.1.1 probe **confirmed there is no native object for this** (§2), so AKO generates it —
and it is the same problem `AIModelRoutePolicy` already solved for the `model` field: **read
the body in `HTTP_REQ_DATA`, branch on a verified claim**. The mechanism is reused verbatim
from [`modelroute_datascript.go`](../../ako-gateway-api/aigateway/modelroute_datascript.go),
and is already implemented as `GenerateMCPToolAuthScripts`
([`mcproute_datascript.go`](../../ako-gateway-api/aigateway/mcproute_datascript.go)).

This DataScript runs in **`HTTP_REQ_DATA`** (it needs the request *body*), whereas Avi's
built-in `System-Standard-MCP` session DataScript runs in `HTTP_REQ`/`HTTP_RESP` (it only
reads the `Mcp-Session-Id` *header*). They occupy different events on the same VS, so AKO's
tool-authz layer and Avi's native session persistence **coexist** — the one remaining item to
confirm under load (§10).

An MCP `tools/call` request body looks like:

```json
POST /mcp
{ "jsonrpc": "2.0", "id": 7, "method": "tools/call",
  "params": { "name": "filesystem.write", "arguments": { ... } } }
```

The generated DataScript reads `method` and (for `tools/call`) `params.name`, then checks
it against the role's allow-list:

```lua
-- Event 1 — HTTP_REQ: enable request-body buffering (32 KB cap; the method/tool
-- name sits at the JSON head, so the buffered head suffices — same as model routing).
pcall(function() avi.http.set_request_body_buffer_size(32768) end)
```

```lua
-- Event 2 — HTTP_REQ_DATA: read body, extract the tool, authorize by role.
local ok, body = pcall(function() return avi.http.get_req_body(32768) end)
body = (ok and body) or ""

local method = extract_json_string(body, "method")          -- generated helper
local tool   = extract_nested_string(body, "params", "name") -- generated helper

-- Only tools/call is per-tool gated; other methods (initialize, tools/list, ping)
-- are session/protocol plumbing and pass the OAuth check alone.
if method == "tools/call" then
  local role = jwt_claim(ROLE_CLAIM)                         -- shared AI Gateway helper
  if not allowed(role, tool) then                            -- baked-in ALLOW table + globs
    avi.http.response(403, {["Content-Type"]="application/json"},
      '{"jsonrpc":"2.0","error":{"code":-32001,"message":"tool_not_authorized"}}')
    return
  end
end
```

`extract_json_string`, `allowed`, the baked-in `ALLOW` table, and `ROLE_CLAIM` are emitted
from the CR as constant tables/helpers — the same generation approach as `MODEL_TIERS` /
`TIER_PG` in [`modelroute_datascript.go`](../../ako-gateway-api/aigateway/modelroute_datascript.go).
`jwt_claim` is the **same** sandbox-safe, `pcall`-guarded claim accessor the auth and
model-route scripts already use ([`datascript.go`](../../ako-gateway-api/aigateway/datascript.go)).
`extract_nested_string` (for `params.name`) is the one small new helper — a second-level
quote scan over the already-proven `string.find` technique (no `string.match`, which the SE
sandbox lacks).

**Fail-closed properties:**

- **Oversized body.** If the JSON-RPC envelope exceeds the 32 KB buffer (large
  `arguments`), `method`/`params.name` still sit at the head, so extraction succeeds; but
  the implementation must fail **closed** (deny `tools/call`) if either field comes back
  empty, matching the response-side `get_response_body` caution in
  [model-routing.md](model-routing.md#feasibility--verified-on-avi-3122).
- **`onUnauthorized: Log`** swaps the `avi.http.response(403,…)` for a counter increment +
  `add_header` tag, so a rollout can observe denials before enforcing them.

---

## 7. Composition with the existing policies

`AIMCPRoutePolicy` is additive and orthogonal to the other three CRDs. On the MCP child VS
the event ordering mirrors the LLM VS (auth first so claims are readable, then body-aware
authorization, then budget):

1. **OAuth/OIDC** — the MCP VS's own `SSO_TYPE_OAUTH` policy (§5). Claims become readable to
   DataScripts via `avi.http.oauth_get_claim()`.
2. **Tool authorization** — `AIMCPRoutePolicy`'s `HTTP_REQ` → `HTTP_REQ_DATA` scripts (§6),
   indexed *after* auth so the role claim is populated.
3. **Token budgets (optional, reuses [`AITokenRateLimitPolicy`](ai-gateway.md))** — an
   `AITokenRateLimitPolicy` can target the **same MCP route** to put per-consumer or
   per-group **call budgets** on tool traffic (e.g. cap `filesystem.*` calls/hour), using
   the existing counter machinery. Token-*usage* budgets are LLM-shaped; for MCP the natural
   limit is **request-rate / per-tool call count**, which `requestRateLimit` and the
   counter keys already support. Reuse, not new code.

The result: **one verified identity** governs an agent's entire loop — LLM calls on the
inference route and tool calls on the MCP route — authenticated by the same IdP and
budget-governed by the same policy family. That is the "single control plane for the entire
agent execution loop" framing already promised in [ai-gateway.md](ai-gateway.md#overview).

---

## 8. UI integration (specification — UI lives in an external repo)

The dashboard UI (`ako-inference-demo/ai-gateway-ui`, a Go binary + embedded Clarity SPA)
is **not** in this repository; this section specifies the work, it does not implement it.

A new **"MCP Gateways"** section sits alongside the existing LLM views and **shares the same
OIDC login/session** the console already uses — no second sign-in, because the MCP Gateway
shares the IdP (§5). It surfaces:

| UI element | Reads / writes | Notes |
|---|---|---|
| **MCP Gateway list** | `Gateway` objects annotated `ai.ako.vmware.com/mcp: "true"` | Create/inspect the MCP Gateway + its `/mcp` `HTTPRoute`. |
| **Auth binding** | `AIMCPRoutePolicy.spec.authRef` → picks an existing `AIGatewayAuthPolicy` | The "tied to same auth as LLM" control: a dropdown of LLM auth policies; selecting one wires the shared IdP. |
| **Tool-access matrix** | `AIMCPRoutePolicy.spec.toolAccess.rules` | A role × tool grid (rows = job roles, cells = allow/deny), writing the `allow` lists. The console's most MCP-specific view. |
| **Session settings** | `AIMCPRoutePolicy.spec.session` | Persistence header + timeout. |
| **Tool-call / denial counters** | the read-only counters endpoint pattern ([ai-gateway.md](ai-gateway.md)) | If an `AITokenRateLimitPolicy` is attached to the MCP route, reuse the existing `GET /v1/admin/counters` polling to show per-role tool-call volume and `onUnauthorized: Log` denials. |

All writes are in-cluster CRD writes through the same authenticated path the console
already uses for the LLM policies — so the MCP section is, from the console's perspective,
"three more CRDs and a Gateway annotation," not a new integration surface.

---

## 9. MCP server registry & catalog onboarding (Option A)

Operators should not hand-write an `HTTPRoute` + `AIMCPRoutePolicy` for every tool server —
they should **browse a catalog and onboard with a click**. This section specifies that as a
**management-plane (UI) concern** ("Option A"): the dashboard reads an MCP **registry**, and
on *Add* it writes the same CRDs §8 already describes. AKO is **unchanged** — the source of
truth remains the generated CRDs; the registry is a convenience layer above them.

> A heavier, fully-reconciled alternative — an `MCPServer` CRD that AKO expands and keeps in
> sync with the catalog (version/withdrawal/drift tracking) — is deferred as **Option B**
> (§13 roadmap). Option A is the stepping stone; the migration is eased by §9.4.

### 9.1 Which registry — two-tier model

The MCP ecosystem settled on a two-tier split, and the split *is* the recommendation:

| Tier | What it is | Role here |
|---|---|---|
| **Official MCP Registry** (`registry.modelcontextprotocol.io`) | Community registry of record (Anthropic, GitHub, Microsoft, PulseMCP). Built for **programmatic consumption by subregistries**, not raw end-use. | **Upstream** you federate *from*. |
| **Private subregistry** (same open spec) | A curated, governed catalog: a vetted subset of upstream **+** internal servers. Self-host the open-source `modelcontextprotocol/registry` reference implementation; or ServiceNow / Docker private MCP catalogs. | **What the console actually browses.** |

**Recommended:** run a **self-hosted private subregistry**, seed it with a vetted subset of
the official registry, and point the console at *its* API — never at the public registry
directly. The curation/approval decision then lives inside your control plane, consistent
with the multi-cluster-governance thesis
([ai-gateway-multicluster-strategy.md](ai-gateway-multicluster-strategy.md)). A public index
such as Smithery is fine to *link humans to* for discovery, but it is not the enforcement
catalog.

### 9.2 The contract — `server.json`

Every tier speaks the same schema (current `2025-12-11`, camelCase since `2025-09-29`), so
the console integrates once. The fields that matter for a gateway:

| `server.json` field | Meaning | Gateway relevance |
|---|---|---|
| `name` | reverse-DNS id, e.g. `io.github.acme/filesystem` | Stamped as provenance on generated objects (§9.4). |
| `description`, `version` | catalog display + pinned version | Shown in the browse view. |
| **`remotes[]`** | hosted server: `{ type: streamable-http \| sse, url, headers }` | **The common case for a gateway** — a URL + transport maps straight to an Avi backend. |
| `packages[]` | self-run server: `{ registryType: oci\|npm\|pypi, identifier, version, … }` | Out of scope for Option A — the operator (or GitOps) deploys it, then onboards it as a **reference to its in-cluster Service** (treated like a `remote`). AKO does **not** deploy workloads. |

### 9.3 `server.json` → Avi/Gateway object mapping

On *Add*, the console materializes one catalog entry into the §8 objects:

| Catalog input | Generated object | Notes |
|---|---|---|
| `remotes[].url` (external) | `Service` (`ExternalName`) **or** `Endpoints` → Avi **Pool** | Re-originate TLS to the upstream MCP server; the Pool is the route backend. |
| in-cluster server (from `packages[]`, pre-deployed) | reference to the existing `Service` | No new backend created; just point the route at it. |
| chosen MCP Gateway | `HTTPRoute` (path `/mcp/<server>`) on the `ai.ako.vmware.com/mcp: "true"` Gateway | One route per onboarded server. |
| chosen auth + tool access | `AIMCPRoutePolicy` (`authRef`, `session`, `toolAccess`) | The governance object from §4. |
| **server-advertised tools** (`tools/list`, or `server.json` capabilities) | **pre-filled `toolAccess` role × tool matrix** | The operator checks boxes against a real tool list instead of typing names — the key UX win. ⚠️ live `tools/list` is spike-gated (§10). |

### 9.4 Provenance breadcrumb (eases the future move to Option B)

Even though Option A's source of truth is the generated CRDs, the console **stamps the
catalog link onto them** as annotations so the connection to the registry is not lost:

```yaml
metadata:
  annotations:
    ai.ako.vmware.com/mcp-catalog-name: io.github.acme/filesystem
    ai.ako.vmware.com/mcp-catalog-version: "1.4.2"
    ai.ako.vmware.com/mcp-catalog-registry: https://mcp-registry.internal
```

This costs nothing now and means a later `MCPServer` controller (Option B) can **adopt**
existing routes by reading these annotations, rather than requiring a re-onboard. It also
lets a simple management-plane job *report* drift (compare the stamped version to the current
catalog) even before a reconciling controller exists — A's biggest gap (no
version/withdrawal tracking) becomes a read-only report instead of a blind spot.

### 9.5 Onboarding flow

```
 Official MCP Registry ──curate/import──► Private subregistry (server.json)
                                                  │  console: "Browse catalog"
                                                  ▼
                                operator picks a server, reviews advertised tools,
                                sets role×tool access + auth binding
                                                  │  "Add to MCP Gateway"
                                                  ▼
       console writes (in-cluster, same authed path as today):
         • backend   (ExternalName Service / Endpoints → Avi Pool)  [remotes only]
         • HTTPRoute (/mcp/<server>) on the MCP Gateway
         • AIMCPRoutePolicy (authRef = shared LLM IdP, session, toolAccess)
         • all stamped with catalog provenance annotations (§9.4)
```

Because every write is "the §8 CRDs, plus a backend Service," Option A adds **no new
integration surface to AKO** and **no new CRD** — it is entirely a console feature against
the existing objects.

### 9.6 Approved registry & governance — the enforced allow-list

The private subregistry is not merely a catalog; it is the **approved allow-list** the MCP
Gateway enforces. This is the product's **north-star governance object** — the single,
auditable place that decides which agent tools exist in the fleet. "Approved registry" does
three distinct jobs:

| Job | What it means | Where it lives |
|---|---|---|
| **Catalog** | vetted, discoverable MCP servers | the subregistry (§9.1) |
| **Approval** | how a server earns its place | curation = approval (MVP) → request/review workflow (future) |
| **Allow-list enforcement** | the gateway routes **only** to approved servers; unapproved are unreachable through it | the onboarding control plane (§9.5) — structural in Option A, reconciled in Option B |

**Approval model — phased.**

- *MVP — curation **is** approval.* A server is approved iff it is present in the private
  subregistry; importing a vetted entry from upstream is the approval act. The subregistry's
  contents are the allow-list — no separate workflow, no new state.
- *Future — explicit workflow.* Entries carry `Requested → UnderReview → Approved/Rejected`
  with approver roles and an **audit trail** (who approved what, when), enabling CVE-driven
  **revocation** as a first-class action. This is the enterprise-governance differentiator;
  it rides on Option B's controller.

**Enforcement — default-deny, honestly scoped.** Because the console (A) / controller (B)
generates an MCP Gateway route **only** for an approved entry, an unapproved server has **no
path through the gateway** — default-deny is *structural*, not a rule to maintain. Two honest
boundaries:

- The gateway can only deny traffic **it sees**. True egress lockdown — an agent must not be
  able to reach an MCP server *except* through the gateway — requires forcing all MCP egress
  through the MCP Gateway with Kubernetes **NetworkPolicy** and/or **NSX/vDefend DFW**. The
  registry is the allow-list; the security fabric makes it inescapable. This is the concrete
  vDefend tie-in the strategy memo
  ([ai-gateway-multicluster-strategy.md](ai-gateway-multicluster-strategy.md) §4.2) is
  reaching for — a per-cluster proxy with a hand-edited list cannot claim it the same way.
- **Revocation.** An approved server later withdrawn (deprecated / CVE) is removed from the
  registry → its route is torn down → in-flight calls fail closed. In Option A this leans on
  the drift/withdrawal report (§9.4 annotations); in Option B the controller reconciles it
  automatically — so **revocation latency is a function of which option ships**, and is a
  reason the governed product ultimately wants B.

---

## 10. Feasibility — spikes on Avi 32.1.1

Same method as the model-routing spike (read-only probe, then a throwaway VS torn down
after). **Spike-1 is resolved** by the live 32.1.1 probe
([hack/spikes/mcp-probe.sh](../../hack/spikes/mcp-probe.sh)); the rest gate the remaining
capability claims.

| # | Question | Status / pass criterion |
|---|---|---|
| **1 (object model)** | What are the real 32.1.1 object names for the **MCP application profile** and **session persistence**? | **✅ RESOLVED (probe).** App profile = `APPLICATION_PROFILE_TYPE_HTTP` + `app_service_type: APP_SERVICE_TYPE_HTTP_MCP` (system **`System-Secure-HTTP-MCP`**). Session persistence = the built-in DataScriptSet **`System-Standard-MCP`** (VS persistence tables on `Mcp-Session-Id`), **not** a persistence profile. Per-tool authz has **no** native object → DataScript (§6). AKO reuses both system objects. |
| **2** | Does the **OAuth resource-server** flow ([oauth_rest.go](../../ako-gateway-api/aigateway/oauth_rest.go)) enforce on an MCP VS, and can **one issuer Pool + AuthProfile back two SSO policies** (LLM + MCP)? | A token minted for the LLM gateway is accepted at the MCP VS; revoking it rejects both. |
| **3 (coexistence)** | Does AKO's `HTTP_REQ_DATA` tool-authz DataScript run cleanly **alongside** the system `System-Standard-MCP` session DataScript (different events) on the same VS? | `tools/call` for `filesystem.write` is denied for `app-owner`, allowed for `operator`; `tools/list` passes for both — while sessions stay pinned. Body > 32 KB still extracts `method`/`name` from the head (else fails closed). |
| **4** | Does **SSE streaming** (the MCP `GET` response channel) proxy through the VS without full-response buffering, alongside session persistence? | A long-lived MCP stream stays open and sticky for the session timeout. |
| **5** | Reference-counted lifecycle (§5): deleting the MCP policy leaves the LLM auth intact; deleting the last referrer cleans the shared Pool/AuthProfile. | No orphaned and no prematurely-deleted OAuth objects across add/delete permutations. |
| **6** | Catalog onboarding (§9): does `tools/list` against a `remotes[]` server return a usable tool set to pre-fill the `toolAccess` matrix, and does an `ExternalName`/`Endpoints` Pool re-originate TLS to an external MCP server? | "Add from catalog" produces a working, governed route; the role × tool grid is pre-populated from the live server. |

> **Residual carried from model-routing:** the 32 KB request-body buffer cap and
> `get_req_body` head-buffering behaviour — unchanged here, since the MCP `method`/tool name
> sit at the JSON head.

---

## 11. Implementation outline (for the follow-up code pass)

Mirrors how `AIModelRoutePolicy` was wired (commits `c05fc5bc` → `a2e7b995` → `404775d4`):

1. **CRD + Go types — ✅ done.** `helm/ako/crds/ai.ako.vmware.com_aimcproutepolicies.yaml` and
   `AIMCPRoutePolicy` types/validation in
   [`mcproute_types.go`](../../ako-gateway-api/aigateway/mcproute_types.go), mirroring
   [`modelroute_types.go`](../../ako-gateway-api/aigateway/modelroute_types.go).
2. **RBAC — ✅ done.** `aimcproutepolicies` (+ `/status`) added to
   [`helm/ako/templates/clusterrole.yaml`](../../helm/ako/templates/clusterrole.yaml) **in the
   same change**, guarded by a test
   ([`rbac_clusterrole_test.go`](../../ako-gateway-api/aigateway/rbac_clusterrole_test.go)) so
   the model-routing RBAC miss (`404775d4`) cannot recur silently.
3. **Tool-authz DataScript — ✅ done.** `GenerateMCPToolAuthScripts` in
   [`mcproute_datascript.go`](../../ako-gateway-api/aigateway/mcproute_datascript.go).
4. **Informer + handlers** — a dynamic informer and `SetupMCPRoutePolicyEventHandlers` wired
   in [`gateway_crd_controller.go`](../../ako-gateway-api/k8s/gateway_crd_controller.go),
   started in [`gateway_controller.go`](../../ako-gateway-api/k8s/gateway_controller.go), plus
   a `PolicyStore` entry (`GetMCPRoutePoliciesForRoute`) in
   [`controller.go`](../../ako-gateway-api/aigateway/controller.go).
5. **Translator** — `ApplyMCPRoutePolicy(key, policy, vsNode, …)` invoked from the same
   per-child-VS hook as `ApplyAuthPolicy` / `ApplyModelRoutePolicy`. Per the 32.1.1 probe it
   **reuses Avi's system objects**: set the VS `application_profile_ref` =
   `System-Secure-HTTP-MCP` and attach the `System-Standard-MCP` DataScriptSet (Avi's session
   persistence — AKO writes no session logic); resolve `authRef` and reuse/ref-count the OAuth
   graph; emit the MCP SSO policy; and attach the already-built tool-authz DataScript set.
6. **Gateway annotation** — honor `ai.ako.vmware.com/mcp: "true"` when building the Gateway's
   VS to apply step 5's system application profile + session DataScript.

> **Status semantics.** `AIMCPRoutePolicy.status.conditions` reports an unresolved `authRef`
> (no such `AIGatewayAuthPolicy`) and unknown tools/roles referenced in `toolAccess`, the
> same validation shape as `AIModelRoutePolicy`.

---

## 12. Failure modes (graceful degradation)

| Condition | Behaviour |
|---|---|
| `AIMCPRoutePolicy` absent / unreconciled | MCP route serves as a plain HTTPRoute — no session profile, no tool-authz. Reachable but ungoverned. (Benign, like a missing `AIModelRoutePolicy`.) |
| `authRef` unresolved | MCP VS has no SSO policy → fail closed: reject with 401 (don't silently serve unauthenticated). Status condition flags the dangling ref. |
| Tool name unextractable (oversized/garbled body) | `tools/call` denied (fail closed); other methods unaffected. |
| Session persistence profile unsupported on the build | Sessions still work but may rebalance mid-run (degraded statefulness); alarmed. Spike-1 gates this. |
| Shared OAuth graph deleted out from under MCP | Prevented by ref-counting (§5/§11); MCP enforcement never depends on an object it doesn't keep alive. |

---

## 13. Roadmap

| Phase | Feature | Status |
|---|---|---|
| 3 | `AIMCPRoutePolicy` + MCP Gateway annotation; shared-IdP/separate-SSO auth | Design (this doc); **spike-gated** |
| 3 | Native MCP session persistence (`Mcp-Session-Id`) | Design; Spike-1 |
| 3 | Role-based tool authorization DataScript | Design; reuses model-route mechanism |
| 3 | Per-tool call budgets via `AITokenRateLimitPolicy` on the MCP route | Design (reuse) |
| 3.x | UI "MCP Gateways" section (external repo) | Spec (§8) |
| 3.x | Registry-driven catalog onboarding — **Option A** (console browses a private subregistry, writes the §8 CRDs + provenance annotations) | Spec (§9) |
| 3.x | **Approved-registry allow-list** — default-deny: the gateway routes only to approved servers (structural) | Spec (§9.6) |
| 4 | `MCPServer` CRD — **Option B** (AKO reconciles catalog entries; version/withdrawal/tool-drift tracking; adopts Option A's annotated routes) | Idea (§9 intro) |
| 4 | Approval **workflow** (Requested→Approved, audit trail) + **CVE-driven revocation**; egress lockdown via NetworkPolicy / NSX-vDefend DFW | Idea (§9.6) |
| 4 | Cross-site MCP delivery (compose with [ai-gateway-multisite.md](ai-gateway-multisite.md) GSLB) | Idea |

---

## 14. Related docs

- [AI Gateway](ai-gateway.md) — OAuth/OIDC auth + token budgets; the IdP this design shares
  and the DataScript/claim patterns it reuses
- [Model-Based Routing](model-routing.md) — the body-inspection + `oauth_get_claim`
  entitlement mechanism reused here for tool authorization
- [Multi-Site Model Delivery](ai-gateway-multisite.md) — the dedicated-VS isolation rationale,
  and the future cross-site MCP composition
- [Multi-Cluster Strategy](ai-gateway-multicluster-strategy.md) — §4.3 of which this design
  supersedes (MCP is native as of Avi 32.1.1)

## Sources (Avi 32.1.1 MCP features)

- [Avi Load Balancer 32.1.1: VCF 9.1, MCP Traffic, and the Licensing Shift — the segmented nerd](https://thesegmentednerd.com/blog/2026-05-12-avi-32-1-1/)
- [Release Notes for Avi Load Balancer Version 32.1.1 — Broadcom TechDocs](https://techdocs.broadcom.com/us/en/vmware-security-load-balancing/avi-load-balancer/avi-load-balancer/32-1/vmware-avi-load-balancer-release-notes/release-notes-for-avi-load-balancer-version-32-1-1.html)
