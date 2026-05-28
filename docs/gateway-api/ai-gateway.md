# AKO AI Gateway — Phase 1

## Overview

The AKO AI Gateway extension adds **JWT authentication** and **token-based rate limiting** to any HTTPRoute managed by AKO's Gateway API controller. Both capabilities are expressed as lightweight Kubernetes CRDs that attach to an HTTPRoute via a `targetRef` and are reconciled into existing Avi Service Engine features — no sidecars, no external rate-limit servers, no changes to the data-plane binary.

This feature builds directly on top of the [AKO Inference Extension](inference-extension.md). It is designed to protect and govern the same LLM endpoints that `InferencePool` load-balances.

---

## How It Works

```
┌─────────────────────────────────────────────────────┐
│                  Kubernetes Control Plane            │
│                                                      │
│  AIGatewayAuthPolicy ──────────────────────────┐    │
│  AITokenRateLimitPolicy ───────────────────────►│    │
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
│  Avi Controller                                      │
│  Virtual Service                                     │
│  ├─ SsoPolicyRef  ← JWT validation (SSO Policy)     │
│  ├─ HttpPolicySet ← claim-to-header injection        │
│  └─ DataScriptSet ← token-budget enforcement/acct   │
└─────────────────────────────────────────────────────┘
```

**Control flow:**

1. User creates `AIGatewayAuthPolicy` and/or `AITokenRateLimitPolicy` targeting an `HTTPRoute`.
2. AKO's informers detect the event, parse the CR into the `PolicyStore`, and re-enqueue the targeted HTTPRoute.
3. During `BuildChildVS`, AKO calls `ApplyAuthPolicy` and `ApplyTokenRateLimitPolicy` for each policy attached to the route.
4. The resulting Avi Virtual Service node is pushed to the Avi controller via the normal REST path.

---

## Prerequisites

- `featureGates.GatewayAPI: true` in `values.yaml`
- `aiGateway.enabled: true` in `values.yaml`
- AKO image: `ghcr.io/gricec1981/ako-gateway-api:inference-ext` (or a build from this branch)
- Install the two CRDs on the cluster before enabling the feature flag (see [Installing the CRDs](#installing-the-crds))
- For JWT auth: an **Avi SSO Policy** pre-created in the Avi controller (see [JWT Authentication](#jwt-authentication-aigatewaythauthpolicy))

---

## Enabling the Feature

Add the following to `values.yaml`:

```yaml
featureGates:
  GatewayAPI: true

aiGateway:
  enabled: true
```

The Helm chart injects `AI_GATEWAY_ENABLED=true` into the `ako-gateway-api` container and grants the necessary ClusterRole RBAC permissions for the two new CRDs automatically.

| Environment Variable | Default | Description |
|---|---|---|
| `AI_GATEWAY_ENABLED` | `false` | Master switch for AI Gateway CRD watching and policy application |

---

## Installing the CRDs

```bash
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aigatewayauthpolicies.yaml
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aitokenratelimitpolicies.yaml
```

Both CRDs are namespaced and live under the `ai.ako.vmware.com` API group, version `v1alpha1`.

---

## JWT Authentication — `AIGatewayAuthPolicy`

### What it does

- Validates the `Authorization: Bearer <token>` JWT on every inbound request using an Avi SSO Policy.
- Extracts the configured identity claim (default: `sub`) and writes it into a stable request header (`x-ai-consumer` by default) so downstream token-rate-limit and observability policies have a consistent key to work with.
- Optionally forwards additional JWT claims as request headers.

### Avi object mapping (Phase 1)

| Policy field | Avi object |
|---|---|
| `jwt.issuer` / `jwt.jwksUri` | Pre-created **SSO Policy** → `JWTServerProfile`. AKO derives the SSO policy name as `<namespace>-<name>-sso` and sets `VirtualService.SsoPolicyRef`. |
| `jwt.identityClaim` → `identityHeader` | **HTTP Request Policy** rule that copies `X-AVI-JWT-<CLAIM>` (injected by the SSO policy) into the configured header. |
| `jwt.forwardClaims` | One additional **HTTP Request Policy** rule per claim. |

> **Phase 1 note:** You must pre-create the Avi SSO Policy with the correct `JWTServerProfile` (issuer and JWKS). Full AKO-managed `JWTServerProfile` lifecycle is planned for Phase 1.5.

### Creating an SSO Policy in Avi

1. In the Avi UI: **Templates → Security → SSO Policy → Create**
2. Set **Type** to `JWT`.
3. Under **JWT Server Profile**, create a profile with:
   - **Issuer** matching `spec.jwt.issuer` in your `AIGatewayAuthPolicy`
   - **JWKS** URL or inline keys
4. Name the policy exactly `<namespace>-<policy-name>-sso` — e.g., for a policy named `llm-auth` in namespace `inference`, name the SSO policy `inference-llm-auth-sso`.

### Example

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
    issuer: "https://auth.example.com"
    jwksUri: "https://auth.example.com/.well-known/jwks.json"
    audiences:
      - "llm-api"
    identityClaim: sub          # default
    forwardClaims:
      - tenant
      - model
  identityHeader: x-ai-consumer # default
  onFailure:
    statusCode: 401             # default
```

After applying this, AKO will:
1. Set `VirtualService.SsoPolicyRef` → `/api/ssopolicy?name=inference-llm-auth-sso`
2. Add HTTP request rules to copy `X-AVI-JWT-SUB` → `x-ai-consumer`, `X-AVI-JWT-TENANT` → `tenant`, `X-AVI-JWT-MODEL` → `model`.

---

## Token Rate Limiting — `AITokenRateLimitPolicy`

### What it does

- Enforces per-consumer (or per-IP) **token budgets** over rolling time windows using OpenAI-compatible response parsing (`usage.prompt_tokens`, `usage.completion_tokens`, `usage.total_tokens`).
- Optionally enforces a **classic requests-per-second** soft rate limit.
- All enforcement runs as Lua DataScripts on the Avi Service Engine — no extra infrastructure required.

### Avi object mapping (Phase 1)

| Policy field | Avi mechanism |
|---|---|
| `limits[]` | Two **DataScript** nodes per VS: `<vsname>-ai-tok-req` (HTTP_REQ — enforce budget before forwarding) and `<vsname>-ai-tok-resp` (HTTP_RESP — parse usage and update counters). Counters are stored in the SE shared table via `avi.vs.table_insert` / `avi.vs.table_lookup`. |
| `requestRateLimit` | Soft token-bucket logic prepended to the `HTTP_REQ` DataScript using `avi.vs.table_insert`. |

> **Consistency model:** Token-budget counters are **per-SE and eventually consistent** — a consumer can briefly overshoot a budget by one request-window across scaled-out SEs before enforcement kicks in. RPS rate limiting is also soft (per-SE token bucket). Both are intentional Phase 1 trade-offs. Phase 1.5 will use Avi's native distributed rate limiter (`avi.vs.rate_limiter()`) for exact cross-SE enforcement.

### Identity resolution

By default, the policy reads the `x-ai-consumer` header set by `AIGatewayAuthPolicy`. You can override this or configure a fallback:

```yaml
identitySource:
  header: x-ai-consumer    # default
  fallback: clientIP        # "clientIP" (default) or "reject"
```

If `fallback: reject` is set and the identity header is absent, the request is rejected with HTTP 401 before any backend is contacted.

### Example

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
      key: consumer               # keyed on x-ai-consumer value
      tokens: total               # prompt + completion
      budget: 100000
      window: 1h
      action:
        type: Reject
        statusCode: 429
        retryAfter: true
    - name: daily-consumer-tokens
      key: consumer
      tokens: total
      budget: 1000000
      window: 24h
      action:
        type: Reject
        statusCode: 429
  requestRateLimit:
    requestsPerSecond: 50
    burst: 100
    key: consumer               # "consumer" or "clientIP"
```

### Token-limit actions

| `action.type` | Behaviour |
|---|---|
| `Reject` (default) | Returns the configured `statusCode` (default 429) immediately. |
| `Log` | Allows the request but logs the budget overage. Useful for shadow-mode rollout. |

When `retryAfter: true` is set, the DataScript adds a `Retry-After` header pointing to the current window boundary.

---

## Combining Auth and Rate Limiting

The two policies are independent and additive — apply both to the same HTTPRoute for the full stack:

```yaml
# 1. Authenticate and resolve consumer identity
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
    issuer: "https://auth.example.com"
    identityClaim: sub
---
# 2. Enforce token budgets keyed on the identity set above
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
  limits:
    - name: hourly-tokens
      key: consumer
      tokens: total
      budget: 100000
      window: 1h
```

AKO applies both policies during the same `BuildChildVS` reconcile cycle, in order: auth first, then rate limits. This means the identity header set by the auth policy is immediately available to the rate-limit DataScript.

---

## API Reference

### AIGatewayAuthPolicy

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.targetRef.group` | string | yes | API group of referent (`gateway.networking.k8s.io`) |
| `spec.targetRef.kind` | string | yes | `HTTPRoute` or `Gateway` |
| `spec.targetRef.name` | string | yes | Name of the referent in the same namespace |
| `spec.jwt.issuer` | string | yes | Expected `iss` claim value |
| `spec.jwt.jwksUri` | string | no | JWKS endpoint URL |
| `spec.jwt.audiences` | []string | no | Acceptable `aud` values (skip validation if empty) |
| `spec.jwt.identityClaim` | string | no | Claim to use as consumer identity. Default: `sub` |
| `spec.jwt.forwardClaims` | []string | no | Additional claims to forward as request headers |
| `spec.identityHeader` | string | no | Header for resolved identity. Default: `x-ai-consumer` |
| `spec.onFailure.statusCode` | int | no | HTTP status on JWT failure. Default: `401` |

### AITokenRateLimitPolicy

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.targetRef.*` | PolicyTargetRef | yes | Same as AIGatewayAuthPolicy |
| `spec.identitySource.header` | string | no | Identity header to read. Default: `x-ai-consumer` |
| `spec.identitySource.fallback` | string | no | `clientIP` (default) or `reject` when header is absent |
| `spec.limits[].name` | string | yes | Unique name for this limit (used as counter-key prefix) |
| `spec.limits[].key` | string | yes | Counter dimension: `consumer`, `header:<name>`, or `clientIP` |
| `spec.limits[].tokens` | string | no | `total` (default), `prompt`, or `completion` |
| `spec.limits[].budget` | int64 | yes | Maximum token count in the window |
| `spec.limits[].window` | string | yes | Time window: `30s`, `1m`, `1h`, `24h`, etc. |
| `spec.limits[].action.type` | string | no | `Reject` (default) or `Log` |
| `spec.limits[].action.statusCode` | int | no | HTTP status on rejection. Default: `429` |
| `spec.limits[].action.retryAfter` | bool | no | Add `Retry-After` header on rejection |
| `spec.requestRateLimit.requestsPerSecond` | int | yes* | Sustained RPS limit (*required when block is present) |
| `spec.requestRateLimit.burst` | int | no | Max burst above sustained rate. Default: equals RPS |
| `spec.requestRateLimit.key` | string | no | `clientIP` (default) or `consumer` |

---

## Printer columns

```bash
kubectl get aigatewayauthpolicies -n inference
# NAME        TARGET       ISSUER                        AGE
# llm-auth    llm-route    https://auth.example.com      5m

kubectl get aitokenratelimitpolicies -n inference
# NAME         TARGET       AGE
# llm-limits   llm-route    5m
```

---

## Troubleshooting

**Policy not taking effect after apply**

Check that AKO saw the event and re-enqueued the HTTPRoute:
```bash
kubectl logs -n avi-system deploy/ako-gateway-api | grep "AIGateway\|ai-tok\|SsoPolicyRef"
```

**JWT validation always failing**

Verify the Avi SSO Policy name matches exactly `<namespace>-<policy-name>-sso`. If you named your policy `inference/llm-auth`, the expected SSO policy name is `inference-llm-auth-sso`.

**Token limits not being enforced after a pod restart**

DataScript counters live in the SE process memory. They are reset when the SE restarts or the VS is re-programmed. This is a known Phase 1 limitation — persistent counters via an external store are a Phase 2 item.

**`AI_GATEWAY_ENABLED` is set but informers don't start**

Ensure the CRDs are installed before AKO starts. AKO will log a warning if it cannot list resources for the informer GVR. Install the CRDs and restart the `ako-gateway-api` pod.

---

## Phase Roadmap

| Phase | Feature | Status |
|---|---|---|
| 1 (this branch) | `AIGatewayAuthPolicy` — JWT via SSO Policy reference | ✅ Done |
| 1 (this branch) | `AITokenRateLimitPolicy` — per-SE DataScript token accounting | ✅ Done |
| 1 (this branch) | Soft RPS rate limiting (DataScript token bucket) | ✅ Done |
| 1.5 | AKO-managed `JWTServerProfile` + SSO Policy lifecycle | Planned |
| 1.5 | Native distributed rate limiter (`avi.vs.rate_limiter()`) | Planned |
| 2 | `AIObservabilityPolicy` — per-request token usage logging | Planned |
| 2 | Persistent token counters (Redis / external store) | Planned |
| 2 | mTLS / OIDC in `AIGatewayAuthPolicy` | Planned |

---

## Related docs

- [Inference Extension](inference-extension.md) — LLM-aware load balancing via `InferencePool`
- [Inference Install Guide](inference-install.md) — End-to-end cluster setup walkthrough
- [Gateway API v1](gateway-api-v1.md) — Core `HTTPRoute` / `Gateway` configuration reference
