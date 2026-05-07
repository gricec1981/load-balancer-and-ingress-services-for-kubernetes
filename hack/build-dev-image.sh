#!/usr/bin/env bash
# hack/build-dev-image.sh
#
# Builds and pushes a dev ako-gateway-api container image using public base
# images. No VMware-internal registry access required.
#
# Usage:
#   ./hack/build-dev-image.sh [REGISTRY] [TAG]
#
# Examples:
#   ./hack/build-dev-image.sh
#   ./hack/build-dev-image.sh ghcr.io/gricec1981 inference-ext
#   ./hack/build-dev-image.sh docker.io/myuser ako-gw-dev
#
# Prerequisites:
#   - Docker installed and running
#   - Logged in to the target registry (docker login / gh auth login)
#   - Go 1.24+ (only needed if you want a local binary too)

set -euo pipefail

REGISTRY="${1:-ghcr.io/gricec1981}"
TAG="${2:-inference-ext}"
IMAGE="${REGISTRY}/ako-gateway-api:${TAG}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "============================================"
echo "  AKO Gateway API — Dev Image Builder"
echo "============================================"
echo "  Image:    ${IMAGE}"
echo "  Platform: linux/amd64"
echo "  Source:   ${REPO_ROOT}"
echo ""

# Verify we are on the right branch.
BRANCH="$(git -C "${REPO_ROOT}" rev-parse --abbrev-ref HEAD)"
COMMIT="$(git -C "${REPO_ROOT}" rev-parse --short HEAD)"
echo "  Branch:   ${BRANCH}"
echo "  Commit:   ${COMMIT}"
echo ""

# Build
echo ">>> Building image..."
docker build \
  --platform linux/amd64 \
  --label "git.branch=${BRANCH}" \
  --label "git.commit=${COMMIT}" \
  --build-arg AKO_LDFLAGS="-X 'main.version=dev-${COMMIT}'" \
  -t "${IMAGE}" \
  -f "${REPO_ROOT}/Dockerfile.ako-gateway-api-dev" \
  "${REPO_ROOT}"

echo ""
echo ">>> Build complete: ${IMAGE}"
echo ""

# Push
read -r -p "Push to registry? [y/N] " answer
if [[ "${answer}" =~ ^[Yy]$ ]]; then
  echo ">>> Pushing..."
  docker push "${IMAGE}"
  echo ""
  echo ">>> Pushed: ${IMAGE}"
  echo ""
  echo "Update your values.yaml:"
  echo ""
  echo "  GatewayAPI:"
  echo "    image:"
  echo "      repository: ${REGISTRY}/ako-gateway-api"
  echo "      pullPolicy: Always"
  echo ""
  echo "  inferenceExtension:"
  echo "    enabled: true"
  echo "    scrapeIntervalSeconds: 15"
  echo "    alphaKVCache: 1.0"
  echo "    betaTokenRate: 1.0"
  echo ""
  echo "Then deploy:"
  echo "  helm upgrade ako ./helm/ako -n avi-system -f values.yaml \\"
  echo "    --set GatewayAPI.image.repository=${REGISTRY}/ako-gateway-api \\"
  echo "    --set GatewayAPI.image.tag=${TAG}"
else
  echo "Skipping push. Run manually:"
  echo "  docker push ${IMAGE}"
fi
