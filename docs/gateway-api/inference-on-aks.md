# Deploying the AKO Native Inference Extension Demo on AKS

A step-by-step runbook to stand up the **AKO native Gateway API Inference Extension** demo on Azure Kubernetes Service:

- **3× GPU nodes** running vLLM (Llama 3.1 8B Instruct, AWQ-quantized to fit a 16 GB T4)
- **1× Avi Controller + 1× Service Engine** (NSX Advanced Load Balancer)
- **AKO** with the inference extension enabled, scraping vLLM metrics and steering traffic via Avi Pool Group weights
- An `InferencePool` + `HTTPRoute` and a load generator to watch the weights shift live

> **Source of truth:** every API group, field name, annotation, env var, and metric name in this
> runbook has been checked against the AKO inference code in this repo
> ([`ako-gateway-api/inference/`](../../ako-gateway-api/inference/)). Where this runbook differs from
> the older `inference-install.md` / `inference-extension.md` docs, **this file matches the code that
> actually ships in the binary** — see "Read first" below.

---

## ⚠️ Read first — three things that will bite you otherwise

1. **The InferencePool API group is `inference.networking.x-k8s.io/v1alpha2`, NOT `gateway.inference.x-k8s.io/v1`.**
   The AKO binary watches, lists, and GETs InferencePool objects at
   `inference.networking.x-k8s.io` / `v1alpha2` / `inferencepools`
   ([`controller.go`](../../ako-gateway-api/inference/controller.go), `InferencePoolGVR`), and its RBAC
   ([`clusterrole.yaml`](../../helm/ako/templates/clusterrole.yaml)) only grants that group. If you
   install the CRD or create objects in the `gateway.inference.x-k8s.io` group (as the older docs show),
   AKO's informer never sees them and **nothing happens — no pools, no weights, silent no-op.** Use the
   v1alpha2 manifests in Phase 5/6 below and verify the CRD version (Phase 5a).

2. **AKO image and chart are a custom build — install the LOCAL chart, not the upstream OCI chart.**
   The `inferenceExtension.*` values, the `INFERENCE_*` env wiring
   ([`statefulset.yaml`](../../helm/ako/templates/statefulset.yaml)), and the inferencepools RBAC exist
   only in this fork's chart (`./helm/ako`). The stock
   `oci://projects.registry.vmware.com/ako/helm-charts/ako` chart does **not** have this feature. Build
   and push the image (Phase 5b), then `helm install ako ./helm/ako` (Phase 5e).

3. **Avi Controller needs a license.** The Controller + SE software is licensed by Broadcom/VMware. For a
   demo, request an **eval license** (free for the trial window). The Azure VM/compute cost is separate.

The exact Avi *UI* steps vary by Controller version (this targets the 30.x line). Where a step is
version-specific, it's noted.

---

## Prerequisites

- An Azure subscription with quota for `Standard_NC4as_T4_v3` in your target region (request a quota increase if needed — GPU quota is **not** granted by default)
- CLI tools: `az`, `kubectl`, `helm` (v3), `jq`
- A Hugging Face account + token. Llama 3.1 is **gated** — accept Meta's license on the model page first. (License-clean alternatives that skip the gate: `Qwen/Qwen2.5-7B-Instruct-AWQ` or a Mistral 7B AWQ build, both Apache-2.0.)
- A Docker registry (ACR, GHCR, or Docker Hub) to push the custom AKO image

Set some shared variables:

```bash
export RG=rg-ako-inference
export LOC=eastus
export CLUSTER=aks-inference-demo
export NS=inference
```

---

## Phase 1 — AKS cluster + GPU node pool

Use **Azure CNI** so pods get VNet-routable IPs. That matters for AKO's inference scraper, which reaches
pod IPs directly for `/metrics`.

> **⚠️ Data-path caveat (read before committing to Azure CNI):** the inference controller
> *unconditionally* annotates the backing Services with `nodeportlocal.antrea.io/enabled=true`
> ([`controller.go`](../../ako-gateway-api/inference/controller.go), `annotateServicesForNPL`), and the
> translator resolves the Avi SE → pod data path via Antrea **NodePortLocal** lookups
> ([`avi_model_l7_translator.go`](../../ako-gateway-api/nodes/avi_model_l7_translator.go)). The *scraper*
> works fine on Azure CNI (direct pod IPs), so **weights will compute and the Pool Group ratios will
> move** — but without Antrea there is nothing to fulfill NPL, so the SE may have **no working path to
> the backend pods**. Two options:
> - **Demo the weight logic only** (Pool Group ratios shifting in the Avi UI): Azure CNI is fine; you may
>   not get a working end-to-end `curl` to the VIP.
> - **Demo the full data path** (traffic actually served through the SE): deploy **Antrea as a BYO CNI**
>   so NPL ports get allocated. Validate end-to-end before you rely on it.

```bash
az group create -n $RG -l $LOC

# System (CPU) node pool — runs AKO, CoreDNS, etc.
az aks create \
  -g $RG -n $CLUSTER \
  --network-plugin azure \
  --node-count 2 \
  --node-vm-size Standard_D4s_v5 \
  --generate-ssh-keys

# GPU node pool — 3× T4, tainted so only GPU workloads land here
az aks nodepool add \
  -g $RG --cluster-name $CLUSTER \
  -n gpunp \
  --node-count 3 \
  --node-vm-size Standard_NC4as_T4_v3 \
  --node-taints sku=gpu:NoSchedule \
  --labels workload=gpu

az aks get-credentials -g $RG -n $CLUSTER
kubectl get nodes -L workload
```

> **Note on the VNet:** for the Avi Controller to orchestrate the SE and for the SE data path to reach pods, the Controller/SE must sit in the **same VNet** (or a peered VNet) as the cluster. Record the AKS VNet/subnet — you'll need it in Phase 4:
> ```bash
> NODE_RG=$(az aks show -g $RG -n $CLUSTER --query nodeResourceGroup -o tsv)
> az network vnet list -g $NODE_RG -o table
> ```

---

## Phase 2 — Confirm GPUs are schedulable

AKS auto-installs the NVIDIA driver + device plugin for GPU SKUs. Verify each GPU node advertises one GPU:

```bash
kubectl get nodes -l workload=gpu \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}'
```

You want to see `1` next to each of the three nodes. If you see nothing, install the device plugin as a fallback:

```bash
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.15.0/deployments/static/nvidia-device-plugin.yml
```

---

## Phase 3 — Deploy the vLLM model servers

Create the namespace and your HF token secret:

```bash
kubectl create namespace $NS
kubectl create secret generic hf-token \
  -n $NS \
  --from-literal=token='hf_XXXXXXXXXXXXXXXXX'
```

`vllm-deployment.yaml` — 3 replicas, one per GPU node, serving AWQ Llama 3.1 8B. vLLM exposes the OpenAI API **and** Prometheus `/metrics` on port 8000, which is exactly what the InferencePool scrapes.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-llama
  namespace: inference
spec:
  replicas: 3
  selector:
    matchLabels:
      app: vllm
  template:
    metadata:
      labels:
        app: vllm
    spec:
      # one pod per GPU node
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: DoNotSchedule
          labelSelector:
            matchLabels:
              app: vllm
      nodeSelector:
        workload: gpu
      tolerations:
        - key: "sku"
          operator: "Equal"
          value: "gpu"
          effect: "NoSchedule"
      containers:
        - name: vllm
          image: vllm/vllm-openai:latest
          args:
            - "--model"
            - "hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4"
            - "--quantization"
            - "awq"
            - "--max-num-seqs"
            - "128"            # used as the slot-utilisation denominator (see InferencePool annotation)
            - "--max-model-len"
            - "8192"
            - "--gpu-memory-utilization"
            - "0.9"
          ports:
            - containerPort: 8000
          env:
            - name: HUGGING_FACE_HUB_TOKEN
              valueFrom:
                secretKeyRef:
                  name: hf-token
                  key: token
          resources:
            limits:
              nvidia.com/gpu: 1
          readinessProbe:
            httpGet:
              path: /health
              port: 8000
            initialDelaySeconds: 60
            periodSeconds: 10
```

```bash
kubectl apply -f vllm-deployment.yaml
kubectl rollout status deploy/vllm-llama -n $NS --timeout=10m
kubectl get pods -n $NS -o wide   # confirm one pod per GPU node
```

**Sanity-check the metric names — this is critical.** AKO's scraper hardcodes these exact metric names
([`scraper.go`](../../ako-gateway-api/inference/scraper.go)):
`vllm:num_requests_running`, `vllm:num_requests_waiting`, `vllm:kv_cache_usage_perc`. Some vLLM builds
emit `vllm:gpu_cache_usage_perc` instead of `vllm:kv_cache_usage_perc`; if yours does, the KV signal
silently reads 0 and the KV-pressure part of the demo won't fire. Confirm the names verbatim:

```bash
POD=$(kubectl get pod -n $NS -l app=vllm -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n $NS $POD -- \
  curl -s localhost:8000/metrics | grep -E 'num_requests_waiting|kv_cache_usage_perc|num_requests_running'
```

If you see `gpu_cache_usage_perc` but not `kv_cache_usage_perc`, either pin a vLLM version that emits the
latter, or drive the demo with the waiting-queue / slot signals instead (still fully functional).

---

## Phase 4 — Stand up the Avi Controller + Service Engine

This is the part that lives outside Kubernetes. High level: deploy one Controller VM, point it at Azure as a cloud, define a VIP network, and let it create the SE on demand.

1. **Get the Controller image.** From the Azure Marketplace ("VMware NSX Advanced Load Balancer / Avi") or the VHD provided with your Broadcom entitlement. Apply your **eval license**.

2. **Deploy the Controller VM** into the AKS VNet (or a peered VNet):
   - Size: `Standard_D8s_v5` (8 vCPU / 32 GB) + ~128 GB disk for a demo
   - Give it a stable private IP; record it as `AVI_CONTROLLER_IP`

3. **Initial setup wizard** — browse to `https://<AVI_CONTROLLER_IP>`:
   - Set the admin password (record it for AKO)
   - Set DNS + NTP

4. **Configure the Cloud connector (Azure).** This lets the Controller create/manage the SE VM for you:
   - Create an Azure AD **service principal** (or use a managed identity) with **Contributor** on `$RG`/`$NODE_RG`
   - In the Controller: *Infrastructure → Clouds → Create → Azure*, supply the subscription, the resource group, the VNet/subnet, and the SP credentials
   - *Alternative:* a **No-Orchestrator (Linux Server)** cloud where you deploy the SE VM by hand. Azure cloud is less manual for a 1-SE demo.

5. **VIP network + IPAM/DNS.** Create (or pick) a subnet in the VNet for VIPs, then create an **IPAM profile** bound to it and attach it to the cloud. This is where your Gateway's external IP will come from.

6. **Service Engine Group.** *Infrastructure → Service Engine Group* — set **max SEs = 1** so the demo stays at exactly one SE. The SE VM is created automatically when the first VIP (your Gateway) is programmed.

7. **Record for AKO:** Controller IP, admin user/password, cloud name, SE Group name, VIP network name, and the Controller version.

> The first SE won't exist until AKO programs a Virtual Service in Phase 6 — that's expected. After you apply the Gateway, watch *Infrastructure → Service Engines* and one SE will spin up.

---

## Phase 5 — Gateway API CRDs + the AKO inference build

**5a. Install the Gateway API + InferencePool CRDs.**

```bash
# Gateway API (standard channel)
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml
```

For the InferencePool CRD you **must** install a release that serves the
`inference.networking.x-k8s.io/**v1alpha2**` API — that is the group/version the AKO binary watches. The
newer Inference Extension releases (`latest`, v1.x) ship a *different* group and will not be reconciled by
this build. Install an `v1alpha2`-era release of the CRD, then **verify**:

```bash
# Example: an older Inference Extension release that ships the v1alpha2 group.
# Pin to whichever release tag serves inference.networking.x-k8s.io/v1alpha2 — do NOT use 'latest'.
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v0.3.0/manifests.yaml

# VERIFY the CRD exists in the correct group AND serves v1alpha2:
kubectl get crd inferencepools.inference.networking.x-k8s.io \
  -o jsonpath='{.spec.group}{"  versions="}{range .spec.versions[*]}{.name}{" "}{end}{"\n"}'
# Expected output, e.g.:
#   inference.networking.x-k8s.io  versions=v1alpha2
```

If that `kubectl get crd` returns "not found", you installed the wrong group (likely
`inferencepools.gateway.inference.x-k8s.io`). Delete it and install a v1alpha2 release instead — AKO will
not see the `gateway.inference.x-k8s.io` group.

**5b. Build & push the custom AKO image** (the step that makes `inferenceExtension` exist). See
[`inference-install.md`](inference-install.md) Step 3 for detail; in outline, from the repo root:

```bash
cd ~/load-balancer-and-ingress-services-for-kubernetes

# Interactive build + push to your registry (GHCR example):
./hack/build-dev-image.sh ghcr.io/<your-user> inference-ext
# …or:
make dev-build-and-push-gateway-api REGISTRY=ghcr.io/<your-user> TAG=inference-ext
```

If your registry package is private, create a pull secret (see `inference-install.md` Step 4b) and add it
to `image.pullSecrets` in `values.yaml` below.

**5c. Avi credentials secret:**

```bash
kubectl create namespace avi-system
kubectl create secret generic avi-secret \
  -n avi-system \
  --from-literal=username='admin' \
  --from-literal=password='<AVI_ADMIN_PASSWORD>'
```

**5d. AKO `values.yaml`** — the inference-relevant settings; fill the Avi-specific blanks from Phase 4.

```yaml
image:
  repository: ghcr.io/<your-user>/ako-gateway-api
  pullPolicy: IfNotPresent
  # pullSecrets:
  #   - name: ghcr-pull-secret      # only if your registry package is private

featureGates:
  GatewayAPI: true                  # REQUIRED — inferencepools RBAC is gated on this AND enabled below

inferenceExtension:
  enabled: true                     # REQUIRED — defaults to false in the chart
  scrapeIntervalSeconds: 15         # lower to 5 for a snappier demo
  alphaKVCache: 1.0                 # KV-cache pressure weight (α). 0 disables the KV signal.
  betaTokenRate: 1.0                # slot-utilisation weight (β). 0 disables the slot signal.
                                    # NOTE: the name says "token rate" but the implemented formula
                                    # uses β for slot utilisation (num_requests_running / maxNumSeqs).

AKOSettings:
  clusterName: aks-inference-demo

ControllerSettings:
  controllerHost: "<AVI_CONTROLLER_IP>"
  controllerVersion: "30.2.1"        # match your Controller
  cloudName: "<your-azure-cloud-name>"
  serviceEngineGroupName: "<your-se-group>"

NetworkSettings:
  vipNetworkList:
    - networkName: "<your-vip-subnet-name>"
```

> The Avi credentials are consumed from the `avi-secret` you created in 5c — you don't put them in
> `values.yaml`.

**5e. Install AKO from the LOCAL chart** (not the upstream OCI chart — see "Read first" #2):

```bash
helm install ako ./helm/ako \
  -n avi-system --create-namespace \
  -f values.yaml

kubectl get pods -n avi-system
```

The AKO pod has two containers: `ako` (main) and `ako-gateway-api` (the inference build). Confirm the
feature actually loaded:

```bash
# The inference env vars live on the ako-gateway-api container:
kubectl exec -n avi-system ako-0 -c ako-gateway-api -- env | grep INFERENCE
```

You want `INFERENCE_EXTENSION_ENABLED=true` in that output (plus
`INFERENCE_SCRAPE_INTERVAL_SECONDS`, `INFERENCE_ALPHA_KV_CACHE`, `INFERENCE_BETA_TOKEN_RATE`).

---

## Phase 6 — Gateway, InferencePool, HTTPRoute

AKO registers the `avi-lb` GatewayClass automatically. Verify:

```bash
kubectl get gatewayclass avi-lb
```

`gateway.yaml`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: avi-gateway
  namespace: inference
spec:
  gatewayClassName: avi-lb
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: Same
```

`inferencepool.yaml` — **note the v1alpha2 apiVersion and the `targetPortNumber` field name**:

```yaml
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferencePool
metadata:
  name: llm-pool
  namespace: inference
  annotations:
    # Slot-utilisation denominator. Match your vLLM --max-num-seqs.
    # Omit to use AKO's built-in default of 256.
    inference.ako.vmware.com/max-num-seqs: "128"
spec:
  selector:
    matchLabels:
      app: vllm
  targetPortNumber: 8000   # v1alpha2 field; used for BOTH traffic and /metrics scraping
```

`httproute.yaml` — note: **do not** set a `port` on the backendRef; it's taken from the InferencePool's
`targetPortNumber`:

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
        - group: inference.networking.x-k8s.io
          kind: InferencePool
          name: llm-pool
```

Apply and grab the VIP (this also triggers SE creation on the Controller):

```bash
kubectl apply -f gateway.yaml -f inferencepool.yaml -f httproute.yaml

# Confirm AKO actually reconciled the pool (proves the group/version is right):
kubectl logs -n avi-system ako-0 -c ako-gateway-api | grep -E "InferencePool (ADD|UPDATE)|inference scraper: starting loop"

# wait for the external address
kubectl get gateway avi-gateway -n $NS -w
VIP=$(kubectl get gateway avi-gateway -n $NS -o jsonpath='{.status.addresses[0].value}')
echo "VIP = $VIP"

# smoke test (data path — see the Azure CNI / NPL caveat in Phase 1)
curl -s http://$VIP/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4",
       "messages":[{"role":"user","content":"Say hi in five words."}]}'
```

In the Avi UI you should now see: one Virtual Service, a **Pool Group** with three Pools (one per vLLM pod), and one SE.

> If you see the InferencePool ADD / scraper-start log lines but no Pool Group members move under load,
> re-check the metric names (Phase 3). If you see **no** InferencePool log lines at all, the CRD
> group/version is wrong (Phase 5a) — AKO isn't watching your object.

---

## Phase 7 — Drive load and watch the weights shift

The point of the demo is that as some pods fill their KV cache / saturate slots, AKO lowers their Pool Group ratio and steers traffic to the less-loaded pods. To make that visible, push **sustained concurrent load with long outputs** so KV pressure builds unevenly.

`load.sh` (uses [`hey`](https://github.com/rakyll/hey)):

```bash
cat > body.json <<'EOF'
{"model":"hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4",
 "messages":[{"role":"user","content":"Write a long, detailed essay about the history of computing."}],
 "max_tokens":1024}
EOF

# 200 concurrent streams for 5 minutes
hey -z 5m -c 200 -m POST \
  -H "Content-Type: application/json" \
  -D body.json \
  http://$VIP/v1/chat/completions
```

While that runs, watch the three signals in three panes:

```bash
# 1) AKO scraper — confirms it's polling and recomputing ratios
kubectl logs -n avi-system ako-0 -c ako-gateway-api -f | grep -i "inference scraper"

# 2) Live vLLM load per pod
for p in $(kubectl get pod -n $NS -l app=vllm -o name); do
  echo "== $p =="
  kubectl exec -n $NS ${p#pod/} -- \
    curl -s localhost:8000/metrics | grep -E 'num_requests_(waiting|running)|kv_cache_usage_perc'
done

# 3) Avi UI: Applications → Pool Group → Members — watch the Ratio column move
```

What you're demonstrating: the per-pod **Ratio** values (1–100, summing to 100) diverge as load builds,
then rebalance as it drains. By design there's a cycle or two of lag:

- The **KV signal** only contributes above **75%** cache occupancy (ramps from 0 at 0.75 to α at 1.0).
- The **waiting signal** needs **≥2 consecutive scrapes** with a non-empty queue before it activates
  (filters transient single-cycle spikes).
- AKO only writes to Avi when the computed ratios actually **change**, so idle/steady periods produce no
  churn in the logs.

For a more dramatic demo, lower `scrapeIntervalSeconds` to 5 and/or raise `alphaKVCache` to 2.0–5.0 in
the AKO values and `helm upgrade`.

---

## Quick troubleshooting

| Symptom | Check |
|---|---|
| No `InferencePool ADD` log lines at all | CRD is in the wrong group. Must be `inferencepools.inference.networking.x-k8s.io` serving `v1alpha2` (Phase 5a). The `gateway.inference.x-k8s.io` group is **not** watched. |
| Weights never move | `INFERENCE_EXTENSION_ENABLED=true` on the `ako-gateway-api` container; scraper "starting loop" line present |
| KV part of demo never fires | Your vLLM emits `vllm:gpu_cache_usage_perc`, not the hardcoded `vllm:kv_cache_usage_perc` (Phase 3) |
| All pods equal ratio | Expected when idle or in the first 2 scrape cycles; confirm KV cache is actually >75% under load |
| Slot signal always 0 | Confirm `vllm:num_requests_running` is exposed; set `betaTokenRate: 0` to drop the slot term |
| VIP serves no traffic but ratios move | Azure CNI + no Antrea NPL — the SE has no path to pods. See the Phase 1 data-path caveat. |
| HTTPRoute rejected | `backendRef` must have `kind: InferencePool` and **no** `port`; set `group: inference.networking.x-k8s.io` |
| InferencePool not reconciling | `kubectl get crd inferencepools.inference.networking.x-k8s.io`; confirm AKO restarted after config change |
| `max-num-seqs` ignored | Annotation key is exactly `inference.ako.vmware.com/max-num-seqs`; non-positive/unparseable values fall back to the default 256 |

---

## Teardown — stop the GPU bill

The GPUs are the expensive part, so scale them to zero between sessions:

```bash
az aks nodepool scale -g $RG --cluster-name $CLUSTER -n gpunp --node-count 0
```

Full teardown (remember the Avi Controller + SE VMs live in their own/Node resource group — delete those too, or let the cloud connector remove the SE):

```bash
az group delete -n $RG --yes --no-wait
```
