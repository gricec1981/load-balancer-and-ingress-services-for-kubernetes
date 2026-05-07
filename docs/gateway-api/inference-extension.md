# AKO Native Inference Extension

## Overview

AKO supports load balancing for LLM inference workloads via a native implementation of the [Gateway API Inference Extension](https://gateway-api-inference-extension.sigs.k8s.io/) (`gateway.inference.x-k8s.io`).

Instead of using Envoy's External Processing (ext-proc) and a separate Endpoint Picker (EPP) sidecar, AKO implements intelligent backend selection directly in the controller. AKO periodically scrapes Prometheus metrics from each LLM pod — including both instantaneous request-queue gauges and token throughput rates — and translates them into Avi Pool Group member weights, steering traffic away from overloaded instances automatically.

This approach works with any gateway that AKO manages — no ext-proc support is required.

---

## How It Works

```
HTTPRoute (backendRef: InferencePool)
        │
        ▼
AKO Inference Controller
  ├── Watches InferencePool CRDs
  ├── Resolves matching pod IPs via label selector
  └── Starts per-pool Prometheus scraper goroutine
              │  (every scrapeIntervalSeconds)
              ▼
        Pod /metrics endpoints (e.g. vLLM)
          ├── Gauges:   num_requests_waiting, kv_cache_usage_perc
          └── Counters: generation_tokens_total, prompt_tokens_total
              │
              ▼
        Weight Calculator
          load  = waiting + α·kv_cache + β·(tokens/sec ÷ max_in_pool)
          score = 1 / (load + ε)
          ratio = round(100 · score / Σscores)
              │
              ▼
        Avi Pool Group Members
          pod-1  Ratio=65   ←── low queue, low tokens/sec, low cache
          pod-2  Ratio=25   ←── moderate queue
          pod-3  Ratio=10   ←── high tokens/sec (processing large context)
```

Each `InferencePool` becomes a set of individual Avi Pools — one per matched pod — grouped under a single Pool Group. The Pool Group member `Ratio` values are updated after each scrape cycle by re-enqueuing the parent HTTPRoute through AKO's normal graph layer.

---

## Prerequisites

> For a complete fresh-install walkthrough including Gateway API CRDs, image build, and mock LLM deployment, see [inference-install.md](inference-install.md).

- `featureGates.GatewayAPI: true` must be set in `values.yaml`
- The InferencePool CRD must be installed on the cluster:
  ```bash
  kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/latest/download/manifests.yaml
  ```
- LLM pods must expose a Prometheus `/metrics` endpoint on the port specified in `InferencePool.spec.targetPort`
- AKO must have network access from its pod to the LLM pod IPs on the metrics port (check NetworkPolicies)

---

## Enabling the Feature

Set the following in `values.yaml`:

```yaml
featureGates:
  GatewayAPI: true

inferenceExtension:
  enabled: true
  scrapeIntervalSeconds: 15   # how often to scrape each pod
  alphaKVCache: 1.0           # weight of KV-cache signal vs waiting queue
  betaTokenRate: 1.0          # weight of token throughput signal vs waiting queue
```

These values are injected as environment variables into the `ako-gateway-api` container:

| Environment Variable | Default | Description |
|---|---|---|
| `INFERENCE_EXTENSION_ENABLED` | `false` | Master switch |
| `INFERENCE_SCRAPE_INTERVAL_SECONDS` | `15` | Scrape interval per pod (seconds) |
| `INFERENCE_ALPHA_KV_CACHE` | `1.0` | KV-cache signal weight (0 = disable) |
| `INFERENCE_BETA_TOKEN_RATE` | `1.0` | Token throughput signal weight (0 = disable) |

---

## Kubernetes Resources

### InferencePool

Replace a Kubernetes `Service` in the HTTPRoute `backendRef` with an `InferencePool`:

```yaml
apiVersion: gateway.inference.x-k8s.io/v1
kind: InferencePool
metadata:
  name: llm-pool
  namespace: inference
spec:
  selector:
    matchLabels:
      app: vllm
  targetPort: 8000   # port for both traffic AND /metrics scraping
```

### HTTPRoute

Reference the `InferencePool` directly as a `backendRef`:

```yaml
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
    - group: gateway.inference.x-k8s.io
      kind: InferencePool
      name: llm-pool
```

**Note:** Do not specify a `port` when using `InferencePool` as the `backendRef` — the port is taken from `InferencePool.spec.targetPort`.

---

## Avi Object Mapping

| Kubernetes resource | Avi object |
|---|---|
| `InferencePool` | Pool Group containing one Pool per matched pod |
| Each matched pod IP | Individual Pool with a single server entry (direct pod IP) |
| Computed load score | Pool Group member `Ratio` (1–100) |

### Naming Convention

Each pod pool is named using the pattern:
```
<prefix>-<parentNs>-<parentName>-<poolNs>-<poolName>-<matchHash>-<podIP>-<port>
```

Each pod gets its own Avi Pool object. Avi's built-in health monitors on each pool independently detect and remove unhealthy pods, separate from the weight-based routing.

---

## Weight Calculation

The scoring formula used to compute `Ratio` values is:

```
load(pod)  = waiting
           + α · kv_cache_perc
           + β · (totalTokens/sec ÷ maxTokens/sec across pool)

score(pod) = 1 / (load + ε)

ratio(pod) = round(100 · score(pod) / Σ score(all_pods))
```

Where:

| Variable | Source | Description |
|---|---|---|
| `waiting` | `vllm:num_requests_waiting` | Instantaneous queue depth |
| `kv_cache_perc` | `vllm:kv_cache_usage_perc` | KV cache fill level (0.0–1.0) |
| `totalTokens/sec` | `vllm:generation_tokens_total` + `vllm:prompt_tokens_total` (rate) | Combined prompt + output token throughput |
| `maxTokens/sec` | Highest rate observed across the pool | Normalises β to [0, 1] |
| `α` | `alphaKVCache` config (default 1.0) | KV-cache signal weight |
| `β` | `betaTokenRate` config (default 1.0) | Token throughput signal weight |
| `ε` | 1.0 (fixed) | Prevents division by zero; ensures equal distribution when all pods are idle |

**Token throughput rate** is computed as a per-interval delta from Prometheus cumulative counters:

```
totalTokens/sec = (Δgeneration_tokens + Δprompt_tokens) / Δtime
```

Counter resets (e.g. pod restarts) are detected by clamping negative deltas to zero, so a restarted pod starts with a clean rate on the next cycle.

Pods that fail scraping receive `Ratio=1` (minimum), ensuring they still receive a small amount of traffic while Avi's health monitor decides whether to take them out of service.

Ratios always sum to 100. Any rounding error is absorbed by the pod with the highest score.

### Why Token Throughput Matters

Request-count metrics alone cannot distinguish between:
- A pod serving 100 short (10-token) completions — low actual load
- A pod serving 1 long (100k-token) context — extremely high GPU memory and compute load

By incorporating the token throughput rate, AKO steers new requests away from pods that are already generating large amounts of tokens, regardless of how many requests are queued.

### Supported Metrics

| Metric | Type | Used for |
|---|---|---|
| `vllm:num_requests_waiting` | Gauge | Primary queue depth signal |
| `vllm:num_requests_running` | Gauge | Collected, not used in scoring |
| `vllm:kv_cache_usage_perc` | Gauge | Memory pressure signal (α) |
| `vllm:generation_tokens_total` | Counter | Output token rate (β) |
| `vllm:prompt_tokens_total` | Counter | Input token rate (β) |

Other LLM servers (e.g. TGI, Ollama) work if they expose metrics with the same names at `/metrics`.

---

## Tuning

| Scenario | Recommendation |
|---|---|
| Fast-changing load (interactive chat) | Lower `scrapeIntervalSeconds` (5–10s) |
| Stable throughput workloads (batch) | Higher `scrapeIntervalSeconds` (30–60s) |
| Memory-bound models (large KV cache) | Increase `alphaKVCache` (2.0–5.0) |
| Long-context / large output models | Increase `betaTokenRate` (2.0–5.0) |
| Short-context chat, token rate not relevant | Set `betaTokenRate: 0` |
| Ignore KV-cache, use queue + tokens only | Set `alphaKVCache: 0` |
| Queue depth only (original behaviour) | Set `alphaKVCache: 0`, `betaTokenRate: 0` |
| All pods always idle | Weights automatically equal (ε ensures fair distribution) |

---

## Limitations

- **Periodic, not per-request:** Weight adjustment happens on a configurable interval (default 15s), not per-request like the EPP ext-proc approach. Rapid load spikes within a scrape window are not reacted to immediately.
- **No LoRA / adapter awareness:** The current implementation does not route based on which LoRA adapters are loaded on a given pod. This is a Phase 2 consideration.
- **No prefix-cache awareness:** Unlike the EPP's scheduling layer, AKO cannot route requests to pods that have a matching KV-cache prefix. Aggregate load is used instead.
- **Direct pod IP scraping:** AKO scrapes pod IPs directly. Ensure NetworkPolicies allow traffic from the AKO pod to LLM pods on the `targetPort`.
- **InferenceObjective not yet supported:** The `InferenceObjective` CRD (model-name based traffic split) is a Phase 2 item.
- **IPv4 only:** The current implementation constructs pool servers as `V4` address type. IPv6 support is not yet implemented.
- **Token rate unavailable on first scrape:** The rate requires two data points. On the first scrape cycle after startup, `totalTokens/sec` is 0 for all pods and only the queue and KV-cache signals contribute to scoring.

---

## Comparison with ext-proc EPP Approach

| Feature | AKO Native | ext-proc EPP |
|---|---|---|
| Gateway dependency | Any AKO-managed gateway | Must support ext-proc (Envoy-based only) |
| Routing granularity | Periodic weight update (configurable) | Per-request |
| Token-aware routing | Yes (throughput rate via counter deltas) | Yes (per-request token count) |
| LoRA adapter routing | Not supported | Supported |
| Prefix-cache routing | Not supported | Supported |
| Operational complexity | Low (built into AKO) | High (EPP sidecar, ext-proc wiring) |
| Suitable for | Throughput workloads, production LB | Latency-sensitive, KV-cache reuse critical |

---

## Troubleshooting

**Weights not updating:**
- Check AKO logs for `inference scraper` lines
- Verify `INFERENCE_EXTENSION_ENABLED=true` in the ako-gateway-api container: `kubectl exec -n avi-system <ako-pod> -c ako-gateway-api -- env | grep INFERENCE`
- Confirm AKO pod can reach `http://<podIP>:<targetPort>/metrics` — try `kubectl exec` from the AKO pod

**Token rate always zero:**
- Expected on first scrape cycle (needs two data points to compute a delta)
- Check that the LLM server exposes `vllm:generation_tokens_total` and `vllm:prompt_tokens_total`: `curl http://<podIP>:<port>/metrics | grep tokens_total`
- Set `betaTokenRate: 0` to disable the signal if your server does not expose these metrics

**All pods getting equal weight:**
- Expected when all pods are idle (ε ensures fair distribution)
- Check live metrics: `curl http://<podIP>:<port>/metrics | grep -E 'waiting|kv_cache|tokens_total'`

**InferencePool not reconciling:**
- Confirm the InferencePool CRD is installed: `kubectl get crd inferencepools.gateway.inference.x-k8s.io`
- Check that `inferenceExtension.enabled: true` is set and the pod has restarted after the config change

**HTTPRoute not accepting InferencePool backendRef:**
- Confirm `group: gateway.inference.x-k8s.io` and `kind: InferencePool` are set in the `backendRef`
- Check HTTPRoute status conditions for `ResolvedRefs` errors: `kubectl get httproute <name> -o jsonpath='{.status}'`
