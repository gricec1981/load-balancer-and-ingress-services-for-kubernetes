#!/usr/bin/env bash
# Deploy the AI shim + semantic-cache stack to AKS, fronted by the dedicated
# Avi LLM gateway (stream-llm.demo.local). Images build server-side in ACR
# (no local Docker needed).
#
#   ./deploy.sh                 # build cache-service:latest + deploy
#   TAG=dev1 ./deploy.sh        # build+deploy a pinned tag
set -euo pipefail
cd "$(dirname "$0")"

NS="${NS:-ai-shim}"
ACR="${ACR:-akoinfdemo8161}"
TAG="${TAG:-latest}"
REGISTRY="${ACR}.azurecr.io"

# In-cluster service addresses baked into nginx.conf (rendered here, so the
# container needs no envsubst). RESOLVER = kube-dns/coredns ClusterIP.
RESOLVER="${RESOLVER:-$(kubectl -n kube-system get svc -l k8s-app=kube-dns \
  -o jsonpath='{.items[0].spec.clusterIP}' 2>/dev/null || echo 10.0.0.10)}"
MODEL_BACKEND="mock-vllm.${NS}.svc.cluster.local:8000"
CACHE_BACKEND="cache-service.${NS}.svc.cluster.local:9200"
CLASSIFIER_BACKEND="127.0.0.1:9999"   # placeholder; /_classify is off by default

echo "== rendering nginx.conf (resolver=${RESOLVER}) =="
RENDERED="$(mktemp)"
sed -e "s|\${RESOLVER}|${RESOLVER}|g" \
    -e "s|\${MODEL_BACKEND}|${MODEL_BACKEND}|g" \
    -e "s|\${CACHE_BACKEND}|${CACHE_BACKEND}|g" \
    -e "s|\${CLASSIFIER_BACKEND}|${CLASSIFIER_BACKEND}|g" \
    nginx.conf > "$RENDERED"

echo "== building cache-service image in ACR (${REGISTRY}/cache-service:${TAG}) =="
az acr build -r "$ACR" -t "cache-service:${TAG}" -f cache/Dockerfile cache/

echo "== namespace + configmaps =="
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

kubectl create configmap ai-shim-conf -n "$NS" \
  --from-file=nginx.conf="$RENDERED" \
  --from-file=lua/ \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create configmap mock-vllm-app -n "$NS" \
  --from-file=mock_vllm.py=mock/mock_vllm.py \
  --dry-run=client -o yaml | kubectl apply -f -
rm -f "$RENDERED"

echo "== applying stack + stream-llm gateway =="
kubectl apply -f k8s/shim.yaml
kubectl -n "$NS" set image deploy/cache-service "cache=${REGISTRY}/cache-service:${TAG}"
# pick up any configmap change
kubectl -n "$NS" rollout restart deploy/ai-shim deploy/mock-vllm

kubectl -n "$NS" rollout status deploy/ai-shim
kubectl -n "$NS" rollout status deploy/mock-vllm
kubectl -n "$NS" rollout status deploy/redis-stack
kubectl -n "$NS" rollout status deploy/cache-service

VIP="$(kubectl -n "$NS" get gateway stream-llm-gateway \
  -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
echo
echo "Shim stack ready in namespace/${NS}."
echo "stream-llm gateway VIP: ${VIP:-<pending — re-check: kubectl -n $NS get gateway>}"
echo
echo "Test from the in-cluster demo-shell (host header + --resolve to the VIP):"
echo "  curl -sN --resolve stream-llm.demo.local:80:${VIP:-<VIP>} \\"
echo "    -H 'Content-Type: application/json' -H 'X-Target-Model: llama-3.1-8b' \\"
echo "    http://stream-llm.demo.local/v1/chat/completions \\"
echo "    -d '{\"model\":\"llama-3.1-8b\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
echo
echo "Or shim-direct:  kubectl -n ${NS} port-forward svc/ai-shim 8080:8080"
