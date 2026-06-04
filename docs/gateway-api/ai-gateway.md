# AKO AI Gateway

## Overview

The AKO AI Gateway extension adds **OAuth/OIDC authentication** and **token-based rate limiting**
(including **per-group token budgets**) to any HTTPRoute managed by AKO's Gateway API
controller. Both capabilities are expressed as lightweight Kubernetes CRDs that attach to an
HTTPRoute via a `targetRef` and are reconciled into existing Avi Service Engine features — no
sidecars, no external rate-limit servers, no changes to the data-plane binary.

This feature builds directly on top of the [AKO Inference Extension](inference-extension.md).
It is designed to protect and govern the same LLM endpoints that `InferencePool` load-balances.

A Phase 3 `AIMCPPolicy` extension will bring the same governance to **MCP tool servers**,
making the AI Gateway the single control plane for the entire agent execution loop — inference
calls to LLMs and tool calls to MCP servers, authenticated and budget-governed by the same
policies and the same verified identity.

> **Why OAuth/OIDC and not raw JWT validation?** Avi's `SSO_TYPE_JWT` validates a bearer token
> but **strips the `Authorization` header before any DataScript runs**, and this Avi build has no
> mechanism to inject validated claims as headers — so a token-budget DataScript cannot read the
> per-group claim. Avi's **OAuth/OIDC** flow is a browser, session-cookie **authorization-code**
> flow: the Service Engine runs the login redirect, exchanges the code for a token, validates it,
> and exposes the verified claims to DataScripts via `avi.http.oauth_get_claim()`. That is the
> supported path for "validate the token **and** read its claims in a DataScript", so the AI
> Gateway builds on it. Because OAuth/OIDC requires TLS, the **Gateway listener must be HTTPS**.

---

## How It Works

```
┌─────────────────────────────────────────────────────┐
│                  Kubernetes Control Plane            │
│                                                      │
│  AIGatewayAuthPolicy ──────────────────────────┐    │
│  AITokenRateLimitPolicy ───────────────────────►│   │
│                                                 │    │
│  AKO (ako-gateway-api)                          │    │
│  ┌──────────────────────────────────────────┐   │    │
│  │  PolicyStore     (in-process cache)      │◄──┘    │
│  │  BuildChildVS    (graph translator)      │        │
│  │  ApplyAuthPolicy / ApplyTokenRateLimit   │        │
│  └────────────────┬─────────────────────────┘        │
└───────────────────┼─────────────────────────────────┘
                    │ Avi REST API
                    ▼
┌─────────────────────────────────────────────────────┐
│  Avi Controller   (objects AKO creates/manages)      │
│  Pool                ←  issuer Service endpoints      │
│  AuthProfile (OAUTH) ←  issuer + jwks_uri → the Pool  │
│  SSOPolicy (OAUTH)   ←  references the AuthProfile     │
│  Virtual Service (HTTPS / TLS-terminating)           │
│  ├─ SsoPolicyRef + oauth_vs_config ← OIDC validation  │
│  └─ DataScriptSet ← budget enforcement + accounting   │
└─────────────────────────────────────────────────────┘
```

**Control flow:**

1. User creates `AIGatewayAuthPolicy` and/or `AITokenRateLimitPolicy` targeting an `HTTPRoute`
   on an **HTTPS** Gateway listener.
2. AKO's informers detect the event, parse the CR into the `PolicyStore`, and re-enqueue the
   targeted HTTPRoute.
3. During `BuildChildVS`, AKO calls `ApplyAuthPolicy` and `ApplyTokenRateLimitPolicy` for each
   policy attached to the route. For auth, AKO **creates/updates the issuer Pool, the
   `AUTH_PROFILE_OAUTH` AuthProfile and the `SSO_TYPE_OAUTH` SSOPolicy** in Avi and wires
   `SsoPolicyRef` + `oauth_vs_config` onto the VS (the same way the `SSORule` CRD attaches OAuth).
4. The resulting Avi Virtual Service node and its DataScripts are pushed to the Avi controller
   via the normal REST path.
5. At request time the SE runs the OIDC auth-code flow against the issuer, validates the token,
   and the token-budget DataScript reads the verified `group` (and `sub`) via
   `avi.http.oauth_get_claim()`.

---

## Prerequisites

- `featureGates.GatewayAPI: true` in `values.yaml`
- `aiGateway.enabled: true` in `values.yaml`
- An AKO image built from this branch (the AI Gateway code lives behind the `AI_GATEWAY_ENABLED`
  runtime flag — no rebuild is needed to toggle it, only a Helm value change)
- Install the two CRDs on the cluster before enabling the feature flag (see
  [Installing the CRDs](#installing-the-crds))
- For auth: the **Gateway listener must be HTTPS** (OAuth/OIDC requires TLS termination).
- For auth: the OIDC issuer must be an **OAuth/OIDC provider** (discovery, `/authorize`,
  `/token` code exchange, `/jwks`). The **Avi Service Engine** reaches the issuer's `/jwks` and
  `/token` at runtime through an Avi **Pool** that AKO builds from the issuer Service's
  endpoints — so the issuer must be reachable from the SE data path (in Azure CNI, pod IPs are
  VNet-routable; the in-cluster `jwt-issuer` works as-is).

---

## Enabling the Feature

Add the following to `values.yaml`:

```yaml
featureGates:
  GatewayAPI: true

aiGateway:
  enabled: true
```

The Helm chart injects `AI_GATEWAY_ENABLED=true` into the `ako-gateway-api` container and grants
the necessary ClusterRole RBAC for the two CRDs automatically.

| Environment Variable | Default | Description |
|---|---|---|
| `AI_GATEWAY_ENABLED` | `false` | Master switch for AI Gateway CRD watching and policy application |

> The `ako-gateway-api` container is **distroless** (no shell, no `env` binary) and AKO does not
> log the flag, so verify it from the pod spec rather than `kubectl exec ... -- env`:
> ```bash
> kubectl get pod ako-0 -n avi-system \
>   -o jsonpath='{range .spec.containers[?(@.name=="ako-gateway-api")].env[*]}{.name}={.value}{"\n"}{end}' \
>   | grep AI_GATEWAY
> ```

---

## Installing the CRDs

```bash
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aigatewayauthpolicies.yaml
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aitokenratelimitpolicies.yaml
```

Both CRDs are namespaced and live under the `ai.ako.vmware.com` API group, version `v1alpha1`.

---

## OAuth/OIDC Authentication — `AIGatewayAuthPolicy`

### What it does

- Authenticates inbound requests via the Avi **OAuth/OIDC** flow on an SSO Policy that **AKO
  creates and manages**. Unauthenticated requests are redirected into the issuer's
  authorization-code login; once authenticated, the SE validates the token and carries a session.
- Makes the **verified** token claims (e.g. `sub`, `group`) available to downstream
  token-rate-limit enforcement via `avi.http.oauth_get_claim()`.

> **This is a browser/session flow, not an API-bearer flow.** Avi obtains the token itself
> through the OIDC redirect and stores it in a session cookie — it does not validate a bearer
> token presented on the `Authorization` header. Drive the demo with a cookie jar (a browser, or
> `curl -c jar`), not by sending `Authorization: Bearer`.

### AKO-managed Avi objects (no manual OAuth setup)

When you apply an `AIGatewayAuthPolicy`, AKO creates/updates three Avi objects and wires them
onto the VS. Names are derived from the policy namespace/name:

| Avi object | Name | Built from |
|---|---|---|
| `Pool` | `<ns>-<name>-oauth-pool` | the issuer Service's endpoint IPs (so the SE can reach `/jwks` and `/token`) |
| `AuthProfile` (type `AUTH_PROFILE_OAUTH`) | `<ns>-<name>-oauth` | `jwt.issuer`, `jwt.jwksUri`, and the OAuth endpoints, pointing at the Pool |
| `SSOPolicy` (type `SSO_TYPE_OAUTH`) | `<ns>-<name>-oauth-sso` | references the AuthProfile |
| VS `sso_policy_ref` + `oauth_vs_config` | — | client `app_settings` + `resource_server` (`access_type: JWT`, `audiences[0]`), `redirect_uri` on the route host |

Unlike the JWT flow, the **SE fetches the JWKS at runtime through the Pool** (it has no cluster
DNS), so AKO resolves the issuer Service's endpoints into Pool servers. The Pool is treated as
immutable once OAuth-bound (Avi blocks server edits); if the issuer endpoints change, delete and
re-apply the `AIGatewayAuthPolicy`. The objects are deleted when the policy is removed.

### How claims reach rate limiting

After the OIDC flow validates the token, the `AITokenRateLimitPolicy` DataScript reads the
required claims (e.g. `sub`, `group`) with `avi.http.oauth_get_claim(0, "<claim>")` — the
SE-verified claim, not a client-supplied value. (The accessor returns a Lua table on this Avi
build; AKO's generated DataScript unwraps the first scalar.)

### Example

The issuer must be a real OIDC provider and the route must sit on an HTTPS Gateway listener:

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIGatewayAuthPolicy
metadata:
  name: llm-auth
  namespace: inference
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  jwt:
    issuer: "http://jwt-issuer.inference.svc.cluster.local:8080"          # token `iss`
    jwksUri: "http://jwt-issuer.inference.svc.cluster.local:8080/jwks"     # SE fetches via the Pool
    audiences:
      - "llm-api"               # access-token aud + OAuth client_id
    identityClaim: sub          # default; becomes the consumer identity
    forwardClaims:
      - group                   # group claim used for per-group budgets
  identityHeader: x-ai-consumer # default
  onFailure:
    statusCode: 401             # default
```

---

## Token Rate Limiting — `AITokenRateLimitPolicy`

### What it does

- Enforces per-consumer (or per-IP) **token budgets** over rolling time windows.
- Supports **per-group budgets** (e.g. group1 = 500 tokens/hr, group2 = 1000 tokens/hr) keyed on
  a JWT claim.
- Optionally enforces a **classic requests-per-second** soft rate limit.
- All enforcement runs as Lua DataScripts on the Avi Service Engine — no extra infrastructure.

### Token usage source — the response body (`HTTP_RESP_DATA`)

Token accounting parses the OpenAI-compatible `usage` block **directly from the response body**,
so it works with **stock vLLM** — no token-header contract and no sidecar required.

The `HTTP_RESP` event can't see the body, but the **`HTTP_RESP_DATA`** event can:

1. In `HTTP_RESP`, `avi.http.set_response_body_buffer_size(N)` tells the SE to buffer the body.
2. In `HTTP_RESP_DATA`, `avi.http.get_response_body(N)` returns the buffered body, and the
   DataScript reads `usage.total_tokens` / `prompt_tokens` / `completion_tokens` from the JSON
   (sandbox-safe `string.find` + a digit scan — Avi's Lua sandbox lacks `string.match`).

| Token field (read from `usage`) | Meaning |
|---|---|
| `prompt_tokens` | prompt/input tokens |
| `completion_tokens` | completion/output tokens |
| `total_tokens` | total (falls back to prompt + completion if absent) |

Two safeguards keep the body-parse path honest:

- **Gated to POST + JSON.** `HTTP_RESP` only enables buffering when the request was a `POST` and
  the response `Content-Type` is `application/json`. So `GET /metrics` scrapes, health checks,
  and streaming `text/event-stream` responses are never buffered or scanned — no wasted SE work
  and no accidental parsing of non-completion bodies.
- **Fail-closed on unreadable usage.** If a buffered JSON completion (it has the
  `"object":"chat.completion"` / `"choices"` markers, which survive a tail truncation) yields no
  parseable `usage` — because the body was larger than the buffer, or compressed — the script
  charges a conservative penalty (`FailClosedTokens`, ~`RespBodyBufferKB*1024/4`) instead of 0,
  so the consumer's **next** request is blocked rather than letting an over-buffer response slip
  through the budget unmetered.

> **Buffering cost / streaming.** `usage` sits at the *end* of the body, so the SE buffers the
> whole response before parsing — controlled by `RespBodyBufferKB` (default 256 KB ≈ ~40K output
> tokens; size it to your model's `max_tokens` — roughly `max_tokens * 6` bytes). This buffers
> (store-and-forward) rather than streams, so it suits non-streaming API traffic. The memory cost
> is the buffered body per in-flight response, so very high concurrency × large responses eats SE
> memory (and competes with connection capacity). For **streaming** (SSE) responses, buffering
> the whole stream defeats token-by-token delivery — there, meter in a proxy/sidecar and report
> out-of-band, or rate-limit by request count instead. The response-header approach
> (`X-Prompt-Tokens` / `X-Completion-Tokens` / `X-Total-Tokens` emitted by a sidecar) remains a
> valid alternative when you want to keep the SE in streaming pass-through.

### Avi object mapping

| Policy field | Avi mechanism |
|---|---|
| `limits[]` | Three **DataScript** nodes per VS: `<vsname>-ai-tok-req` (HTTP_REQ — enforce budget), `<vsname>-ai-tok-resp` (HTTP_RESP — enable response-body buffering), and `<vsname>-ai-tok-respdata` (HTTP_RESP_DATA — parse `usage` from the body and update counters). |
| `requestRateLimit` | Soft token-bucket logic prepended to the `HTTP_REQ` DataScript. |

Counters are stored in the Avi VS string table (`avi.vs.table_lookup` / `table_remove` /
`table_insert`), which Avi replicates across all SEs hosting the VS.

> **Consistency model:** counters are **eventually consistent** across scaled-out SEs — a
> consumer can briefly overshoot a budget by about one request-window before replication
> catches up. RPS limiting is also soft (per-window token bucket). Both are intentional
> trade-offs; the Phase 1.5 roadmap item is Avi's native distributed rate limiter for exact
> cross-SE enforcement.

### Identity resolution

The consumer identity (the counter key) is resolved in this order:

1. The verified `sub` claim from `avi.http.oauth_get_claim()` (when `AIGatewayAuthPolicy` is in use).
2. The configured identity header (`identitySource.header`, default `x-ai-consumer`).
3. The fallback: `clientIP` (default) keys on the source IP; `reject` returns HTTP 401 when no
   identity is found.

```yaml
identitySource:
  header: x-ai-consumer    # default
  fallback: clientIP        # "clientIP" (default) or "reject"
```

### Flat (single) budget example

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AITokenRateLimitPolicy
metadata:
  name: llm-limits
  namespace: inference
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  identitySource:
    header: x-ai-consumer
    fallback: clientIP
  limits:
    - name: hourly-consumer-tokens
      key: consumer               # one counter per consumer identity
      tokens: total               # prompt + completion
      budget: 100000
      window: 1h
      action:
        type: Reject
        statusCode: 429
        retryAfter: true
  requestRateLimit:
    requestsPerSecond: 50
    burst: 100
    key: consumer                 # "consumer" or "clientIP"
```

### Per-group budget example

The counter stays **per-consumer** (each user gets their own running total), but the *budget
ceiling* is looked up from the user's group claim. Users in `group1` get 500 tokens/hour; users
in `group2` get 1000. Requests whose group is not listed are rejected with HTTP 403 (set a
positive `budget` to instead use it as the fallback ceiling for unknown groups).

```yaml
spec:
  limits:
    - name: hourly-group-budget
      key: consumer            # per-user counter
      groupHeader: group       # JWT claim (decoded from the token) that selects the budget
      groupBudgets:
        group1: 500
        group2: 1000
      budget: 0                # 0 = reject unknown groups (403); >0 = fallback ceiling
      tokens: total
      window: 1h
      action:
        type: Reject
        statusCode: 429
        retryAfter: true
```

### Token-limit actions

| `action.type` | Behaviour |
|---|---|
| `Reject` (default) | Returns the configured `statusCode` (default 429) immediately. |
| `Log` | Allows the request but logs the budget overage. Useful for shadow-mode rollout. |

When `retryAfter: true` is set, the DataScript adds a `Retry-After` header pointing to the
current window boundary.

---

## Combining Auth and Group-based Rate Limiting

Apply both policies to the same HTTPRoute for the full stack — OAuth/OIDC auth resolves identity
and group, and the token policy enforces per-group budgets keyed on the verified `sub`:

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIGatewayAuthPolicy
metadata: { name: llm-auth, namespace: inference }
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route }
  jwt:
    issuer: "http://jwt-issuer.inference.svc.cluster.local:8080"
    jwksUri: "http://jwt-issuer.inference.svc.cluster.local:8080/jwks"
    audiences: [llm-api]
    identityClaim: sub
    forwardClaims: [group]
  identityHeader: x-ai-consumer
---
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AITokenRateLimitPolicy
metadata: { name: llm-limits, namespace: inference }
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route }
  identitySource: { header: x-ai-consumer, fallback: clientIP }
  limits:
    - name: hourly-group-budget
      key: consumer
      groupHeader: group
      groupBudgets: { group1: 500, group2: 1000 }
      budget: 0
      tokens: total
      window: 1h
      action: { type: Reject, statusCode: 429, retryAfter: true }
```

AKO applies both policies during the same `BuildChildVS` reconcile cycle.

---

## API Reference

### AIGatewayAuthPolicy

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.targetRef.group` | string | yes | API group of referent (`gateway.networking.k8s.io`) |
| `spec.targetRef.kind` | string | yes | `HTTPRoute` or `Gateway` |
| `spec.targetRef.name` | string | yes | Name of the referent in the same namespace |
| `spec.jwt.issuer` | string | yes | OIDC issuer URL; must match the token `iss` and is used as the OAuthProfile issuer |
| `spec.jwt.jwksUri` | string | yes | JWKS endpoint URL. The SE fetches it at runtime through the AKO-built issuer Pool |
| `spec.jwt.audiences` | []string | no | Access-token `aud` + OAuth `client_id` (uses `audiences[0]`) |
| `spec.jwt.identityClaim` | string | no | Claim used as consumer identity. Default: `sub` |
| `spec.jwt.forwardClaims` | []string | no | Claims of interest (e.g. `group`); read in the DataScript via `oauth_get_claim` |
| `spec.identityHeader` | string | no | Header for resolved identity. Default: `x-ai-consumer` |
| `spec.onFailure.statusCode` | int | no | HTTP status on auth failure. Default: `401` |

### AITokenRateLimitPolicy

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.targetRef.*` | PolicyTargetRef | yes | Same as AIGatewayAuthPolicy |
| `spec.identitySource.header` | string | no | Identity header to read. Default: `x-ai-consumer` |
| `spec.identitySource.fallback` | string | no | `clientIP` (default) or `reject` |
| `spec.limits[].name` | string | yes | Unique name (used as counter-key prefix) |
| `spec.limits[].key` | string | yes | Counter dimension: `consumer`, `header:<name>`, or `clientIP` |
| `spec.limits[].tokens` | string | no | `total` (default), `prompt`, or `completion` |
| `spec.limits[].budget` | int64 | yes | Max token count in the window. With `groupBudgets`, `0` rejects unknown groups; `>0` is the fallback ceiling |
| `spec.limits[].window` | string | yes | Time window: `30s`, `1m`, `1h`, `24h`, etc. |
| `spec.limits[].groupHeader` | string | no | JWT claim / header whose value selects a per-group budget |
| `spec.limits[].groupBudgets` | map[string]int64 | no | Group value → budget (e.g. `{group1: 500, group2: 1000}`). Requires `groupHeader` |
| `spec.limits[].action.type` | string | no | `Reject` (default) or `Log` |
| `spec.limits[].action.statusCode` | int | no | HTTP status on rejection. Default: `429` |
| `spec.limits[].action.retryAfter` | bool | no | Add `Retry-After` header on rejection |
| `spec.requestRateLimit.requestsPerSecond` | int | yes* | Sustained RPS limit (*required when block is present) |
| `spec.requestRateLimit.burst` | int | no | Max burst above sustained rate. Default: equals RPS |
| `spec.requestRateLimit.key` | string | no | `clientIP` (default) or `consumer` |

---

## Troubleshooting

**Policy not taking effect after apply**

```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "AIGateway\|ai-tok\|SsoPolicyRef\|issuer Pool\|OAuth"
```

**Request 302-redirects to the issuer and never completes**

That is the OIDC login redirect — expected for an unauthenticated request. Drive the flow with a
cookie jar so the auth-code exchange and session cookie are followed (`curl -c jar -b demo_user=…`,
or a browser). Confirm the **Gateway listener is HTTPS** (OAuth needs TLS) and that the
`redirect_uri` path falls under the route's path prefix so the callback lands on the child VS.

**Request 404s on the OAuth callback**

The callback must be content-switched to the EVH child holding `oauth_vs_config`; AKO places it
under the route prefix (e.g. `/v1/oauth/callback`). If you changed the route path, the callback
path must move with it.

**Auth succeeds but group budget rejects with 403 `unknown_group`**

The DataScript reads the verified `group` claim via `oauth_get_claim`. Confirm the issued token
carries the claim named in `groupHeader` and that its value is a key in `groupBudgets`. Check that
the issuer Pool is `OPER_UP` and reachable from the SE (a failed JWKS fetch makes Avi fall back to
the login redirect instead of validating).

**Token limits not enforced / 429 never fires**

- Confirm the backend returns a non-streaming `application/json` response that contains a JSON
  `usage` block with `total_tokens`, `prompt_tokens`, and/or `completion_tokens` — the DataScript
  parses these directly from the response body in the `HTTP_RESP_DATA` event. Without a parseable
  `usage` block the DataScript accounts 0 tokens (or charges `FailClosedTokens` if the body has
  `"choices"` / `"chat.completion"` markers but no readable `usage`).
- Confirm the body fits within `RespBodyBufferKB` (default 256 KB). `usage` sits at the end of
  the JSON, so if the response is larger than the buffer, the tail is lost and the fail-closed
  penalty fires instead of the real token count.
- For a clean single-counter demo, cap the SE group at 1 SE so all requests hit the same SE.

**`AI_GATEWAY_ENABLED` set but informers don't start**

Install the CRDs before AKO starts; AKO logs a warning if it cannot list the informer GVR.
Install the CRDs and restart the `ako-gateway-api` pod.

---

## Phase Roadmap

| Phase | Feature | Status |
|---|---|---|
| 1 | `AITokenRateLimitPolicy` — DataScript token accounting | ✅ Done |
| 1 | Soft RPS rate limiting (DataScript token bucket) | ✅ Done |
| 1.5 | Per-group token budgets (`groupHeader` / `groupBudgets`) | ✅ Done |
| 2 | `AIGatewayAuthPolicy` — OAuth/OIDC auth, AKO-managed `Pool` + `AuthProfile` + `SSOPolicy` lifecycle | ✅ Done |
| 2 | Verified claims in the DataScript via `oauth_get_claim` | ✅ Done |
| 2.5 | Native distributed rate limiter (`avi.vs.rate_limiter()`) for exact cross-SE limits | Planned |
| 2.5 | `AIObservabilityPolicy` — per-request token usage logging | Planned |
| 3 | `AIMCPPolicy` — route and govern MCP tool server endpoints from a registry using the same `targetRef` attachment model | Planned |
| 3 | MCP registry integration — auto-discover registered MCP servers from the registry, materialise them as AKO-managed Avi Pool backends | Planned |
| 3 | Cross-resource budget — unified per-consumer spend limit spanning token consumption (LLM) and call count (MCP tools) in a single rolling window | Planned |

---

## MCP Tool Governance (Phase 3 — Planned)

### The Gap Today

The AI Gateway governs the *inference path*: authenticating callers and enforcing token budgets
on LLM endpoints load-balanced by `InferencePool`. Modern AI agents, however, make two kinds
of calls:

- **Inference calls** → LLMs (already governed)
- **Tool calls** → MCP servers for retrieval, code execution, external APIs (not yet governed)

An [MCP registry](https://modelcontextprotocol.io) bridges this gap. It is the catalog of
available tool servers — the tool-side equivalent of the Kubernetes API that backs
`InferencePool`.

### Expanded Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                   Agent (AI workload)                            │
└───────────────────────────┬─────────────────────────────────────┘
                            │ HTTPS
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│  AKO AI Gateway  (Avi Virtual Service)                           │
│                                                                  │
│  AIGatewayAuthPolicy    → OAuth/OIDC session, claim resolution   │
│  AITokenRateLimitPolicy → per-consumer token + call budgets      │
│  AIMCPPolicy (Phase 3)  → MCP server routing + governance        │
│                                                                  │
│  ┌────────────────────┐      ┌──────────────────────────────┐   │
│  │  /v1/chat  route   │      │  /mcp/* routes  (Phase 3)    │   │
│  └──────────┬─────────┘      └──────────────┬───────────────┘   │
└─────────────┼──────────────────────────────┼────────────────────┘
              │                              │
              ▼                              ▼
┌─────────────────────┐        ┌────────────────────────────────┐
│  InferencePool      │        │  MCP Registry                  │
│  (LLM backends)     │        │  (tool server catalog)         │
│                     │        │                                │
│  vLLM pod A         │        │  MCP server: web-search        │
│  vLLM pod B         │        │  MCP server: code-exec         │
│  vLLM pod C         │        │  MCP server: internal-api      │
└─────────────────────┘        └────────────────────────────────┘
```

### How `AIMCPPolicy` Extends the Existing Pattern

`AIMCPPolicy` follows the same `targetRef` attachment model as the existing policies. AKO reads
the MCP registry at the configured endpoint, materialises the registered tool servers as Avi
Pool backends, and applies auth and call-budget governance to the `/mcp/*` HTTPRoute — no
manual Pool configuration required.

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIMCPPolicy
metadata:
  name: mcp-governance
  namespace: inference
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: mcp-route
  registry:
    url: "https://mcp-registry.internal/v1"   # MCP registry endpoint
    refreshInterval: 5m                        # how often AKO re-syncs the catalog
  auth:
    policyRef: llm-auth     # re-use the same AIGatewayAuthPolicy on this route
  limits:
    callBudget:
      per: consumer         # one call counter per verified sub claim
      window: 1h
      max: 500              # max tool calls per consumer per hour
      action:
        type: Reject
        statusCode: 429
        retryAfter: true
```

### What the MCP Registry Provides

| Registry capability | AI GW use |
|---|---|
| Tool server catalog (name, endpoint, schema) | GW routes `/mcp/<tool>` calls to the correct backend Pool |
| Server health / availability | GW skips unavailable servers (same health-check model as `InferencePool`) |
| Tool ACLs (which identities may call which tools) | GW enforces via the verified `sub` / `group` claim from `AIGatewayAuthPolicy` |
| Tool versioning | GW can pin routes to specific tool server versions from the registry |

### Governance Continuity — One Identity, Both Paths

Because `AIGatewayAuthPolicy` already resolves a verified identity (`sub`) and group claim from
the OIDC token, that same verified principal governs tool access — **no second authentication
hop**. An agent authenticated to call the LLM endpoint is the same identity whose tool-call
budget is enforced on the MCP route. A Phase 3 cross-resource budget will let operators express
a *unified* per-consumer spend limit that spans both token consumption (LLM) and call count
(MCP tools) within a single rolling window.

---

## Related docs

- [AI Gateway Install Guide](ai-gateway-install.md) — end-to-end demo walkthrough
- [Inference Extension](inference-extension.md) — LLM-aware load balancing via `InferencePool`
- [Inference Install Guide](inference-install.md) — end-to-end cluster setup walkthrough
