# AKO AI Gateway

> **Scope note.** This document is the reference for the **two foundation policies** —
> `AIGatewayAuthPolicy` (authentication) and `AITokenRateLimitPolicy` (token budgets). Everything
> built on top of them has its own document: model/tier routing
> ([model-routing.md](model-routing.md)), tool governance ([ai-gateway-mcp.md](ai-gateway-mcp.md)),
> agent-to-agent governance ([ai-gateway-a2a.md](ai-gateway-a2a.md)), guardrails
> ([ai-gateway-guardrails.md](ai-gateway-guardrails.md)) and consumption accounting
> ([ai-gateway-token-ledger.md](ai-gateway-token-ledger.md)). For the whole system in one place,
> read the [Handbook](ai-gateway-handbook.md); for what shipped when, the
> [release notes](ai-gateway-release-notes.md).

## Overview

The AKO AI Gateway extension adds **OAuth/OIDC authentication** and **token-based rate limiting**
(including **per-group token budgets**) to any HTTPRoute managed by AKO's Gateway API
controller. Both capabilities are expressed as lightweight Kubernetes CRDs that attach to an
HTTPRoute via a `targetRef` and are reconciled into existing Avi Service Engine features — no
sidecars, no external rate-limit servers, no changes to the data-plane binary.

This feature builds directly on top of the [AKO Inference Extension](inference-extension.md).
It is designed to protect and govern the same LLM endpoints that `InferencePool` load-balances.

The "single control plane for the entire agent execution loop" this document once described as a
Phase 3 ambition is **built**: tool calls are governed by
[`AIMCPRoutePolicy`](ai-gateway-mcp.md) and agent↔agent delegation by
[`AIA2ARoutePolicy`](ai-gateway-a2a.md), both under the same verified identity and the same
budgets as inference. The CRD is named `AIMCPRoutePolicy`, not the `AIMCPPolicy` sketched below.

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
- Supports **per-group budgets** (e.g. engineering = 500 tokens/hr, product-management = 1000 tokens/hr) keyed on
  a JWT claim.
- Optionally enforces a **classic requests-per-second** rate limit using the **native Avi rate
  limiter** (see below).
- All enforcement runs as Lua DataScripts on the Avi Service Engine — no extra infrastructure.

### Request rate limiting — native Avi rate limiter (`requestRateLimit`)

`requestRateLimit` is enforced by the **native Avi dynamic rate limiter**, not a hand-rolled
counter. AKO publishes a `RateLimiter` (count / period / burst) on the request-phase
`VSDataScriptSet` and the `HTTP_REQ` DataScript calls:

```lua
avi.vs.ratelimit.exceed("<vs>-ai-rps", rk)   -- rk = consumer identity, else client IP
```

The Service Engine owns the per-`request_key` token bucket and keeps it **consistent across
Virtual Service scale-out** (distributed; patent US10182057B1) — so a `requestsPerSecond` of 10 is
~10 RPS for the whole VS, not 10× the number of SEs. `key: consumer` buckets per resolved
identity (the `x-ai-consumer` header set by `AIGatewayAuthPolicy`, falling back to the client IP);
`key: clientIP` (default) buckets per source IP. `burst` maps to the limiter's burst size and
defaults to `requestsPerSecond`.

> This replaces the earlier per-SE soft token bucket (a `table_lookup` + remove-then-insert in
> Lua), which counted independently on each SE and so over-admitted under VS scale-out. The CRD
> fields are unchanged. **Upgrade note:** on a scaled-out VS the *effective* limit tightens from
> roughly `requestsPerSecond × number-of-SEs` to `requestsPerSecond`; size the value for the whole
> VS. Verify behavior on your target SE build (32.x) before relying on it in production.
> **Token budgets can be native too.** Each `limits[]` entry takes `backend: datascript`
> (default — per-SE shared-state counter, fixed calendar window, feeds the admin counters
> endpoint) or `backend: native` — the Avi rate limiter (`avi.vs.ratelimit.exceed`), exact
> across SEs, enforced by a *deferred carry* charged at the gate, with the DataScript table
> kept for display only. `llm-limits` on the lab front door runs `native`. Limits keyed on a
> reqvar (`groupHeader: reqvar:ai_tier`) stay on the DataScript path. Design and the live
> findings that shaped it: [native-token-budget-design.md](native-token-budget-design.md).
> section).

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
  and no accidental parsing of non-completion bodies. **Caveat:** this also means streamed
  responses are *not metered* — see the streaming limitation below.
- **Fail-closed on unreadable usage.** If a buffered JSON completion (it has the
  `"object":"chat.completion"` / `"choices"` markers, which survive a tail truncation) yields no
  parseable `usage` — because the body was larger than the buffer, or compressed — the script
  charges a conservative penalty (`FailClosedTokens`, ~`RespBodyBufferKB*1024/4`) instead of 0,
  so the consumer's **next** request is blocked rather than letting an over-buffer response slip
  through the budget unmetered.

> **Streaming is not metered today (known limitation).** `usage` sits at the *end* of the body,
> so metering requires the SE to buffer the whole response — controlled by `RespBodyBufferKB`
> (default 256 KB ≈ ~40K output tokens; roughly `max_tokens * 6` bytes). That store-and-forward
> model suits non-streaming JSON traffic, but for **streaming** (`stream:true` /
> `text/event-stream`) it's a dead end: the SE's `HTTP_RESP_DATA` event is **buffer-complete**
> (verified by probe) — reading the body forces full buffering, which collapses token-by-token
> delivery. So streamed responses are currently **not metered — they bypass the budget (count 0)**;
> use non-streaming where budgets must hold.
>
> The fix belongs **in the SE, not a sidecar** — the gateway is the only proxy in the path. The SE
> already relays every chunk; it needs a *tap*: either a native per-chunk response-body event
> (so the existing DataScript meters streaming), or native LLM token metering that inspects the
> live stream and writes to a **distributed counter** (which also makes budgets consistent across
> a multi-SE / multi-cluster fabric). See the
> [release notes](ai-gateway-release-notes.md#known-limitations).
>
> *Memory note:* the buffered body is held per in-flight metered (non-streaming) response, so very
> high concurrency × large responses competes with SE connection capacity.

### Avi object mapping

| Policy field | Avi mechanism |
|---|---|
| `limits[]` | Three **DataScript** nodes per VS: `<vsname>-ai-tok-req` (HTTP_REQ — enforce budget), `<vsname>-ai-tok-resp` (HTTP_RESP — enable response-body buffering), and `<vsname>-ai-tok-respdata` (HTTP_RESP_DATA — parse `usage` from the body and update counters). |
| `requestRateLimit` | A native Avi `RateLimiter` (count/period/burst) on the `<vsname>-ai-tok-req` `VSDataScriptSet` (`rate_limiters`), referenced from the `HTTP_REQ` DataScript via `avi.vs.ratelimit.exceed("<vsname>-ai-rps", request_key)`. The SE owns the per-`request_key` token bucket. |

Token-budget counters are stored in the Avi VS string table (`avi.vs.table_lookup` /
`table_remove` / `table_insert`), which Avi replicates across all SEs hosting the VS. The
`requestRateLimit` bucket, by contrast, is owned by the native rate limiter.

> **Consistency model:** with `backend: native` (the LLM front door today) a budget is
> enforced by the Avi rate limiter — **exact across scaled-out SEs**, on a rolling
> token-bucket window rather than a calendar reset. With the default `backend: datascript`
> the counter is per-SE and eventually consistent: a consumer can briefly overshoot by about
> one request-window. In both modes the number the console *displays* is the per-SE table,
> so it is approximate even where enforcement is exact. RPS limiting (`requestRateLimit`) is
> native in both modes.

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
ceiling* is looked up from the user's group claim. Users in `engineering` get 500 tokens/hour; users
in `product-management` get 1000. Requests whose group is not listed are rejected with HTTP 403 (set a
positive `budget` to instead use it as the fallback ceiling for unknown groups).

```yaml
spec:
  limits:
    - name: hourly-group-budget
      key: consumer            # per-user counter
      groupHeader: group       # JWT claim (decoded from the token) that selects the budget
      groupBudgets:
        engineering: 500
        product-management: 1000
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

### Dashboard counters endpoint

For a UI/dashboard to display live per-user usage, AKO can expose a **read-only counters
endpoint** on the same VS, gated by an admin token. It is opt-in: set the
`ai.ako.vmware.com/admin-token-secret` annotation on the `AITokenRateLimitPolicy` to the
name of a Secret (key `token`) in the policy namespace.

```bash
kubectl create secret generic ai-admin-token -n inference \
  --from-literal=token=$(openssl rand -hex 16)
kubectl annotate aitokenratelimitpolicy llm-limits -n inference \
  ai.ako.vmware.com/admin-token-secret=ai-admin-token
```

AKO then prepends a branch to the request DataScript (before enforcement):

- `GET /v1/admin/counters?users=alice,bob,carol` with header `X-Admin-Token: <token>`
  → `200 {"window":<epoch-sec>,"limit":"<name>","counters":[{"user":"alice","used":300}, …]}`
- Missing / wrong token → `403 {"error":"forbidden"}`

The SE counter table has no enumeration API, so the **caller passes the identities it wants**
(`?users=`). Each is looked up at the *same* key the response-phase accounting writes
(`<epoch>:<limit>:<user>:<window>`), so the values are exactly what enforcement sees. When an
`AIGatewayAuthPolicy` is attached, AKO adds an SSO `SKIP_AUTHENTICATION` rule for `/v1/admin/`
so the endpoint is reachable without the OAuth browser redirect — the `X-Admin-Token` check is
the gate. (The demo OIDC issuer exposes its identity→group roster at `GET /users` so a UI knows
which users to query.)

### Resetting counters

Bump the **`ai.ako.vmware.com/counter-epoch`** annotation. AKO folds the epoch into every
counter key, so changing it moves all of the policy's counters to a fresh keyspace — an instant
reset that leaves budgets and every other field untouched (unlike renaming a limit):

```bash
kubectl annotate aitokenratelimitpolicy llm-limits -n inference \
  ai.ako.vmware.com/counter-epoch=2 --overwrite
```

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
      groupBudgets: { engineering: 500, product-management: 1000 }
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
| `spec.limits[].groupHeader` | string | no | Selector whose value picks a per-group budget. A verified JWT claim name (e.g. `group`), or `reqvar:<name>` to read a request-scoped variable — e.g. `reqvar:ai_tier` for **per-tier budgets** set by an [`AIModelRoutePolicy`](model-routing.md). A `reqvar:` limit is enforced in `HTTP_REQ_DATA` (after the variable is set), not `HTTP_REQ`. |
| `spec.limits[].groupBudgets` | map[string]int64 | no | Group/tier value → budget (e.g. `{engineering: 500, product-management: 1000}` or `{premium: 500, economy: 100000}`). Requires `groupHeader` |
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
| 1 | RPS rate limiting — now on the **native Avi rate limiter** (`avi.vs.ratelimit.exceed`), replacing the per-SE DataScript token bucket (2026-06-16) | ✅ Done |
| 1.5 | Per-group token budgets (`groupHeader` / `groupBudgets`) | ✅ Done |
| 2 | `AIGatewayAuthPolicy` — OAuth/OIDC auth, AKO-managed `Pool` + `AuthProfile` + `SSOPolicy` lifecycle | ✅ Done |
| 2 | Verified claims in the DataScript via `oauth_get_claim` | ✅ Done |
| 2.x | [`AIModelRoutePolicy`](model-routing.md) — route by request-body `model` to per-tier `InferencePool` backends (`avi.poolgroup.select`), group entitlement, per-tier token budgets | ✅ Done |
| 2.5 | **Native token budgets** — `limits[].backend: native`: Avi RateLimiter objects + deferred carry, exact across SEs; DataScript table kept for display (2026-06-17, live on `llm-limits`) | ✅ Done |
| 2.5 | `AIObservabilityPolicy` — per-request token usage logging | ✅ Superseded and delivered as the [token ledger](ai-gateway-token-ledger.md): one immutable usage record per metered response, drained into a durable store |
| 3 | MCP tool governance | ✅ Done — shipped as [`AIMCPRoutePolicy`](ai-gateway-mcp.md), not the `AIMCPPolicy` sketched below |
| 3 | MCP registry integration — the gateway brokers only servers on an approved list | ✅ Done — `mcp-registry` ConfigMap, enforced by the factory and the console |
| 3 | A2A (agent↔agent) governance | ✅ Done — [`AIA2ARoutePolicy`](ai-gateway-a2a.md) |
| 3 | Guardrails / DLP on the SE's native WAF, plus a semantic layer over ICAP | ✅ Done — [`AIGuardrailPolicy`](ai-gateway-guardrails.md) |
| 3 | Cross-resource budget — unified per-consumer spend limit spanning token consumption (LLM) and call count (MCP tools) in a single rolling window | Planned |

---

## MCP Tool Governance (Phase 3 — superseded)

> **Superseded.** MCP governance shipped as
> **[`AIMCPRoutePolicy`](ai-gateway-mcp.md)** — a dedicated MCP gateway with its own VIP, the
> native Avi 32.1.1 MCP application profile, `Mcp-Session-Id` affinity and per-role tool
> authorization. The section below is the original sketch, kept because its framing of *why*
> tool calls need governing still reads well. For anything you intend to configure, use
> [ai-gateway-mcp.md](ai-gateway-mcp.md); the CRD shape described here was never built.

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
- [AI Gateway Release Notes](ai-gateway-release-notes.md) — features, fixes, and known limitations
- [Model-Based Routing](model-routing.md) — `AIModelRoutePolicy`: route by the request `model` to per-tier (quality/cost) `InferencePool` backends, with per-tier token budgets
- [Inference Extension](inference-extension.md) — LLM-aware load balancing via `InferencePool`
- [Inference Install Guide](inference-install.md) — end-to-end cluster setup walkthrough
