# AI Gateway Demo — Install Guide

Step-by-step guide to demo `AIGatewayAuthPolicy` and `AITokenRateLimitPolicy` on the same
cluster already running the [Inference Extension](inference-install.md).

The demo requires **no real LLMs or GPUs**. It reuses the `mock-llm` pod pattern, which returns
OpenAI-compatible JSON and — importantly — emits the token-usage **response headers** the
AI-Gateway DataScript reads.

> **Two things that changed and matter for this guide**
> 1. **Token usage is read from response headers, not the body.** Avi DataScripts cannot read
>    the response body in the `HTTP_RESP` event, so the backend emits
>    `X-Prompt-Tokens` / `X-Completion-Tokens` / `X-Total-Tokens`. The bundled `mock-llm.yaml`
>    does this; a metrics-only mock will account 0 tokens.
> 2. **AKO manages the Avi JWT objects (Phase 1.5).** You no longer pre-create the
>    JWTServerProfile / SSO Policy. Applying an `AIGatewayAuthPolicy` makes AKO create them. You
>    only need the in-cluster `jwt-issuer` pod so AKO can fetch its JWKS.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Working inference-extension demo | Gateway, InferencePool, HTTPRoute already set up |
| AKO image with AI Gateway code | This branch; toggled by `aiGateway.enabled` (no rebuild to toggle) |
| Avi Controller reachable from AKO | For JWT auth (Part B) |

> Starting fresh? Complete [inference-install.md](inference-install.md) first, then return here.

---

## Step 1 — Enable the AI Gateway feature

```yaml
# values.yaml
featureGates:
  GatewayAPI: true
aiGateway:
  enabled: true
```

```bash
helm upgrade ako ./helm/ako -n avi-system -f values.yaml
kubectl rollout status statefulset/ako -n avi-system
```

> **No local Docker to build the image?** `Dockerfile.ako-gateway-api-dev` is self-contained
> (compiles the Go binary, then distroless), so build it remotely with no Docker daemon:
> ```bash
> az acr build -r <your-acr> -t ako-gateway-api:<tag> -f Dockerfile.ako-gateway-api-dev .
> ```

Confirm the flag (the container is **distroless** — no `env` binary — and the flag isn't logged,
so read the pod spec):

```bash
kubectl get pod ako-0 -n avi-system \
  -o jsonpath='{range .spec.containers[?(@.name=="ako-gateway-api")].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep AI_GATEWAY
# Expected: AI_GATEWAY_ENABLED=true
```

---

## Step 2 — Install the AI Gateway CRDs

```bash
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aigatewayauthpolicies.yaml
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aitokenratelimitpolicies.yaml

kubectl get crd | grep ai.ako
# aigatewayauthpolicies.ai.ako.vmware.com
# aitokenratelimitpolicies.ai.ako.vmware.com
```

---

## Step 3 — Deploy the mock LLM pods (emit token headers)

```bash
kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/mock-llm.yaml
kubectl rollout status deployment/mock-llm-1 -n inference
kubectl rollout status deployment/mock-llm-2 -n inference
```

> **⚠️ Selector + headers.** `mock-llm.yaml` labels pods `app: mock-llm`. Your `InferencePool`
> must select them — if it was created with `selector: {app: vllm}`, either label these pods
> `app: vllm` or change the pool selector to `app: mock-llm` (and let AKO re-resolve). The mock
> emits the `X-*-Tokens` response headers the DataScript needs.

Verify the endpoint and headers:

```bash
kubectl port-forward -n inference deployment/mock-llm-1 8000:8000 &
PF_PID=$!
curl -si -X POST http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}' \
  | grep -iE 'X-(Prompt|Completion|Total)-Tokens|total_tokens'
kill $PF_PID
```

Expected: `X-Total-Tokens: 100` (and a `usage` block with `total_tokens: 100`). Tune with the
`PROMPT_TOKENS` / `COMPLETION_TOKENS` env vars on the Deployment.

---

## Part A — Token rate limiting (no JWT)

A simple per-IP token budget — no auth required. Apply a flat-budget policy:

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AITokenRateLimitPolicy
metadata:
  name: llm-limits-flat
  namespace: inference
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route }
  identitySource: { header: x-ai-consumer, fallback: clientIP }
  limits:
    - name: hourly-tokens
      key: consumer        # falls back to clientIP when no identity header/JWT
      tokens: total
      budget: 500          # 5 x 100-token requests
      window: 1h
      action: { type: Reject, statusCode: 429, retryAfter: true }
EOF
```

Confirm AKO attached the DataScripts:

```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "registered token-accounting DataScripts"
# AITokenRateLimitPolicy inference/llm-limits-flat: registered token-accounting DataScripts on VS ...
```

In the Avi UI: **Applications → Virtual Services → <llm-route VS> → DataScript** shows
`<vsname>-ai-tok-req` (HTTP_REQ) and `<vsname>-ai-tok-resp` (HTTP_RESP).

### Run it

```bash
VIP=$(kubectl get gateway avi-gateway -n inference -o jsonpath='{.status.addresses[0].value}')

# 5 succeed (100 tokens each, budget 500), 6th is rejected
for i in $(seq 1 6); do
  curl -s -o /dev/null -w "req $i: HTTP %{http_code}\n" \
    -X POST "http://${VIP}/v1/chat/completions" -H "Content-Type: application/json" \
    -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}'
done
```

Expected: `req 1..5: HTTP 200`, `req 6: HTTP 429` with body
`{"error":"token_budget_exceeded","limit":"hourly-tokens","current":500,"budget":500}`.

> The VIP is on the Avi VIP network and may be private (e.g. `10.225.0.100`). If you can't reach
> it from your workstation, run the curls from an in-cluster pod that shares the VNet.

Remove the flat policy before Part B so the two don't both attach:

```bash
kubectl delete aitokenratelimitpolicy llm-limits-flat -n inference
```

---

## Part B — JWT auth + per-group token budgets

Demonstrates: requests without a token → 401; with a valid token → identity + group come from
the JWT; **group1 users get 500 tokens/hr, group2 users get 1000**; unknown groups → 403.

### B1. Deploy the in-cluster JWT issuer

The issuer self-generates an RSA key, signs RS256 JWTs (with a `group` claim), and serves JWKS.
It is a **ClusterIP** service — the Avi Controller reaches it over the pod network and AKO fetches
its JWKS in-cluster, so no public IP is needed.

```bash
# RSA signing key (any RSA-2048 key works)
openssl genrsa -out /tmp/key.pem 2048
kubectl create secret generic jwt-signing-key -n inference --from-file=key.pem=/tmp/key.pem

kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/jwt-issuer.yaml
kubectl rollout status deployment/jwt-issuer -n inference
```

### B2. Apply the auth + group-budget policies

```bash
kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/ai-gateway-policies.yaml
```

This applies an `AIGatewayAuthPolicy` (issuer = `jwt-issuer.inference.svc.cluster.local:8080`) and
the group-budget `AITokenRateLimitPolicy`. AKO then **creates the Avi objects automatically**:

```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep -E "JWTServerProfile|SSOPolicy|SsoPolicyRef"
# JWTServerProfile inference-llm-auth-jwt created
# SSOPolicy inference-llm-auth-sso created
# AIGatewayAuthPolicy inference/llm-auth: set SsoPolicyRef -> inference-llm-auth-sso (audience=llm-api)
```

In the Avi UI they appear under **Templates → Security → SSO Policies** (`inference-llm-auth-sso`)
and **JWT Server Profiles** (`inference-llm-auth-jwt`).

### B3. Run it

```bash
ISS=http://jwt-issuer.inference.svc.cluster.local:8080   # run these from an in-cluster pod
tok() { curl -sf "$ISS/token?sub=$1&group=$2" | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])"; }
hit() { curl -s -o /dev/null -w "%{http_code}" -X POST "http://${VIP}/v1/chat/completions" \
          -H "Authorization: Bearer $1" -H "Content-Type: application/json" -d '{}'; }

# no token -> 401
curl -s -o /dev/null -w "no token: HTTP %{http_code}\n" -X POST "http://${VIP}/v1/chat/completions" -d '{}'

# alice is group1 (budget 500 = 5 requests, 6th -> 429)
A=$(tok alice group1); for i in $(seq 1 6); do echo "alice req $i: $(hit $A)"; done

# bob is group2 (budget 1000 = 10 requests, 11th -> 429)
B=$(tok bob group2);   for i in $(seq 1 11); do echo "bob req $i: $(hit $B)"; done

# carol is also group1 but an INDEPENDENT counter -> 200
C=$(tok carol group1); echo "carol req 1: $(hit $C)"

# dave's group isn't in groupBudgets -> 403 unknown_group
D=$(tok dave admin);   echo "dave req 1: $(hit $D)"
```

Expected: alice 5×200 then 429; bob 10×200 then 429; carol 200 (fresh counter); dave 403.

---

## Tuning the demo

| Goal | How |
|---|---|
| Faster budget exhaustion | Lower `budget` (or raise `PROMPT_TOKENS`/`COMPLETION_TOKENS` on the mock) |
| Per-IP limiting | `key: clientIP` |
| Log-only (shadow) mode | `action.type: Log` |
| RPS throttle | uncomment `requestRateLimit` in `ai-gateway-policies.yaml` |
| Change group budgets | edit `groupBudgets` (e.g. add `group3: 2000`) and re-apply |

---

## Cleanup

```bash
kubectl delete aigatewayauthpolicy llm-auth -n inference        # AKO deletes the Avi SSO/JWT objects
kubectl delete aitokenratelimitpolicy llm-limits -n inference
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/jwt-issuer.yaml
kubectl delete secret jwt-signing-key -n inference
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/mock-llm.yaml
```

---

## Troubleshooting

**DataScripts don't appear on the VS** — confirm `AI_GATEWAY_ENABLED=true` (Step 1; the
container is distroless so read the pod spec, not `kubectl exec -- env`).

**5xx / "no available servers"** — the backend pods are down or not selected by the
InferencePool. If you're running a large scale test, many InferencePools all scraping the same
few single-threaded mock pods can saturate them and fail health checks — scale the mock down or
add replicas.

**Token limits never fire** — the backend isn't emitting `X-Total-Tokens` (or the prompt/
completion pair). The DataScript reads usage from those headers, not the JSON body.

**429/403 wrong under JWT** — confirm the issued token carries the claim named in `groupHeader`
and that its value is a key in `groupBudgets`. The DataScript decodes the claim from the bearer
token; check the AKO logs for `SsoPolicyRef` to confirm auth is wired.

**JWT always 401** — confirm AKO created the SSO objects (`grep JWTServerProfile` in the logs)
and that AKO could reach `jwksUri` to fetch the keys (the JWTServerProfile's `jwks_keys` must be
populated).
