# AI Gateway Demo — Install Guide

Step-by-step guide to demo `AIGatewayAuthPolicy` and `AITokenRateLimitPolicy` on the same
cluster already running the [Inference Extension](inference-install.md).

The demo requires **no real LLMs or GPUs**. It reuses the `mock-llm` pod pattern, which returns
OpenAI-compatible JSON and — importantly — emits the token-usage **response headers** the
AI-Gateway DataScript reads.

> **Three things that matter for this guide**
> 1. **Token usage is parsed from the response body.** The DataScript reads the OpenAI `usage`
>    block straight from the JSON body in the `HTTP_RESP_DATA` event (buffered via
>    `set_response_body_buffer_size`) — so it works with **stock vLLM**, no token headers or
>    sidecar required. (The bundled mock also emits `X-*-Tokens` headers, but they're no longer
>    used.) Trade-off: the SE buffers the body, which suits non-streaming traffic; for streaming
>    responses, meter in a proxy instead.
> 2. **Auth is OAuth/OIDC, and AKO manages the Avi objects.** Applying an `AIGatewayAuthPolicy`
>    makes AKO create the issuer `Pool`, `AUTH_PROFILE_OAUTH` AuthProfile and `SSO_TYPE_OAUTH`
>    Policy. You provide an in-cluster **OIDC provider** (`jwt-issuer.yaml`) — the SE runs the
>    auth-code flow against it.
> 3. **OAuth/OIDC requires an HTTPS Gateway listener.** The flow is a browser, session-cookie
>    redirect — drive it with a cookie jar (a browser, or `curl -c jar`), not `Authorization:
>    Bearer`.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Working inference-extension demo | Gateway, InferencePool, HTTPRoute already set up |
| AKO image with AI Gateway code | This branch; toggled by `aiGateway.enabled` (no rebuild to toggle) |
| Avi Controller reachable from AKO | For the OAuth object lifecycle (Part B) |
| HTTPS Gateway listener + TLS cert | OAuth/OIDC requires TLS (Part B, step B0) |
| Issuer reachable from the Service Engine data path | The SE fetches JWKS / exchanges the code through an AKO-built Pool (Part B) |

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

> **⚠️ Selector.** `mock-llm.yaml` labels pods `app: mock-llm`. Your `InferencePool`
> must select them — if it was created with `selector: {app: vllm}`, either label these pods
> `app: vllm` or change the pool selector to `app: mock-llm` (and let AKO re-resolve). The mock
> returns an OpenAI-compatible JSON body with a `usage` block; the DataScript reads token counts
> directly from the body (no response headers required).

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
`<vsname>-ai-tok-req` (HTTP_REQ), `<vsname>-ai-tok-resp` (HTTP_RESP), and
`<vsname>-ai-tok-respdata` (HTTP_RESP_DATA).

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

## Part B — OAuth/OIDC auth + per-group token budgets

Demonstrates: unauthenticated requests are redirected into the OIDC login; once authenticated,
the **verified** group claim drives the budget — **group1 users get 500 tokens/hr, group2 get
1000**; unknown groups → 403.

> **This is a browser/session flow.** Avi obtains the token itself via the OIDC auth-code
> redirect and stores it in a session cookie — it does *not* validate an `Authorization: Bearer`
> token. Drive it with a cookie jar.

### B0. Put the route on an HTTPS Gateway listener

OAuth/OIDC needs TLS. Add an HTTPS listener with a (self-signed, for the demo) cert and let the
route attach to it:

```bash
# self-signed cert for the route host
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout /tmp/llm-tls.key -out /tmp/llm-tls.crt \
  -subj "/CN=llm.demo.local" -addext "subjectAltName=DNS:llm.demo.local"
kubectl create secret tls llm-tls -n inference --cert=/tmp/llm-tls.crt --key=/tmp/llm-tls.key

# add an HTTPS listener (port 443, hostname llm.demo.local, certificateRefs: llm-tls)
# to the Gateway, then re-apply it. See examples/ for a full gateway.yaml.
kubectl get gateway avi-gateway -n inference \
  -o jsonpath='{range .status.listeners[*]}{.name}: programmed={.conditions[?(@.type=="Programmed")].status}{"\n"}{end}'
# http:  programmed=True
# https: programmed=True
```

### B1. Deploy the in-cluster OIDC provider

`jwt-issuer.yaml` is a small OIDC provider: discovery, `/authorize` (auth-code), `/token`
(code exchange), `/jwks`. The logged-in user is chosen by a `demo_user` cookie so the flow can be
driven headlessly. User → group: **alice/carol → group1, bob → group2, dave → admin**.

```bash
openssl genrsa -out /tmp/key.pem 2048
kubectl create secret generic jwt-signing-key -n inference --from-file=key.pem=/tmp/key.pem

kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/jwt-issuer.yaml
kubectl rollout status deployment/jwt-issuer -n inference
```

### B2. Apply the auth + group-budget policies

```bash
kubectl apply -f docs/gateway-api/examples/ai-gateway-demo/ai-gateway-policies.yaml

kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep -E "issuer Pool|OAuth AuthProfile|OAuth SSOPolicy|OAuth SsoPolicyRef"
# issuer Pool inference-llm-auth-oauth-pool created
# OAuth AuthProfile inference-llm-auth-oauth created
# OAuth SSOPolicy inference-llm-auth-oauth-sso created
# AIGatewayAuthPolicy inference/llm-auth: set OAuth SsoPolicyRef → inference-llm-auth-oauth-sso (audience=llm-api, host=llm.demo.local)
```

In the Avi UI they appear under **Applications → Pools** (`…-oauth-pool`), **Templates →
Security → SSO Policies** (`…-oauth-sso`) and **Auth Profiles** (`…-oauth`).

### B3. Run it (cookie-driven auth-code flow)

The harness follows the redirect chain (`VS → issuer /authorize → /v1/oauth/callback → VS`),
injecting the `demo_user` cookie at the issuer hop to pick the user, then fires N requests on the
resulting session. Run it from an **in-cluster pod** (the issuer's `/authorize` is on a pod IP and
the VIP may be private). Save as `oidc-test.py` and run with `python3` in a `python:3.11-slim` pod:

```python
import json, ssl, urllib.request, http.cookiejar
from urllib.parse import urlparse
# VIP of the avi-gateway (kubectl get gateway avi-gateway -n inference -o jsonpath=...)
VIP = "10.225.0.100"
with open("/etc/hosts", "a") as f: f.write(f"\n{VIP} llm.demo.local\n")
ctx = ssl._create_unverified_context()
class NoRedir(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k): return None
def step(o, url, method, extra=None, body=None):
    h = {"Content-Type": "application/json"}; h.update(extra or {})
    try:
        r = o.open(urllib.request.Request(url, data=body, headers=h, method=method), timeout=15)
        return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e: return e.code, e.headers, e.read()
def session(user):                         # walk the auth-code flow -> session cookie
    o = urllib.request.build_opener(NoRedir, urllib.request.HTTPSHandler(context=ctx),
                                    urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    url, method = "https://llm.demo.local/v1/chat/completions", "GET"
    for _ in range(10):
        host = urlparse(url).hostname or ""
        extra = {"Cookie": "demo_user=" + user} if host.startswith("10.224") else None
        s, hd, _ = step(o, url, method, extra); loc = hd.get("Location", "")
        if s in (301,302,303,307,308) and loc:
            url = loc if loc.startswith("http") else f"{urlparse(url).scheme}://{urlparse(url).netloc}{loc}"
            method = "GET"
        else: return o
    return o
def hit(o):
    return step(o, "https://llm.demo.local/v1/chat/completions", "POST", body=b"{}")[0]
for user, n in [("alice",6),("bob",11),("carol",1),("dave",1)]:
    o = session(user); print(user, [hit(o) for _ in range(n)])
```

Expected (with the bundled 100-token mock): `alice [200×5, 429]`, `bob [200×10, 429]`,
`carol [200]`, `dave [403]`.

> The `demo_user` cookie is matched against the issuer **pod IP** (`10.224.x`) in the harness —
> adjust the prefix if your pod network differs. In a real browser demo, the user logs in at the
> issuer instead of the cookie shortcut.

---

## Part C — Dashboard counters endpoint & reset

A UI can read live per-user usage from a token-gated, read-only endpoint AKO adds to the same
VS. Enable it by pointing the policy at a Secret holding the admin token:

```bash
kubectl create secret generic ai-admin-token -n inference \
  --from-literal=token=$(openssl rand -hex 16)
kubectl annotate aitokenratelimitpolicy llm-limits -n inference \
  ai.ako.vmware.com/admin-token-secret=ai-admin-token
```

AKO regenerates the request DataScript with a `/v1/admin/counters` branch and adds an SSO
`SKIP_AUTHENTICATION` rule for `/v1/admin/` (so it isn't OAuth-redirected). Read it from an
in-cluster pod — pass the users you want, since the SE counter table can't be enumerated:

```bash
TOKEN=$(kubectl get secret ai-admin-token -n inference -o jsonpath='{.data.token}' | base64 -d)
curl -sk "https://llm.demo.local/v1/admin/counters?users=alice,bob,carol" \
  -H "X-Admin-Token: $TOKEN"
# {"window":...,"limit":"hourly-group-budget","counters":[{"user":"alice","used":300}, …]}
# missing / wrong token -> 403 {"error":"forbidden"}
```

Each user is looked up at the *same* key the response-phase accounting writes, so the values are
exactly what enforcement sees. The demo OIDC issuer exposes its identity→group roster at
`GET /users`, so a UI knows which users to query.

### Reset all counters

Bump the `counter-epoch` annotation — AKO folds it into every counter key, moving them all to a
fresh keyspace (an instant reset that leaves budgets and the limit name untouched):

```bash
kubectl annotate aitokenratelimitpolicy llm-limits -n inference \
  ai.ako.vmware.com/counter-epoch=2 --overwrite
```

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
kubectl delete aigatewayauthpolicy llm-auth -n inference        # AKO deletes the Avi Pool/AuthProfile/SSO objects
kubectl delete aitokenratelimitpolicy llm-limits -n inference
kubectl delete -f docs/gateway-api/examples/ai-gateway-demo/jwt-issuer.yaml
kubectl delete secret jwt-signing-key -n inference
kubectl delete secret llm-tls -n inference
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

**Token limits never fire** — the DataScript parses `usage.total_tokens` from the response
**body** (in `HTTP_RESP_DATA`). Check the backend actually returns a non-streaming
`application/json` response with an OpenAI `usage` block, and that the body fits within
`RespBodyBufferKB` (default 256 KB — `usage` sits at the end of the body, so a response larger
than the buffer would normally be truncated; the script now charges `FailClosedTokens` for such a
completion rather than letting it through unmetered). Note the bundled workload-sim mock can
report large token counts — reset it (`/set?prompt=25&completion=75`) for a clean
100-token-per-request demo.

**Every request 302-redirects to the issuer / never gets a 200** — that is the OIDC login
redirect (expected when unauthenticated). Drive the flow with a cookie jar (the B3 harness), make
sure the Gateway listener is HTTPS, and that the `redirect_uri` path sits under the route prefix
(AKO uses `/v1/oauth/callback`).

**Auth works but 429/403 is wrong** — confirm the issued token carries the claim named in
`groupHeader` and that its value is a key in `groupBudgets`. The DataScript reads the verified
claim via `oauth_get_claim`; check the AKO logs for `OAuth SsoPolicyRef`.

**Validation seems to fail (always redirects even with a session)** — confirm the issuer Pool
(`…-oauth-pool`) is `OPER_UP` and reachable from the SE; a failed JWKS fetch makes Avi fall back
to the login redirect instead of validating. The Pool is immutable once OAuth-bound — if the
issuer pod IP changed, delete and re-apply the `AIGatewayAuthPolicy` so AKO rebuilds it.
