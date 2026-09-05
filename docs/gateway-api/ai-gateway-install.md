# AKO AI Gateway — Install Guide

Step-by-step guide to install AKO with Gateway API and AI Gateway enabled, and verify the
**full AI Gateway feature set** — authentication, token-budget rate limiting, model-tier
routing, MCP tool governance, A2A agent-to-agent governance, and WAF-based guardrails —
starting from a **fresh Kubernetes cluster with nothing pre-installed**: no Gateway API CRDs,
no AKO, no Gateway/InferencePool/HTTPRoute. This one file is everything you need, start to
finish. It also installs a console UI (Step 6) that drives most of the policy-creation steps
below through forms instead of `kubectl apply`.

The guide needs **no real LLMs or GPUs**. It reuses the `mock-llm` pod pattern, which returns
OpenAI-compatible JSON and — importantly — the token-usage `usage` block in the response
**body** the AI-Gateway DataScripts read.

> **Three things that matter for this guide**
> 1. **Token usage is parsed from the response body.** The DataScript reads the OpenAI `usage`
>    block straight from the JSON body in the `HTTP_RESP_DATA` event (buffered via
>    `set_response_body_buffer_size`) — so it works with **stock vLLM**, no token headers or
>    sidecar required. The SE buffers the body, which covers non-streaming traffic; meter
>    streaming responses in a proxy instead.
> 2. **Auth is `jwtQuery`, and AKO manages the Avi objects.** Applying an `AIGatewayAuthPolicy`
>    with `authMode: jwtQuery` makes AKO build a `JWTServerProfile` + `AUTH_PROFILE_JWT` +
>    `SSO_TYPE_JWT` policy and set `jwt_config` on the child VS directly — **no issuer Pool, no
>    OAuth session, no browser redirect.** Every downstream policy (token budgets, model-tier
>    entitlements, MCP tool RBAC, A2A agent RBAC) reads claims straight off the same
>    SE-validated token, via the same `jwt_claim()` DataScript helper.
> 3. **The token still rides in the URL, and the listener still needs TLS.** `jwtQuery` drops
>    the OAuth callback path, session cookie, and browser redirect — but the SE validates the
>    JWT from a `?jwt=` query parameter, so it travels encrypted. Keep tokens short-lived.
>    See [ai-gateway-auth.md](ai-gateway-auth.md#security-token-in-url).

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes cluster | 1.28+ recommended |
| Helm 3 | `brew install helm` |
| kubectl | configured and pointing at your cluster |
| Docker, or `az acr build` | Needed to build the `ako-gateway-api` image in Step 2 — locally via Docker, or remotely via ACR (no local Docker needed on that path) |
| Avi Controller, Cloud, and Service Engine Group already exist in Avi | This is a **hard prerequisite nothing in this guide can script around** — create them in the Avi Controller itself before starting |
| Avi Controller reachable from AKO | AKO builds every AI Gateway object (JWT/WAF/Pool Group/DataScript) over the Avi REST API |
| HTTPS Gateway listener + TLS cert | **Both** `AIGatewayAuthPolicy` modes — `oauthBrowser` and `jwtQuery` — require the Gateway listener to terminate TLS. `jwtQuery` removes the OAuth callback/session/cookie-jar machinery, **not** the TLS requirement itself. |
| Issuer reachable **from AKO**, not from the SE | Under `jwtQuery`, AKO fetches the JWKS itself and embeds it in a `JWTServerProfile` — no issuer Pool, no SE-side reachability needed. (Contrast with `oauthBrowser`, where the SE reaches the issuer at runtime through an AKO-built Pool.) |
| Avi Controller ≥ 32.1.1 | Only needed for the [MCP](#mcp--aimcproutepolicy) section — the native MCP application profile and session DataScript are 32.1.1+ objects. |

This guide also turns on `inferenceExtension` in Step 4 — the Prometheus-scraping controller
that auto-adjusts Avi Pool Group member weights across an InferencePool's pods. See [Inference
Extension — weight-based load balancing](#inference-extension--weight-based-load-balancing) after
Step 8.

---

## Step 1 — Get the code

```bash
git clone git@github-vcf.devops.broadcom.net:ANS/AI-Gateway-chris.git
cd AI-Gateway-chris
git checkout feature/ai-a2a-gateway
```

## Step 2 — Build and push the `ako-gateway-api` image

`Dockerfile.ako-gateway-api-dev` is self-contained (compiles the Go binary, then distroless).
Pick whichever build path matches your setup:

**A — No local Docker (build remotely, lands directly in your registry):**

```bash
az acr build -r <your-acr-name> -t ako-gateway-api:<tag> -f Dockerfile.ako-gateway-api-dev .
```

**B — Local Docker:**

```bash
make dev-build-and-push-gateway-api REGISTRY=<your-registry> TAG=<tag>
# builds `dev-docker-gateway-api` then pushes with `dev-push-gateway-api`;
# both default to REGISTRY=ghcr.io/gricec1981 TAG=inference-ext if you omit them.
```

Either way, note the `<registry>/ako-gateway-api:<tag>` you end up with — you need it in the next step.

## Step 3 — Install the CRDs

Three sets of CRDs need to exist before anything else works: the base Gateway API CRDs, the
InferencePool CRD, and the six AI Gateway policy CRDs. None of them depend on AKO being
installed, so apply all of them now.

### Gateway API CRDs

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml
```

Verify:
```bash
kubectl get crd gateways.gateway.networking.k8s.io
```

### InferencePool CRD

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.1.0/manifests.yaml
```

Verify:
```bash
kubectl get crd | grep inference
```

You should see:
```
inferencepools.inference.networking.x-k8s.io
inferenceobjectives.inference.networking.x-k8s.io
```

If the CRD is still not found, try the kustomize path instead:
```bash
kubectl kustomize "github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd?ref=v1.1.0" | kubectl apply -f -
```

### AI Gateway policy CRDs

All six AI Gateway policy CRDs live under `helm/ako/crds/`:

```bash
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aigatewayauthpolicies.yaml
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aitokenratelimitpolicies.yaml
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aimodelroutepolicies.yaml
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aimcproutepolicies.yaml
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aia2aroutepolicies.yaml
kubectl apply -f helm/ako/crds/ai.ako.vmware.com_aiguardrailpolicies.yaml

kubectl get crd | grep ai.ako
# aia2aroutepolicies.ai.ako.vmware.com
# aigatewayauthpolicies.ai.ako.vmware.com
# aiguardrailpolicies.ai.ako.vmware.com
# aimcproutepolicies.ai.ako.vmware.com
# aimodelroutepolicies.ai.ako.vmware.com
# aitokenratelimitpolicies.ai.ako.vmware.com
```

You don't need all six for every workload — install only the ones you plan to use — but this
guide walks through each in turn, so install all six now.

---

## Step 4 — Avi credentials secret and a fresh AKO install

AKO reads the Avi Controller credentials from a Kubernetes Secret:

```bash
kubectl create namespace avi-system

kubectl create secret generic avi-secret \
  -n avi-system \
  --from-literal=username=admin \
  --from-literal=password=<your-avi-password>
```

Now build one `values.yaml` that both points AKO at your Avi environment **and** turns on
Gateway API + AI Gateway with the image from Step 2:

```yaml
# values.yaml
ControllerSettings:
  controllerHost: "10.x.x.x"            # your Avi Controller IP
  cloudName: "Default-Cloud"            # your Avi Cloud name
  serviceEngineGroupName: "Default-Group"

AKOSettings:
  clusterName: "ai-gateway-demo"        # unique name for this cluster in Avi

NetworkSettings:
  vipNetworkList:
    - networkName: "vip-network"        # your Avi VIP network

featureGates:
  GatewayAPI: true
GatewayAPI:
  image:
    repository: <registry>/ako-gateway-api   # from Step 2
    tag: <tag>                               # from Step 2
aiGateway:
  enabled: true
inferenceExtension:
  enabled: true
  scrapeIntervalSeconds: 15
  alphaKVCache: 1.0
  betaTokenRate: 1.0
```

`inferenceExtension` turns on the Prometheus-scraping controller that auto-adjusts Avi Pool
Group member weights across the InferencePool's pods — see [Inference Extension — weight-based
load balancing](#inference-extension--weight-based-load-balancing) below.

Nothing exists yet, so this is a fresh install, not an upgrade:

```bash
helm install ako ./helm/ako \
  -n avi-system --create-namespace \
  -f values.yaml
kubectl rollout status statefulset/ako -n avi-system
```

Watch the pod come up — you should see two containers, `ako` (the main AKO controller) and
`ako-gateway-api` (your Gateway API build from Step 2):

```bash
kubectl get pods -n avi-system -w
```

---

## Step 5 — Confirm the flag

The container is **distroless** — no `env` binary — and the flag isn't logged, so read the pod
spec directly:

```bash
kubectl get pod ako-0 -n avi-system \
  -o jsonpath='{range .spec.containers[?(@.name=="ako-gateway-api")].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep AI_GATEWAY
# Expected: AI_GATEWAY_ENABLED=true
```

---

## Step 6 — Install the Dashboard UI

A console UI drives most of the policy-creation work in the sections below — token budgets,
model routing, gateway objects, and DLP toggles — through forms instead of `kubectl apply`. It
lives in a separate repo from AKO.

### Get the code

```bash
git clone git@github-vcf.devops.broadcom.net:ANS/AI-Gateway-UI-chris.git
cd AI-Gateway-UI-chris
git checkout feature/shared-counters-ui
```

This branch carries the Governance (token budgets + model routing), Models, Gateways, MCP
Registry, and Agent Registry screens this guide uses below, plus a Dashboard tab (Topology,
Live Counters, Streaming/Shim) and an Avi-backed Inference view.

### Build

```bash
az acr build -r <your-acr-name> -t ai-gateway-ui:<tag> .
```

### Deploy

Manifests live in `k8s/`:

```bash
kubectl apply -f k8s/01-rbac.yaml
```

Edit `k8s/02-deployment.yaml` (container name `ui`, namespace `inference`). Set the image
(`k8s/02-deployment.yaml:19`) to `<your-acr-name>.azurecr.io/ai-gateway-ui:<tag>`. Leave the
rest of the env vars at their defaults for now — nothing downstream exists yet, and they only
affect the optional Topology / Live-Counters / Inference dashboard views, not the
policy-creation forms this guide relies on:

- `NAMESPACE=inference` (`k8s/02-deployment.yaml:24`)
- `POLICY_NAME=llm-limits` (`k8s/02-deployment.yaml:25`) — the `AITokenRateLimitPolicy` name from
  [Token rate limiting](#token-rate-limiting--aitokenratelimitpolicy)
- `GATEWAY_VIP=10.225.0.100` (`k8s/02-deployment.yaml:26`) — a placeholder until the real
  Gateway exists (Step 7)
- `DEMO_HOST=llm.demo.local` (`k8s/02-deployment.yaml:27`)
- `ISSUER_IP_PREFIX=10.224` (`k8s/02-deployment.yaml:28`) — adjust to your pod CIDR prefix
- `ISSUER_USERS_URL` / `ISSUER_TOKEN_URL` — built-in defaults pointing at
  `http://jwt-issuer.inference.svc.cluster.local:8080` (`server.go:114-115`); leave unset unless
  your issuer Service name differs
- Optionally `AVI_CONTROLLER` / `AVI_USERNAME` / `AVI_PASSWORD` / `AVI_VERSION`
  (`k8s/02-deployment.yaml:30-35`) to light up the Inference tab's live Avi pool-ratio view — ties
  to [Inference Extension — weight-based load balancing](#inference-extension--weight-based-load-balancing)

```bash
kubectl apply -f k8s/02-deployment.yaml
kubectl rollout status deployment/ai-gateway-ui -n inference
```

### Expose it

A dedicated Avi Gateway/HTTPRoute, hostname `ai-gw-ui.demo.local`:

```bash
kubectl apply -f k8s/03-gateway-httproute.yaml
```

or a direct AKS public LB Service, IP-locked:

```bash
kubectl apply -f k8s/04-public-lb.yaml
```

### Verify

Open the exposed address. The Dashboard, Governance, Models, Gateways, MCP Registry, and Agent
Registry tabs all load — everything on them is empty at this point, since nothing from Step 7
onward has been created yet.

You're currently inside `AI-Gateway-UI-chris`. Move back into the AKO repo before continuing —
every command from Step 7 on uses paths relative to its root:

```bash
cd ../AI-Gateway-chris
```

---

## Step 7 — Create the Gateway, InferencePool, and HTTPRoute

AKO installs a `GatewayClass` automatically. Verify:

```bash
kubectl get gatewayclass avi-lb
```

Create the `inference` namespace and a Gateway named `avi-gateway`:

```yaml
# gateway.yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: avi-gateway
  namespace: inference
spec:
  gatewayClassName: avi-lb
  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: Same
```

```bash
kubectl create namespace inference
kubectl apply -f gateway.yaml
kubectl get gateway -n inference avi-gateway
```

The bare Gateway object can also be created from the UI's Gateways tab (**+ Create**, leave
Gateway type at its default `LLM`).

Create the `InferencePool` from the UI instead of `kubectl apply`:

1. Open **Models** and click **+ Create**.
2. Name `llm-pool`, Namespace `inference`, Pod selector `app=mock-llm` — the label the mock LLM
   pods deployed in the next step carry — Target port `8000`.
3. Click **Save**.

The `HTTPRoute` that sends `/v1` traffic to it has no UI equivalent — create it with `kubectl`:

```yaml
# httproute.yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: llm-route
  namespace: inference
spec:
  parentRefs:
  - name: avi-gateway
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /v1
    backendRefs:
    - group: inference.networking.x-k8s.io
      kind: InferencePool
      name: llm-pool
```

```bash
kubectl apply -f httproute.yaml
```

---

## Step 8 — Deploy the mock LLM pods (emit token headers)

```bash
kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/mock-llm.yaml
kubectl rollout status deployment/mock-llm-1 -n inference
kubectl rollout status deployment/mock-llm-2 -n inference
```

`mock-llm.yaml` labels pods `app: mock-llm` — the same label the `InferencePool` in Step 7
already selects, so AKO resolves the pool to these pods with no extra wiring. The mock returns
an OpenAI-compatible JSON body with a `usage` block; the DataScript reads token counts directly
from the body (no response headers required).

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

## Inference Extension — weight-based load balancing

AKO scrapes each InferencePool member pod's metrics every `scrapeIntervalSeconds` and adjusts
Avi Pool Group member weights using `score = 1/(waiting + alpha*kv_cache + beta*token_rate)`.

The mock-llm pods deployed in Step 8 already serve this — their `server.py`'s `GET` handler
returns the Prometheus `vllm:num_requests_waiting` / `vllm:kv_cache_usage_perc` /
`vllm:generation_tokens_total` format regardless of path, driven by the same
`WAITING`/`KV_CACHE`/`TOKEN_RATE` env vars already on those Deployments (see
[`mock-llm.yaml`](examples/ai-gateway-demo/mock-llm.yaml)). No new pods, no new InferencePool.

### Run it

```bash
kubectl set env deployment/mock-llm-1 -n inference WAITING=20 KV_CACHE=0.8
```

Check the ratio shift — either in the Avi UI (**Applications → Virtual Services** → the
`avi-gateway`-derived VS → **Pool Group → Members**, `Ratio` column) or via:

```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "weights updated"
```

Expected: mock-llm-1's ratio drops within one scrape interval (15s default), mock-llm-2's rises.

Recover:

```bash
kubectl set env deployment/mock-llm-1 -n inference WAITING=0 KV_CACHE=0.1
```

Ratios re-equalize.

---

## Auth — `jwtQuery`

The UI doesn't configure this policy yet — apply the CRD directly.

`AIGatewayAuthPolicy` with `authMode: jwtQuery` validates a bearer JWT presented as a `?jwt=`
query parameter — 401 on failure, no redirect. Every claim-aware policy below (token budgets,
model-tier entitlements, MCP tool RBAC, A2A agent RBAC) reads claims off the **same** validated
token via the shared `jwt_claim()` DataScript helper, so this section only needs to be done once.

### Add an HTTPS listener

TLS is required by both auth modes (see Prerequisites), but `jwtQuery` needs nothing beyond the
listener itself — no OAuth callback path, no cookie, no redirect chain to reason about:

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout /tmp/llm-tls.key -out /tmp/llm-tls.crt \
  -subj "/CN=llm.demo.local" -addext "subjectAltName=DNS:llm.demo.local"
kubectl create secret tls llm-tls -n inference --cert=/tmp/llm-tls.crt --key=/tmp/llm-tls.key
```

Add an HTTPS listener (port 443, hostname `llm.demo.local`, `certificateRefs: [llm-tls]`) to the
`avi-gateway` Gateway alongside the existing HTTP listener, then re-apply it:

```bash
kubectl get gateway avi-gateway -n inference \
  -o jsonpath='{range .status.listeners[*]}{.name}: programmed={.conditions[?(@.type=="Programmed")].status}{"\n"}{end}'
# http:  programmed=True
# https: programmed=True
```

### Deploy the in-cluster JWT issuer

`jwt-issuer.yaml` exposes `GET /jwks` (SE — actually AKO — fetches the public keyset) and a
legacy direct-mint endpoint `GET /token?sub=&group=` for claim tests. `jwtQuery` clients use
**only** the direct mint — no `/authorize` OAuth dance needed. User → group:
**alice/carol → engineering, bob → product-management, dave → admin.** Any extra query param on `/token` becomes
an extra JWT claim (e.g. `&agent_id=orchestrator`, used later for A2A).

```bash
openssl genrsa -out /tmp/key.pem 2048
kubectl create secret generic jwt-signing-key -n inference --from-file=key.pem=/tmp/key.pem

kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/jwt-issuer.yaml
kubectl rollout status deployment/jwt-issuer -n inference
```

### Apply the auth policy

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIGatewayAuthPolicy
metadata:
  name: llm-auth
  namespace: inference
spec:
  authMode: jwtQuery
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: llm-route
  jwt:
    issuer: "http://jwt-issuer.inference.svc.cluster.local:8080"
    jwksUri: "http://jwt-issuer.inference.svc.cluster.local:8080/jwks"
    audiences:
    - llm-api
EOF

kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "set JWT-query auth"
# AIGatewayAuthPolicy inference/llm-auth: set JWT-query auth (sso=inference-llm-auth-jwt-sso, audience=llm-api, jwt_name=jwt)
```

In the Avi UI: **Templates → Security → Auth Profiles** (`inference-llm-auth-jwt-auth`, type
`AUTH_PROFILE_JWT`) and **SSO Policies** (`inference-llm-auth-jwt-sso`, type `SSO_TYPE_JWT`).
There is **no issuer Pool** — unlike `oauthBrowser`, AKO fetched the JWKS itself, so nothing
needs to appear under **Applications → Pools**.

### Run it

```bash
kubectl port-forward -n inference svc/jwt-issuer 8080:8080 &
PF_PID=$!
TOKEN=$(curl -s "http://localhost:8080/token?sub=alice" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
kill $PF_PID

VIP=$(kubectl get gateway avi-gateway -n inference -o jsonpath='{.status.addresses[0].value}')

# No token -> 401, no redirect
curl -sk -o /dev/null -w "no token:  HTTP %{http_code}\n" \
  -X POST "https://llm.demo.local/v1/chat/completions" --resolve "llm.demo.local:443:${VIP}" \
  -H "Content-Type: application/json" -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}'

# Valid token in the query param -> 200
curl -sk -o /dev/null -w "with token: HTTP %{http_code}\n" \
  -X POST "https://llm.demo.local/v1/chat/completions?jwt=${TOKEN}" --resolve "llm.demo.local:443:${VIP}" \
  -H "Content-Type: application/json" -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}'
```

Expected: `no token: HTTP 401`, `with token: HTTP 200`.

> `llm-auth` stays attached to `llm-route` for the rest of this guide's claim-aware sections
> (token budgets by group, model-tier entitlements). The **flat, unauthenticated** token-budget
> demo just below needs it detached — the guide tells you when to remove and re-apply it.

---

## Token rate limiting — `AITokenRateLimitPolicy`

Identity resolution here is mode-aware but shared code: the DataScript tries `jwt_claim("sub")`
first — which, under `jwtQuery`, base64url-decodes the SE-validated `?jwt=` token directly — and
only falls back to `identitySource.header` (default `x-ai-consumer`) or `clientIP` when no claim
is present. So an authenticated `jwtQuery` request is metered by its verified `sub`, with no
extra wiring needed.

### Flat budget (no auth)

A simple per-IP token budget — no auth required. This check must run **without** an
`AIGatewayAuthPolicy` attached (a request with no `?jwt=` would otherwise 401 before it ever
reaches the DataScript), so detach `llm-auth` first if you completed the Auth section above:

```bash
kubectl delete aigatewayauthpolicy llm-auth -n inference
```

Create the policy from the UI instead of `kubectl apply`:

1. Open **Governance → Token Rate Limits** and click **+ Create**.
2. Name `llm-limits-flat`, Namespace `inference`, Target HTTPRoute `llm-route`.
3. Leave the **Per-group budgets** table empty.
4. Limit settings: Window `1h`, Token dimension `total`, Fallback budget (unknown group) `500`,
   Reject status code `429`, check **Send Retry-After header**.
5. Click **Save & apply to cluster**.

Confirm AKO attached the DataScripts:

```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "registered token-accounting DataScripts"
# AITokenRateLimitPolicy inference/llm-limits-flat: registered token-accounting DataScripts on VS ...
```

In the Avi UI: **Applications → Virtual Services → <llm-route VS> → DataScript** shows
`<vsname>-ai-tok-req` (HTTP_REQ), `<vsname>-ai-tok-resp` (HTTP_RESP), and
`<vsname>-ai-tok-respdata` (HTTP_RESP_DATA).

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

Remove the flat policy and re-apply auth before the group-budget example:

```bash
kubectl delete aitokenratelimitpolicy llm-limits-flat -n inference
kubectl apply -f - <<'EOF'
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIGatewayAuthPolicy
metadata:
  name: llm-auth
  namespace: inference
spec:
  authMode: jwtQuery
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: llm-route }
  jwt:
    issuer: "http://jwt-issuer.inference.svc.cluster.local:8080"
    jwksUri: "http://jwt-issuer.inference.svc.cluster.local:8080/jwks"
    audiences: ["llm-api"]
EOF
```

### Per-group budget (with `jwtQuery` auth)

**Verified finding:** `groupHeader` is read via the same mode-aware `jwt_claim()` helper as the
identity claim — it names a **JWT claim**, not an HTTP header, despite the field's name. That
means the group-budget policy below is **byte-for-byte identical** whether `llm-auth` uses
`oauthBrowser` or `jwtQuery` — only the auth policy's `authMode` and how the client presents the
token change. Verified engineering users get 500 tokens/hr, product-management get 1000; unknown groups → 403.

Create the policy from the UI:

1. Open **Governance → Token Rate Limits** and click **+ Create**.
2. Name `llm-limits`, Namespace `inference`, Target HTTPRoute `llm-route`.
3. Per-group budgets: **+ Add group** `engineering` / `500`, then **+ Add group** `product-management` / `1000`.
4. Limit settings: Window `1h`, Token dimension `total`, Fallback budget (unknown group) `0`
   (0 = reject unknown groups, HTTP 403), Reject status code `429`, check **Send Retry-After
   header**.
5. Click **Save & apply to cluster**.

Run it — mint tokens for two users and drive each to their group's ceiling:

```bash
kubectl port-forward -n inference svc/jwt-issuer 8080:8080 &
PF_PID=$!
mint() { curl -s "http://localhost:8080/token?sub=$1" | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])'; }
TOKEN_ALICE=$(mint alice)   # engineering -> budget 500 (5 requests)
TOKEN_BOB=$(mint bob)       # product-management -> budget 1000 (10 requests)
TOKEN_DAVE=$(mint dave)     # admin  -> not in groupBudgets -> 403
kill $PF_PID

VIP=$(kubectl get gateway avi-gateway -n inference -o jsonpath='{.status.addresses[0].value}')
hit() { curl -sk -o /dev/null -w "%{http_code} " -X POST "https://llm.demo.local/v1/chat/completions?jwt=$1" \
  --resolve "llm.demo.local:443:${VIP}" -H "Content-Type: application/json" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hi"}]}'; }

echo "alice (engineering):"; for i in $(seq 1 6); do hit "$TOKEN_ALICE"; done; echo
echo "bob (product-management):";   for i in $(seq 1 11); do hit "$TOKEN_BOB"; done; echo
echo "dave (admin):";   hit "$TOKEN_DAVE"; echo
```

Expected: `alice: 200 200 200 200 200 429`, `bob: 200×10 429`, `dave: 403` with body
`{"error":"unknown_group","group":"admin"}`.

---

## Model routing — `AIModelRoutePolicy`

Routes each request to a quality/cost tier by the requested `model` field, optionally gated by
the caller's verified group. Full design + Avi object mapping: [model-routing.md](model-routing.md).

Reuse the two mock-llm pods from Step 8 as two tiers, each with its own `InferencePool`. Create
both from the **Models** tab instead of `kubectl apply`:

1. **+ Create**: Name `premium-llm`, Namespace `inference`, Pod selector `pod=1` (mock-llm-1
   only), Target port `8000`. Click **Save**.
2. **+ Create**: Name `economy-llm`, Namespace `inference`, Pod selector `pod=2` (mock-llm-2
   only), Target port `8000`. Click **Save**.

Create the `AIModelRoutePolicy` from the UI instead of `kubectl apply`:

1. Open **Governance → Model Routing Policies** and click **+ Create**.
2. Policy: Name `llm-tiers`, Namespace `inference`, Target HTTPRoute `llm-route` (already the
   default), Model field `model` (already the default).
3. Tiers: row 1 — Tier name `premium`, Backend kind `InferencePool`, Backend `premium-llm`.
   Click **+ Add tier** for row 2 — Tier name `economy`, Backend kind `InferencePool`, Backend
   `economy-llm`.
4. Model → tier mapping: Requested model `mock-llm-premium` → Tier `premium`. Default tier:
   `economy`.
5. Leave **Entitlements** empty and **Unentitled / unknown-model handling** at its defaults
   (`Downgrade`, `403`).
6. Click **Create**.

Confirm AKO built the per-tier Pool Groups and DataScript:

```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep -i "model.*rout\|tier"
```

### Run it

`llm-route` already carries `llm-auth` (`jwtQuery`) from the sections above, so mint a token
first. The mock's response body echoes the **pod name** that served it
(`"[mock response #N from <pod>]"`), which is the easiest way to see the tier decision land on a
different backend:

```bash
kubectl port-forward -n inference svc/jwt-issuer 8080:8080 &
PF_PID=$!
TOKEN=$(curl -s "http://localhost:8080/token?sub=alice" | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
kill $PF_PID
VIP=$(kubectl get gateway avi-gateway -n inference -o jsonpath='{.status.addresses[0].value}')

curl -sk "https://llm.demo.local/v1/chat/completions?jwt=${TOKEN}" --resolve "llm.demo.local:443:${VIP}" \
  -d '{"model":"mock-llm-premium","messages":[{"role":"user","content":"hi"}]}' | grep -o 'from mock-llm-[a-z0-9-]*'
# from mock-llm-1-... (premium tier)

curl -sk "https://llm.demo.local/v1/chat/completions?jwt=${TOKEN}" --resolve "llm.demo.local:443:${VIP}" \
  -d '{"model":"anything-unlisted","messages":[{"role":"user","content":"hi"}]}' | grep -o 'from mock-llm-[a-z0-9-]*'
# from mock-llm-2-... (unmatched model -> defaultTier economy)
```

Add `entitlements` (gated by the verified `group` claim, same mint-a-token flow as above) and
`onUnentitled: { type: Downgrade }` to see a `free`-group caller silently dropped to `economy`
even when it asks for the premium model — see the ["Tier entitlement by
group"](model-routing.md#tier-entitlement-by-group-with-auth) example.

---

## MCP — `AIMCPRoutePolicy`

Governs agent↔tool (Model Context Protocol) traffic: per-role authorization of individual
JSON-RPC tool calls. Requires **Avi 32.1.1+** (native MCP application profile + session
DataScript). Full design: [ai-gateway-mcp.md](ai-gateway-mcp.md).

The UI doesn't configure `AIMCPRoutePolicy`'s tool-access rules yet — apply the CRD directly.
(The MCP Gateway shell and its registry-backed route can also be created from the UI's Gateways
and MCP Registry tabs; this guide keeps both on `kubectl` since the RBAC policy needs it anyway.)

MCP gets its own dedicated Gateway (blast-radius isolation from the LLM route) and its own TLS
listener:

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout /tmp/mcp-tls.key -out /tmp/mcp-tls.crt \
  -subj "/CN=mcp.demo.local" -addext "subjectAltName=DNS:mcp.demo.local"
kubectl create secret tls mcp-tls -n inference --cert=/tmp/mcp-tls.crt --key=/tmp/mcp-tls.key
```

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: mcp-gateway
  namespace: inference
spec:
  gatewayClassName: avi-lb
  listeners:
    - name: https
      protocol: HTTPS
      port: 443
      tls: { mode: Terminate, certificateRefs: [{ name: mcp-tls }] }
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: mcp-route
  namespace: inference
spec:
  parentRefs: [{ name: mcp-gateway }]
  hostnames: ["mcp.demo.local"]
  rules:
  - matches: [{ path: { type: PathPrefix, value: /mcp } }]
    backendRefs:
    - name: mock-llm       # stand-in backend — see note below
      port: 8000
```

> The mock-llm backend doesn't speak JSON-RPC/MCP — this step doesn't need it to. AKO's
> tool-authorization check runs in `HTTP_REQ_DATA`, **before** the request ever reaches a pool
> member: a denied tool call gets its `403` straight from the DataScript and never touches the
> backend. Only the *allowed* case actually reaches (and gets an unrelated 200 from) mock-llm.

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIMCPRoutePolicy
metadata:
  name: tools-policy
  namespace: inference
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: mcp-route }
  authRef:
    name: llm-auth              # reuse the same jwtQuery IdP as the LLM gateway
  session:
    header: Mcp-Session-Id
    timeout: 30m
  toolAccess:
    roleClaim: role
    rules:
      - role: operator
        allow: ["*"]
      - role: guest
        allow: ["search.query"]
  onUnauthorized:
    type: Reject
    statusCode: 403
```

```bash
kubectl apply -f mcp-gateway.yaml
kubectl apply -f tools-policy.yaml
```

### Run it

```bash
kubectl port-forward -n inference svc/jwt-issuer 8080:8080 &
PF_PID=$!
TOKEN_GUEST=$(curl -s "http://localhost:8080/token?sub=guest1&role=guest" | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
kill $PF_PID
MCP_VIP=$(kubectl get gateway mcp-gateway -n inference -o jsonpath='{.status.addresses[0].value}')

# guest may call search.query -> allowed, reaches the (unrelated) mock backend -> 200
curl -sk -o /dev/null -w "allowed tool:  HTTP %{http_code}\n" \
  --resolve "mcp.demo.local:443:${MCP_VIP}" "https://mcp.demo.local/mcp?jwt=${TOKEN_GUEST}" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search.query","arguments":{}}}'

# guest may NOT call filesystem.write -> rejected by the DataScript, never reaches the backend
curl -sk -o /dev/null -w "denied tool:   HTTP %{http_code}\n" \
  --resolve "mcp.demo.local:443:${MCP_VIP}" "https://mcp.demo.local/mcp?jwt=${TOKEN_GUEST}" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"filesystem.write","arguments":{}}}'
```

Expected: `allowed tool: HTTP 200`, `denied tool: HTTP 403`.

---

## A2A — `AIA2ARoutePolicy`

Governs agent↔agent (Agent2Agent, JSON-RPC) traffic: shared-IdP identity, per-agent method RBAC,
and multi-turn task affinity, built from the same DataScript machinery as model routing. Full
design: [ai-gateway-a2a.md](ai-gateway-a2a.md).

The UI doesn't configure `AIA2ARoutePolicy`'s agent-access rules yet — apply the CRD directly.
(The A2A Gateway shell can also be created from the UI's Gateways tab; this guide keeps it on
`kubectl` alongside the mock agents and route policies below.)

This one has ready-to-run demo assets — a dedicated A2A Gateway, two mock agents (`ops-agent`,
`security-agent`), and the `AIA2ARoutePolicy` pair (an orchestrator that may submit tasks to both,
a security agent that may only read tasks on `ops-agent`):

```bash
# TLS cert for the A2A gateway hostnames
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout /tmp/a2a-tls.key -out /tmp/a2a-tls.crt -subj "/CN=a2a.demo.local" \
  -addext "subjectAltName=DNS:ops-agent.demo.local,DNS:security-agent.demo.local"
kubectl create secret tls a2a-tls -n inference --cert=/tmp/a2a-tls.crt --key=/tmp/a2a-tls.key

kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/mock-a2a-agent.yaml
kubectl rollout status deployment/ops-agent -n inference
kubectl rollout status deployment/security-agent -n inference

kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/a2a-gateway.yaml
kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/a2a-gateway-policies.yaml
```

The `AIA2ARoutePolicy` CRD's real fields (verified against
[`a2aroute_types.go`](../../ako-gateway-api/aigateway/a2aroute_types.go)) — `targetRef`,
`authRef`, `agentCard`, `taskAffinity`, `agentAccess`, `onUnauthorized`:

```yaml
apiVersion: ai.ako.vmware.com/v1alpha1
kind: AIA2ARoutePolicy
metadata:
  name: ops-agent-a2a
  namespace: inference
spec:
  targetRef: { group: gateway.networking.k8s.io, kind: HTTPRoute, name: ops-agent-route }
  authRef: { name: llm-auth }        # same jwtQuery IdP as the LLM/MCP gateways
  agentCard: { rewrite: true, url: "https://ops-agent.demo.local" }
  taskAffinity: { timeout: 30m }
  agentAccess:
    agentClaim: agent_id             # a distinct claim from "sub" for agent identity
    rules:
      - agent: orchestrator
        allow: ["tasks/send", "tasks/sendSubscribe", "tasks/get", "tasks/cancel", "tasks/resubscribe"]
      - agent: security-agent
        allow: ["tasks/get"]         # read-only
  onUnauthorized: { type: Reject, statusCode: 403 }
```

### Run it

```bash
kubectl port-forward -n inference svc/jwt-issuer 8080:8080 &
PF_PID=$!
TOKEN_ORCH=$(curl -s "http://localhost:8080/token?sub=orchestrator&agent_id=orchestrator" | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
TOKEN_SEC=$(curl -s "http://localhost:8080/token?sub=security-agent&agent_id=security-agent" | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
kill $PF_PID
A2A_VIP=$(kubectl get gateway a2a-gateway -n inference -o jsonpath='{.status.addresses[0].value}')

# orchestrator may submit a task to ops-agent -> 200
curl -sk -o /dev/null -w "orchestrator tasks/send: HTTP %{http_code}\n" \
  --resolve "ops-agent.demo.local:443:${A2A_VIP}" "https://ops-agent.demo.local/?jwt=${TOKEN_ORCH}" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tasks/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}'

# security-agent may only read -> tasks/send to ops-agent is rejected
curl -sk -o /dev/null -w "security-agent tasks/send: HTTP %{http_code}\n" \
  --resolve "ops-agent.demo.local:443:${A2A_VIP}" "https://ops-agent.demo.local/?jwt=${TOKEN_SEC}" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tasks/send","params":{"message":{"role":"user","parts":[{"text":"hi"}]}}}'
```

Expected: `orchestrator tasks/send: HTTP 200`, `security-agent tasks/send: HTTP 403`.

---

## Guardrails — `AIGuardrailPolicy`

Signature/regex DLP on the Avi WAF — blocks secrets, PII, and prompt-injection phrases in request
bodies. No proxy, no sidecar, no model in the hot path. Full design:
[ai-gateway-guardrails.md](ai-gateway-guardrails.md).

Turn it on from the **Gateways** tab instead of `kubectl apply`:

1. Open **Gateways**, find the `avi-gateway` row, and check the box in its **DLP** column.
   Checking it creates a `BlockLLM` guardrail (targeting the gateway, action `Block`) if none
   exists yet, or flips an existing one to `Block`; unchecking flips it to `Log`
   (shadow/detect-only).

The toggle is a binary enforce/shadow switch on `BlockLLM`/`BlockMCP`/`BlockLLMAndMCP` — it does
not expose this guide's `inspect.request`/`inspect.response` or a custom `action.statusCode`;
anything beyond the toggle still needs `kubectl`.

```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "attached WAF guardrail"
# AIGuardrailPolicy inference/avi-gateway-dlp: attached WAF guardrail inference-avi-gateway-dlp-ai-guardrail on VS ...
```

In the Avi UI: **Templates → Security → WAF Policies** (`inference-avi-gateway-dlp-ai-guardrail`),
referenced from the VS's `waf_policy_ref`.

> **jwtQuery + guardrails coexist, but only with the fix for commit `591de2438`.** Guardrail
> rules scan `ARGS|REQUEST_BODY`, which includes query parameters — and `jwtQuery` puts the
> bearer token in `?jwt=`. Without the `!ARGS:jwt` exclusion in
> [`guardrail_waf.go`](../../ako-gateway-api/aigateway/guardrail_waf.go), the WAF's own `jwt`
> secret-signature would match the token string and 403 **every** authenticated request. If your
> image predates that commit, every `jwtQuery` request on a guardrailed route will 403 regardless
> of body content — see [Troubleshooting](#troubleshooting).

### Run it

`llm-route` already requires `?jwt=` (from the Auth section), so this demonstrates the guardrail
blocking on top of a successfully authenticated request — not instead of one:

```bash
kubectl port-forward -n inference svc/jwt-issuer 8080:8080 &
PF_PID=$!
TOKEN=$(curl -s "http://localhost:8080/token?sub=alice" | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
kill $PF_PID
VIP=$(kubectl get gateway avi-gateway -n inference -o jsonpath='{.status.addresses[0].value}')

# Clean prompt -> 200
curl -sk -o /dev/null -w "clean prompt:  HTTP %{http_code}\n" \
  --resolve "llm.demo.local:443:${VIP}" "https://llm.demo.local/v1/chat/completions?jwt=${TOKEN}" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"hello"}]}'

# Prompt containing an AWS access key -> blocked by the WAF, 403
curl -sk -o /dev/null -w "leaked secret: HTTP %{http_code}\n" \
  --resolve "llm.demo.local:443:${VIP}" "https://llm.demo.local/v1/chat/completions?jwt=${TOKEN}" \
  -d '{"model":"mock-llm-v1","messages":[{"role":"user","content":"my key is AKIAIOSFODNN7EXAMPLE"}]}'
```

Expected: `clean prompt: HTTP 200`, `leaked secret: HTTP 403`.

---

## Cleanup

```bash
# Auth + token budgets
kubectl delete aigatewayauthpolicy llm-auth -n inference        # AKO deletes the JWTServerProfile/AuthProfile/SSOPolicy
kubectl delete aitokenratelimitpolicy llm-limits -n inference
kubectl delete aitokenratelimitpolicy llm-limits-flat -n inference --ignore-not-found

# Model routing
kubectl delete aimodelroutepolicy llm-tiers -n inference --ignore-not-found
kubectl delete inferencepool premium-llm economy-llm -n inference --ignore-not-found

# MCP
kubectl delete aimcproutepolicy tools-policy -n inference --ignore-not-found
kubectl delete httproute mcp-route -n inference --ignore-not-found
kubectl delete gateway mcp-gateway -n inference --ignore-not-found
kubectl delete secret mcp-tls -n inference --ignore-not-found

# A2A
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/a2a-gateway-policies.yaml --ignore-not-found
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/a2a-gateway.yaml --ignore-not-found
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/mock-a2a-agent.yaml --ignore-not-found
kubectl delete secret a2a-tls -n inference --ignore-not-found

# Guardrails — resource name depends on how you created it above
kubectl delete aiguardrailpolicy avi-gateway-dlp -n inference --ignore-not-found
kubectl delete aiguardrailpolicy ai-dlp-baseline -n inference --ignore-not-found

# Shared issuer + LLM listener + mocks
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/jwt-issuer.yaml
kubectl delete secret jwt-signing-key -n inference
kubectl delete secret llm-tls -n inference
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/mock-llm.yaml

# Core Gateway API objects created in Steps 7-8
kubectl delete httproute llm-route -n inference --ignore-not-found
kubectl delete inferencepool llm-pool -n inference --ignore-not-found
kubectl delete gateway avi-gateway -n inference --ignore-not-found

# Dashboard UI (Step 6) — run from the AI-Gateway-UI-chris directory
kubectl delete -f k8s/03-gateway-httproute.yaml --ignore-not-found   # if you used the Gateway/HTTPRoute exposure
kubectl delete -f k8s/04-public-lb.yaml --ignore-not-found           # if you used the public LB exposure
kubectl delete -f k8s/02-deployment.yaml --ignore-not-found
kubectl delete -f k8s/01-rbac.yaml --ignore-not-found
```

The steps above remove everything this guide layered on top. AKO itself, the `avi-secret`, and
the `inference` namespace are left running by default — other workloads (or a re-run of a
section above) may still depend on them.

### Full teardown (optional)

Only do this if you're done with the cluster entirely — it removes the fresh AKO install from
Step 4 along with its credentials and namespace:

```bash
helm uninstall ako -n avi-system
kubectl delete secret avi-secret -n avi-system --ignore-not-found
kubectl delete namespace inference --ignore-not-found
```

Leave the Gateway API / InferencePool / AI Gateway CRDs in place unless you're certain nothing
else on the cluster uses them — deleting a CRD deletes every custom resource of that kind,
cluster-wide, not just the ones from this guide:

```bash
kubectl delete -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.1.0/manifests.yaml
kubectl delete -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml
kubectl delete -f helm/ako/crds/ai.ako.vmware.com_aigatewayauthpolicies.yaml
kubectl delete -f helm/ako/crds/ai.ako.vmware.com_aitokenratelimitpolicies.yaml
kubectl delete -f helm/ako/crds/ai.ako.vmware.com_aimodelroutepolicies.yaml
kubectl delete -f helm/ako/crds/ai.ako.vmware.com_aimcproutepolicies.yaml
kubectl delete -f helm/ako/crds/ai.ako.vmware.com_aia2aroutepolicies.yaml
kubectl delete -f helm/ako/crds/ai.ako.vmware.com_aiguardrailpolicies.yaml
```

---

## Troubleshooting

**DataScripts don't appear on the VS** — confirm `AI_GATEWAY_ENABLED=true` (Step 5; the
container is distroless so read the pod spec, not `kubectl exec -- env`).

**5xx / "no available servers"** — the backend pods are down or not selected by the
InferencePool. If you're running a large scale test, many InferencePools all scraping the same
few single-threaded mock pods can saturate them and fail health checks — scale the mock down or
add replicas.

**Token limits never fire** — the DataScript parses `usage.total_tokens` from the response
**body** (in `HTTP_RESP_DATA`). Check the backend actually returns a non-streaming
`application/json` response with an OpenAI `usage` block, and that the body fits within
`RespBodyBufferKB` (default 256 KB — `usage` sits at the end of the body, so a response larger
than the buffer would normally be truncated; the script now charges `FailClosedTokens` for such a
completion rather than letting it through unmetered). The bundled workload-sim mock can report
large token counts — reset it (`/set?prompt=25&completion=75`) for a clean 100-token-per-request
demo.

**Every `jwtQuery` request 401s, even with a valid token** — confirm the listener is HTTPS (both
auth modes require TLS) and that the token's `aud` matches `spec.jwt.audiences` and its `iss`
matches `spec.jwt.issuer` exactly (including scheme/port). Check `EnsureJWTServerProfile` logs for
a JWKS fetch failure — under `jwtQuery`, **AKO** fetches the JWKS (not the SE), so this is a
network path from the AKO pod to `jwksUri`, not from the SE.

**Guardrails 403 every authenticated request on a `jwtQuery` route, even clean prompts** — this is
the WAF-vs-`jwtQuery` conflict fixed by commit `591de2438` (`!ARGS:jwt` exclusion in
[`guardrail_waf.go`](../../ako-gateway-api/aigateway/guardrail_waf.go)). Without it, the
guardrail's built-in `jwt` secret-signature matches the bearer token riding in the query string.
Verify your AKO image includes this commit; no CRD or manifest change is needed once it does.

**MCP route policy never attaches / VS not programmed** — the MCP application profile
(`System-Secure-HTTP-MCP`) and session DataScript (`System-Standard-MCP`) are native **Avi
32.1.1+** objects; on an older controller the VS build fails. Confirm the controller version.

**MCP/A2A route unauthenticated even with `authRef` set** — `authRef` must name an
`AIGatewayAuthPolicy` **in the same namespace**; a typo leaves the MCP/A2A route unauthenticated
with a warning in the AKO logs (`authRef %q not found`), not a hard failure.

**Model-tier / MCP-tool / A2A-agent decision looks wrong** — all three read the group/role/agent
claim via the same `jwt_claim()` helper the token-budget policy uses; confirm the minted token
actually carries that claim (`GET /token?sub=...&<claim>=<value>` on the demo issuer) and that the
claim name in the policy (`groupClaim` / `roleClaim` / `agentClaim`) matches it exactly.

**Auth works but 429/403 group/role decision is wrong** — confirm the issued token carries the
claim named in `groupHeader` / `roleClaim` / `agentClaim` and that its value is a key in the
corresponding rules map. Check the AKO logs for the relevant `Apply*Policy` line.

**UI create/edit buttons return `k8sError: "k8s not connected"`** — the UI pod can't reach the
k8s API server; confirm it's actually running in-cluster and that `k8s/01-rbac.yaml` applied
cleanly (`kubectl get clusterrolebinding ai-gateway-ui`).

**Gateways-tab DLP toggle fails with `AIGuardrailPolicy CRD not installed`** — apply
`helm/ako/crds/ai.ako.vmware.com_aiguardrailpolicies.yaml` (Step 3) before using the toggle.

**Creating an MCP Route from the UI fails with `server <name> is not in the approved registry`**
— the MCP Registry tab only routes to servers already listed in the `mcp-registry` ConfigMap;
unlike the `AIMCPRoutePolicy` backend used in this guide, the UI won't route to an arbitrary
Service.
