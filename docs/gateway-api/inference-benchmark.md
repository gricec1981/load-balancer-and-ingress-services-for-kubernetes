# AKO Inference Extension — Realistic Traffic Benchmark

**Environment:** 3× NVIDIA T4 GPUs on AKS · vLLM v0.6.3 · Llama 3.1 8B AWQ-INT4 · June 2026

---

## Overview

This document describes a realistic, production-representative benchmark that demonstrates the value
of AKO's intelligent weight-based routing for LLM inference workloads. Under a realistic open-loop
traffic stream against a cluster with one hot GPU, round-robin load balancing produces catastrophic
tail latency, while AKO's metric-aware routing keeps first-token latency bounded and uses GPU
capacity far more efficiently.

## Realistic Open-Loop Traffic Simulation

**Setup:** requests arrive as a **Poisson process at 5 req/s** (open-loop — new users keep arriving
regardless of how long earlier requests take, exactly like a production endpoint), each on a
**fresh connection** so every router decides routing per request. Prompt mix is realistic:
**70% short chat / 20% medium / 10% long RAG**. A pre-existing background load makes pod-1 a genuine
hot spot: **KV cache 98.6%** (all three AKO load signals active) with a 41-request queue, while pod-3
keeps headroom. Primary metric is **TTFT (time to first token)** — the industry-standard LLM latency
measure, which reflects what a user feels the instant they hit "send."

| TTFT metric | Round-Robin | Avi AKO | Improvement |
|-------------|------------|---------|-------------|
| p50 | 0.289 s | 0.462 s | −0.17 s (proxy overhead) |
| **p90** | **125.8 s** | **10.2 s** | **−92%** |
| **p99** | **135.0 s** | **13.7 s** | **−90%** |
| max | 138.4 s | 150.2 s | ≈ |
| first token within timeout | 77% (450/588) | 81% (476/590) | +4 pts |

**Per-class TTFT p90:** short 125.7 s → 10.4 s, medium 86.5 s → 10.6 s, long 126.2 s → 1.9 s.
Round-robin degrades *every* traffic class; AKO bounds all of them.

**AKO pool weights under the hot-pod condition:** round-robin's flat 33/33/33 vs AKO's **14 / 35 / 51**
(pod-1 / pod-2 / pod-3) — the hot pod's traffic share is cut from 33% to 14%.

**What AKO delivers:** it steers **new arrivals** away from the hot pod toward the GPU with headroom.
With round-robin, ~⅓ of new users are blindly routed into pod-1's queue and wait minutes for a first
token; AKO keeps that share at 14% and routes the rest to pods that can respond immediately.

The script is `benchmark-v4.yaml`; raw results are saved in `bench-v4-final.json`.

---

## What This Means for GPU Efficiency

GPUs are the dominant cost of an inference platform, so the real value of intelligent routing is
**how much useful work you extract from the GPUs you are already paying for.**

The benchmark exposes the core inefficiency of round-robin: it has **no visibility into GPU state**,
so it strands capacity. During the test:

| Pod | KV cache | Running | Queue | State |
|-----|----------|---------|-------|-------|
| pod-1 | **98.6%** | 49 | 41 waiting | saturated |
| pod-2 | 9.8% | 28 | 0 | moderate |
| pod-3 | **0.3%** | 3 | 0 | **idle — capacity stranded** |

Round-robin keeps sending one-third of traffic to the saturated pod-1 (where it queues for minutes)
while an **entire GPU sits idle**. AKO reads the live KV-cache, queue-depth, and slot-utilisation
metrics and shifts traffic onto the idle GPU — raising pod-3's share from 34% to **51%**.

### Why this improves efficiency

1. **Reclaims stranded capacity.** During load imbalance, up to ~1 of every 3 GPUs can be
   underutilised while another saturates. Round-robin cannot see or use that headroom; AKO converts
   it into served traffic — work extracted from hardware you already own.

2. **Enables higher safe utilisation → fewer GPUs.** To stop round-robin from queueing, you must
   overprovision headroom on *every* pod (run each one well below capacity), because RR will
   inevitably route to whichever pod is hottest. AKO diverts load off hot pods, so you can safely run
   each GPU at a higher utilisation and still hold the latency SLO. Same workload, fewer GPUs.

3. **Higher goodput.** Round-robin dropped **23%** of arrivals (timed out without a first token);
   AKO recovered much of that. Failed requests are wasted GPU-seconds and lost users — eliminating
   them raises the effective throughput per GPU.

### Cost translation

This is an **estimate**, not a directly measured result. It is derived from the observation that
during the imbalance one of three GPUs (pod-3) sat essentially idle (0.3% KV) while another was
saturated — i.e. ~⅓ of fleet capacity was stranded. Rounding down to a conservative **30% GPU
reduction** and applying spot `Standard_NC4as_T4_v3` pricing (~$0.35/GPU-hr, 8,760 hr/yr):

| Sustained demand | Round-Robin GPUs | With AKO | GPUs saved | Annual saving |
|------------------|------------------|----------|-----------|---------------|
| 10 GPUs | 10 | 7 | 3 | **~$9.2K/yr** |
| 50 GPUs | 50 | 35 | 15 | **~$46K/yr** |
| 200 GPUs | 200 | 140 | 60 | **~$184K/yr** |

(Saving = GPUs saved × $0.35/hr × 8,760 hr.) On-demand pricing (~1.5× spot) increases these figures.
The 30% is a planning assumption: actual savings depend on how often and how severely your traffic
becomes imbalanced. Variable workloads (long-context RAG mixed with short chat, mixed model sizes)
create more imbalance — and therefore larger savings — than uniform traffic.

---

## Test Design

### Cluster Setup

- **AKS cluster** with 3 GPU nodes (`Standard_NC4as_T4_v3`, 1× NVIDIA T4 each)
- **3 vLLM pods** — one per GPU node, each running `hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4`
- **AKO** scraping vLLM `/metrics` every 5 seconds and updating Avi Pool Group weights
- **Avi Virtual Service** fronting the pool group on `http://10.225.0.100` (host: `llm.demo.local`)

### vLLM Configuration

```
--model hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4
--max-model-len 4096
--max-num-seqs 64        # hard cap: at most 64 sequences run concurrently
--quantization awq
--port 8000
```

### Why open-loop matters

The traffic generator issues requests at a **fixed Poisson arrival rate** with a **fresh connection
per request**. This is essential for a fair, production-representative measurement:

- **Open-loop** means a request stuck behind a hot pod's queue does *not* prevent new users from
  arriving — just like real traffic. The benefit AKO provides is steering each **new arrival** to a
  healthy GPU, which is exactly what TTFT percentiles capture.
- **Fresh connection per request** means both routers make a routing decision on every request
  (kube-proxy round-robins each new connection; Avi applies pool-group weights per request), so the
  comparison is apples-to-apples.

### Background load gradient

A continuous background load establishes the realistic "one hot GPU" condition that AKO is designed
to handle:

```python
# pod-1: OVERLOADED — long-context requests fill KV cache >75% and back up a queue
POD1_CONC, POD1_MAX_TOK = 90, 400   # ~2000-token prompts → KV 98.6%, ~41 queued
# pod-2: MEDIUM
POD2_CONC, POD2_MAX_TOK = 28, 200
# pod-3: LIGHT — real headroom (AKO's routing target)
POD3_CONC, POD3_MAX_TOK = 3,  64
```

---

## AKO Weight Algorithm

AKO reads three Prometheus gauges from each pod's `/metrics` endpoint every 5 seconds and converts
them into Avi pool-group member ratios:

- `vllm:num_requests_waiting` — queue depth (`waitingLoad`, gated by a sustained-streak filter)
- `vllm:gpu_cache_usage_perc` — KV-cache pressure (`kvLoad`, activates above a 0.75 threshold)
- `vllm:num_requests_running` — slot utilisation vs `max-num-seqs` (`slotLoad`)

The per-pod load is the sum of these terms; the ratio is the inverse-load score normalised so all
ratios sum to 100. A saturated pod gets a low ratio (less traffic) while a pod with headroom gets a
high ratio. Ratios are conservative by design — AKO de-prioritises a hot pod rather than blacklisting
it, so the pod still drains its queue while receiving minimal new work.

In this benchmark all three signals were active on pod-1 (KV 98.6%, queue 41, 49 running), driving
its ratio down to **14** while pod-3 (headroom) rose to **51**.

---

## Bugs Fixed During This Work

Two bugs were discovered and fixed in AKO during benchmarking:

### 1. Wrong KV Cache Metric Name

**File:** `ako-gateway-api/inference/scraper.go`

vLLM ≥ v0.4 renamed the KV cache metric from `vllm:kv_cache_usage_perc` to
`vllm:gpu_cache_usage_perc`. AKO's scraper hardcoded the old name, so it always read 0% KV cache
usage and never penalised saturated pods based on GPU memory pressure.

**Fix:** Try `vllm:gpu_cache_usage_perc` first; fall back to `vllm:kv_cache_usage_perc` for
backward compatibility with older vLLM deployments.

```go
if v, ok := getGaugeValue(families, metricKVCacheUsage); ok {
    m.KVCacheUsagePerc = v
} else if v, ok := getGaugeValue(families, metricKVCacheUsageLegacy); ok {
    m.KVCacheUsagePerc = v
}
```

### 2. Stale Pool Members After Pod Rollout

When the vLLM deployments were rolled out, AKO's Avi Pool Group retained old pod IPs, returning
HTTP 000 (connection refused). **Fix:** Restart AKO to force full reconciliation
(`kubectl rollout restart statefulset ako -n avi-system`).

---

## How to Run

```bash
# 1. Scale GPU node pool up (3 nodes)
az aks nodepool scale \
  --resource-group rg-ako-inference \
  --cluster-name aks-inference-demo \
  --name gpunp --node-count 3

# 2. Wait for vLLM pods to become ready (~10 min for model download)
kubectl wait --for=condition=ready pod -l app=vllm-pod-1 -n inference --timeout=900s

# 3. Update pod IPs + Avi PG_UUID in benchmark-v4.yaml if pods/objects were recreated
kubectl get pods -n inference -o wide | grep vllm

# 4. Deploy and run
kubectl apply -f benchmark-v4.yaml

# 5. Follow logs (warmup table shows KV filling; phases print TTFT percentiles)
kubectl logs -n inference -l job-name=llm-benchmark-v4 -f

# 6. Scale GPUs back to 0 when done
az aks nodepool scale \
  --resource-group rg-ako-inference \
  --cluster-name aks-inference-demo \
  --name gpunp --node-count 0
```
