# AI Gateway Demo — Install Guide

Step-by-step guide to demo `AIGatewayAuthPolicy` and `AITokenRateLimitPolicy` on
the same cluster already running the [Inference Extension](inference-install.md).

The demo requires **no real LLMs or GPUs**. It reuses the same `mock-llm` pod pattern
from the inference extension demo, extended to return OpenAI-compatible JSON responses
with token-usage fields that the DataScript can parse.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Working inference-extension demo | Gateway, InferencePool, HTTPRoute already set up |
| AKO image from `feature/auth-rate-limiting` | Rebuilt with `AI_GATEWAY_ENABLED` support |
| `openssl` | For RSA keypair generation (Part B) |
| Avi Controller access | Required only for Part B (JWT auth) |

> If you're starting fresh, complete [inference-install.md](inference-install.md) first through Step 9, then return here.

---

## Step 1 — Rebuild and push AKO with AI Gateway enabled

The `feature/auth-rate-limiting` branch includes the AI Gateway code. Rebuild the image:

```bash
cd ~/load-balancer-and-ingress-services-for-kubernetes

make dev-build-and-push-gateway-api \
  REGISTRY=ghcr.io/gricec1981 \
  TAG=inference-ext
```

> **No local Docker?** `Dockerfile.ako-gateway-api-dev` is self-contained (compiles the
> Go binary in a `golang` builder, then distroless), so you can build it remotely in an
> Azure Container Registry with no Docker daemon:
> ```bash
> az acr build -r <your-acr> -t ako-gateway-api:inference-ext -f Dockerfile.ako-gateway-api-dev .
> ```

Then enable the feature in `values-inference-dev.yaml` (or your equivalent values file):

```yaml
aiGateway:
  enabled: true
```

Upgrade AKO:

```bash
helm upgrade ako ./helm/ako \
  -n avi-system \
  -f helm/ako/values-inference-dev.yaml
kubectl rollout status statefulset/ako -n avi-system
```

Confirm the feature is on. The `ako-gateway-api` container is **distroless** (no shell, no
`env` binary), and AKO does **not** log the flag — so read it from the pod spec:

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

## Step 3 — Deploy the extended mock LLM pods

The existing inference demo uses pods that only serve `GET /` (Prometheus metrics).
Replace them with the extended version that also handles `POST /v1/chat/completions`:

```bash
kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/mock-llm.yaml
kubectl rollout status deployment/mock-llm-1 -n inference
kubectl rollout status deployment/mock-llm-2 -n inference
```

> **⚠️ Make sure your InferencePool actually selects these pods.** `mock-llm.yaml` labels its
> pods `app: mock-llm`. If your `InferencePool` was created with `selector: {app: vllm}` (as in
> the inference demo), it will match **zero** of these pods and the route will have no backend —
> requests return a 5xx and the token DataScript never sees a `usage` block to account. Either
> deploy these with `app: vllm`, or update the InferencePool selector to `app: mock-llm` and let
> AKO re-resolve.
>
> The backend **must** return an OpenAI `usage` block on `POST /v1/chat/completions` — the
> token-accounting DataScript parses `usage.total_tokens` from the response body. A mock that
> only emits Prometheus `/metrics` (no `usage` JSON) will account **0 tokens**, so Part A never
> reaches the 429. `mock-llm.yaml` includes the `usage` block; a metrics-only mock does not.

Test that the OpenAI endpoint works:

```bash
# Port-forward directly to a pod to bypass the VIP.
kubectl port-forward -n inference deployment/mock-llm-1 8000:8000 &
PF_PID=$!

curl -s -X POST http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}' \
  | python3 -m json.tool

kill $PF_PID
```

Expected output includes:
```json
{
  "usage": {
    "prompt_tokens": 25,
    "completion_tokens": 75,
    "total_tokens": 100
  }
}
```

> **Tip:** Change `PROMPT_TOKENS` / `COMPLETION_TOKENS` env vars on the Deployment to
> adjust how many tokens each response "uses". The DataScript reads these from the
> live response body, not from a configmap.

---

## Step 4 (Part A) — Token Rate Limiting demo (no Avi pre-setup)

This part works independently of JWT auth. Apply the rate-limit policy only:

```bash
kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/ai-gateway-policies.yaml \
  -l demo-part=token-limits
```

AKO will detect the new `AITokenRateLimitPolicy`, re-enqueue the `llm-route` HTTPRoute,
and attach two DataScripts to the child Virtual Service. Confirm in AKO logs:

```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "AITokenRate\|DataScript"
# Expected: "AITokenRateLimitPolicy inference/llm-limits: registered token-accounting DataScripts on VS ..."
```

Confirm the DataScripts appear in the Avi UI:

```
Applications → Virtual Services → <llm-route VS> → DataScript tab
  <vsname>-ai-tok-req    (HTTP_REQ phase)
  <vsname>-ai-tok-resp   (HTTP_RESP phase)
```

---

## Step 5 (Part A) — Run the token rate limit demo

Get the Gateway VIP:

```bash
VIP=$(kubectl get gateway llm-gateway -n inference \
  -o jsonpath='{.status.addresses[0].value}')
echo "VIP: $VIP"
```

Send 5 successful requests (each uses 100 tokens, budget = 500):

```bash
for i in 1 2 3 4 5; do
  echo "── request $i ──"
  curl -s -X POST "http://${VIP}/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}' \
    | python3 -c "
import sys, json
r = json.load(sys.stdin)
u = r['usage']
print(f'  tokens: {u[\"total_tokens\"]}  model: {r[\"model\"]}')
"
done
```

Now send the 6th — it should be rejected:

```bash
echo "── request 6 (should fail) ──"
curl -s -w "\nHTTP %{http_code}" \
  -X POST "http://${VIP}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}'
```

Expected:
```json
{"error":"token_budget_exceeded","limit":"hourly-consumer-tokens","current":500,"budget":500}
HTTP 429
```

**Show per-consumer isolation** — add an `x-ai-consumer` header to get a fresh counter:

```bash
curl -s -w "\nHTTP %{http_code}" \
  -X POST "http://${VIP}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "x-ai-consumer: alice" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}'
# Returns 200 — alice has her own fresh 500-token budget
```

---

## Step 6 (Part B) — JWT Authentication demo

Part B requires the Avi Controller to validate JWTs. Run the full setup:

```bash
export AVI_CONTROLLER=10.x.x.x   # your Avi Controller IP
export AVI_USERNAME=admin
export AVI_PASSWORD=yourpassword

./docs/gateway-api/examples/ai-gateway-demo/setup.sh
```

`setup.sh` will:
1. Generate an RSA-2048 keypair and store it in a Kubernetes Secret
2. Deploy the `jwt-issuer` pod (Python server that signs RS256 JWTs and serves JWKS)
3. Wait for the `jwt-issuer` LoadBalancer IP
4. Create an Avi `JWTServerProfile` pointing at `/jwks` on the issuer
5. Create an Avi `SSO Policy` (`inference-llm-auth-sso`) referencing the JWT profile
6. Apply the `AIGatewayAuthPolicy`

Confirm in Avi UI:
```
Templates → Security → SSO Policies → inference-llm-auth-sso
Templates → Security → JWT Server Profiles → inference-llm-auth-jwt
```

Confirm in AKO logs:
```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "SsoPolicyRef\|AIGatewayAuth"
# Expected: "AIGatewayAuthPolicy inference/llm-auth: set SsoPolicyRef → inference-llm-auth-sso"
```

---

## Step 7 (Part B) — Run the JWT auth demo

```bash
ISSUER_VIP=$(kubectl get svc jwt-issuer -n inference \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# 1. Request without token → 401
echo "── no token ──"
curl -s -w "\nHTTP %{http_code}" \
  -X POST "http://${VIP}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}'

# 2. Get a valid token
TOKEN=$(curl -sf "http://${ISSUER_VIP}:8080/token?sub=alice&tenant=acme&model=mock-llm-v1" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "Token: ${TOKEN:0:60}..."

# 3. Request with valid token → 200
echo "── valid token ──"
curl -s -w "\nHTTP %{http_code}" \
  -X POST "http://${VIP}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}'
```

Expected:
- No token → `HTTP 401`
- Valid token → `HTTP 200`, and the Avi VS sets `x-ai-consumer: alice` as a request header

---

## Step 8 — Use the automated demo script

For a full scripted walkthrough with pauses and narration:

```bash
export VIP=<gateway-external-ip>
./docs/gateway-api/examples/ai-gateway-demo/demo.sh
```

The script covers:
- Part A: 5 successful requests → 6th hits 429, per-consumer isolation
- Part B: no-token 401 → fetch token → 200 → alice's JWT-keyed counter fills up → bob's counter is independent

---

## Tuning the demo

| Goal | How |
|---|---|
| Faster budget exhaustion | Reduce `budget` in `ai-gateway-policies.yaml` (e.g., `budget: 200`) |
| Bigger responses | Increase `COMPLETION_TOKENS` on the Deployment (e.g., `"200"`) |
| Simulate load imbalance | `kubectl set env deployment/mock-llm-1 -n inference WAITING=20 KV_CACHE=0.8` (same as inference extension demo) |
| Per-IP limiting | Change `key: consumer` → `key: clientIP` in the policy |
| Log-only mode | Change `action.type: Reject` → `action.type: Log` to count but not block |
| RPS throttle | Uncomment `requestRateLimit` block in `ai-gateway-policies.yaml` |

---

## Cleanup

```bash
# Remove policies (leaves mock LLMs and gateway running)
kubectl delete aigatewayauthpolicy llm-auth -n inference
kubectl delete aitokenratelimitpolicy llm-limits -n inference

# Remove jwt-issuer
kubectl delete deployment jwt-issuer -n inference
kubectl delete service jwt-issuer -n inference
kubectl delete secret jwt-signing-key -n inference
kubectl delete configmap jwt-issuer-script -n inference

# Remove extended mock LLMs (if you want to go back to the original inference demo)
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/mock-llm.yaml
```

---

## Troubleshooting

**DataScripts don't appear on the VS**

Check that `AI_GATEWAY_ENABLED=true` is set on the gateway-api container. The container is
distroless (no `env` binary), so `kubectl exec … -- env` will fail — read the pod spec instead:
```bash
kubectl get pod ako-0 -n avi-system \
  -o jsonpath='{range .spec.containers[?(@.name=="ako-gateway-api")].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep AI_GATEWAY
```

**429 never fires even after many requests**

The DataScript counter is per-SE. If you're hitting different SEs (scaled-out), the per-SE counter fills up more slowly. To force a single SE for demo: set the SE group to 1 SE max or check you're always hitting the same SE.

**401 even with a valid token**

The Avi SSO Policy name must be exactly `<namespace>-<policy-name>-sso`. Verify in the Avi UI that the SSO Policy exists and is named `inference-llm-auth-sso`. Also check the `issuer` field in the policy matches `spec.jwt.issuer` in `AIGatewayAuthPolicy`.

**jwt-issuer pod not starting**

```bash
kubectl logs -n inference deployment/jwt-issuer -c pip-install
kubectl logs -n inference deployment/jwt-issuer -c jwt-issuer
```
The init container installs `cryptography` and `PyJWT` via pip. If the cluster has no internet access, you'll need to use a private registry image with these pre-installed.
