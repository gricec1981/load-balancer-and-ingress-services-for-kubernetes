# Fresh Install — AKO with Inference Extension

Step-by-step guide to install AKO with the native inference extension enabled
on a new Kubernetes cluster, using the dev image from your fork.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes cluster | 1.28+ recommended |
| Helm 3 | `brew install helm` |
| kubectl | configured and pointing at your cluster |
| Avi Controller | Reachable from the cluster nodes |
| Docker | Running locally for image build |
| GHCR access | `gh auth login` or `docker login ghcr.io` |

---

## Step 1 — Install Gateway API CRDs

Gateway API CRDs must exist before AKO starts.

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml
```

Verify:
```bash
kubectl get crd gateways.gateway.networking.k8s.io
```

---

## Step 2 — Install InferencePool CRD

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml
```

Verify:
```bash
kubectl get crd inferencepools.gateway.inference.x-k8s.io
```

---

## Step 3 — Build and Push Your AKO Image

From the repo root:

```bash
cd ~/load-balancer-and-ingress-services-for-kubernetes

# Interactive build + push
./hack/build-dev-image.sh ghcr.io/gricec1981 inference-ext
```

Or with make:
```bash
make dev-build-and-push-gateway-api \
  REGISTRY=ghcr.io/gricec1981 \
  TAG=inference-ext
```

Make the package public on GHCR (so the cluster can pull it without a secret):
- Go to https://github.com/users/gricec1981/packages
- Find `ako-gateway-api` → Package settings → Change visibility → Public

Or add an image pull secret — see Step 4b.

---

## Step 4 — Configure values-inference-dev.yaml

Edit `helm/ako/values-inference-dev.yaml` and fill in the `<CHANGE ME>` fields:

```yaml
ControllerSettings:
  controllerHost: "10.x.x.x"          # your Avi Controller IP
  cloudName: "Default-Cloud"           # your Avi Cloud name
  serviceEngineGroupName: "Default-Group"

AKOSettings:
  clusterName: "inference-demo"        # unique name for this cluster in Avi

NetworkSettings:
  vipNetworkList:
    - networkName: "vip-network"       # Avi VIP network
```

Leave all inference settings as-is to use defaults:
```yaml
inferenceExtension:
  enabled: true
  scrapeIntervalSeconds: 15
  alphaKVCache: 1.0
  betaTokenRate: 1.0
```

### Step 4b (Optional) — Private registry pull secret

If GHCR package is private, create a pull secret first:

```bash
kubectl create namespace avi-system

kubectl create secret docker-registry ghcr-pull-secret \
  -n avi-system \
  --docker-server=ghcr.io \
  --docker-username=gricec1981 \
  --docker-password=$(gh auth token)
```

Then add to `values-inference-dev.yaml`:
```yaml
image:
  pullSecrets:
    - name: ghcr-pull-secret
```

---

## Step 5 — Create Avi Credentials Secret

AKO reads the Avi Controller credentials from a Kubernetes Secret:

```bash
kubectl create namespace avi-system

kubectl create secret generic avi-secret \
  -n avi-system \
  --from-literal=username=admin \
  --from-literal=password=<your-avi-password>
```

---

## Step 6 — Install AKO via Helm

```bash
helm install ako ./helm/ako \
  -n avi-system --create-namespace \
  -f helm/ako/values-inference-dev.yaml
```

Watch the pods come up:
```bash
kubectl get pods -n avi-system -w
```

You should see two containers in the AKO pod:
- `ako` — main AKO controller
- `ako-gateway-api` — your inference-extension build

Check logs:
```bash
# Main AKO container
kubectl logs -n avi-system ako-0 -c ako -f

# Gateway API container (inference extension logs here)
kubectl logs -n avi-system ako-0 -c ako-gateway-api -f | grep -E "inference|scraper|InferencePool"
```

---

## Step 7 — Deploy a GatewayClass and Gateway

AKO installs a `GatewayClass` automatically. Verify:
```bash
kubectl get gatewayclass avi-lb
```

Create a Gateway:
```yaml
# gateway.yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: llm-gateway
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
kubectl get gateway -n inference llm-gateway
```

---

## Step 8 — Deploy Mock LLM Pods (No GPU Required)

For demo purposes, deploy the mock vLLM server that exposes controllable Prometheus metrics:

```yaml
# mock-vllm.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-pod-1
  namespace: inference
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vllm
      pod: "1"
  template:
    metadata:
      labels:
        app: vllm
        pod: "1"
    spec:
      containers:
      - name: mock-vllm
        image: python:3.11-slim
        command: ["python3", "-c", |
          import os, time
          from http.server import HTTPServer, BaseHTTPRequestHandler
          gen_tokens = 0
          class H(BaseHTTPRequestHandler):
            def do_GET(self):
              global gen_tokens
              waiting = int(os.environ.get('WAITING', '0'))
              kv = float(os.environ.get('KV_CACHE', '0.1'))
              gen_tokens += int(os.environ.get('TOKEN_RATE', '100'))
              body = f"""vllm:num_requests_waiting {waiting}
vllm:kv_cache_usage_perc {kv}
vllm:num_requests_running 1
vllm:generation_tokens_total {gen_tokens}
vllm:prompt_tokens_total 0
""".encode()
              self.send_response(200)
              self.end_headers()
              self.wfile.write(body)
            def log_message(self, *a): pass
          HTTPServer(('0.0.0.0', 8000), H).serve_forever()
        ]
        env:
        - name: WAITING
          value: "0"       # change to simulate load
        - name: KV_CACHE
          value: "0.1"
        - name: TOKEN_RATE
          value: "100"     # tokens added per /metrics scrape
        ports:
        - containerPort: 8000
```

Deploy 3 replicas with different labels for individual control:
```bash
kubectl apply -f mock-vllm.yaml
# duplicate and change pod labels to pod: "2" and pod: "3"
```

---

## Step 9 — Deploy InferencePool and HTTPRoute

```yaml
# inferencepool.yaml
apiVersion: gateway.inference.x-k8s.io/v1
kind: InferencePool
metadata:
  name: llm-pool
  namespace: inference
spec:
  selector:
    matchLabels:
      app: vllm         # matches all 3 mock pods
  targetPort: 8000
```

```yaml
# httproute.yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: llm-route
  namespace: inference
spec:
  parentRefs:
  - name: llm-gateway
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /v1
    backendRefs:
    - group: gateway.inference.x-k8s.io
      kind: InferencePool
      name: llm-pool
```

```bash
kubectl apply -f inferencepool.yaml
kubectl apply -f httproute.yaml
```

---

## Step 10 — Watch the Weights Change

Open the Avi UI:
- **Applications → Virtual Services** → find your VS
- **Pool Group → Members** — watch `Ratio` values change every 15s

Simulate load on pod-1:
```bash
kubectl set env deployment/vllm-pod-1 -n inference WAITING=20 KV_CACHE=0.8
```

Within one scrape interval (15s), pod-1's ratio should drop significantly and pod-2/3 ratios should rise.

Recover pod-1:
```bash
kubectl set env deployment/vllm-pod-1 -n inference WAITING=0 KV_CACHE=0.1
```

Watch ratios equalise again.

AKO log confirmation:
```bash
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep "weights updated"
```

---

## Upgrade After Code Changes

```bash
# Rebuild and push
./hack/build-dev-image.sh ghcr.io/gricec1981 inference-ext

# Rolling restart to pick up new image (pullPolicy: Always)
kubectl rollout restart statefulset/ako -n avi-system

# Or full helm upgrade
helm upgrade ako ./helm/ako \
  -n avi-system \
  -f helm/ako/values-inference-dev.yaml
```

---

## Uninstall

```bash
helm uninstall ako -n avi-system
kubectl delete namespace inference
kubectl delete -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml
```
