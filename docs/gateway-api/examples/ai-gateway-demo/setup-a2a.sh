#!/usr/bin/env bash
# setup-a2a.sh — one-shot setup for the AKO AI Gateway A2A demo
#
# Run AFTER setup.sh (LLM gateway + JWT issuer must already be deployed).
#
# What it does:
#   1. Installs the AIA2ARoutePolicy CRD
#   2. Generates a self-signed TLS cert for the A2A gateway hostnames
#   3. Stores the cert as a Kubernetes Secret (a2a-tls)
#   4. Deploys mock A2A agent backends (ops-agent, security-agent — 3 replicas each)
#   5. Applies a2a-gateway.yaml  → AKO creates a dedicated Avi VS
#   6. Applies a2a-gateway-policies.yaml → AKO attaches A2A DataScripts to each child VS
#   7. Waits for the A2A gateway VIP
#
# Prerequisites:
#   - setup.sh has run successfully (jwt-issuer deployed, LLM gateway up)
#   - kubectl, openssl, curl in PATH
#
# Usage:
#   ./setup-a2a.sh

set -euo pipefail

NAMESPACE="inference"
REPO_ROOT="$(cd "$(dirname "$0")/../../../../.." && pwd)"
DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"

info() { echo "  [$(date +%H:%M:%S)] $*"; }
ok()   { echo "✅ $*"; }
die()  { echo "❌ $*" >&2; exit 1; }

# ─── 0. Preflight ─────────────────────────────────────────────────────────────
info "Checking prerequisites..."
command -v kubectl >/dev/null || die "kubectl not found"
command -v openssl  >/dev/null || die "openssl not found"

# JWT issuer must already be up (shared with LLM gateway). It is ClusterIP —
# accessed from within the cluster at jwt-issuer.inference.svc.cluster.local:8080.
ISSUER_READY=$(kubectl get deployment jwt-issuer -n "$NAMESPACE" \
  -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
[[ "${ISSUER_READY}" -ge 1 ]] || die "jwt-issuer deployment not ready. Has setup.sh been run?"
ISSUER_URL="http://jwt-issuer.${NAMESPACE}.svc.cluster.local:8080"
ok "JWT issuer ready (ClusterIP — access from demo-shell or in-cluster pods)"

# ─── 1. Install AIA2ARoutePolicy CRD ──────────────────────────────────────────
info "Installing AIA2ARoutePolicy CRD..."
kubectl apply -f "$REPO_ROOT/helm/ako/crds/ai.ako.vmware.com_aia2aroutepolicies.yaml"
ok "AIA2ARoutePolicy CRD installed"

# ─── 2. Generate TLS cert for A2A gateway ────────────────────────────────────
TMPDIR_KEYS="$(mktemp -d)"
info "Generating self-signed TLS cert for ops-agent.demo.local + security-agent.demo.local..."
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout "$TMPDIR_KEYS/a2a-tls.key" \
  -out    "$TMPDIR_KEYS/a2a-tls.crt" \
  -subj   "/CN=a2a.demo.local" \
  -addext "subjectAltName=DNS:ops-agent.demo.local,DNS:security-agent.demo.local" \
  2>/dev/null
ok "TLS cert generated"

# ─── 3. Store cert as Kubernetes Secret ──────────────────────────────────────
kubectl delete secret a2a-tls -n "$NAMESPACE" --ignore-not-found >/dev/null
kubectl create secret tls a2a-tls -n "$NAMESPACE" \
  --cert="$TMPDIR_KEYS/a2a-tls.crt" \
  --key="$TMPDIR_KEYS/a2a-tls.key"
ok "Secret a2a-tls created in $NAMESPACE"
rm -rf "$TMPDIR_KEYS"

# ─── 4. Deploy mock A2A agent backends ───────────────────────────────────────
info "Deploying mock A2A agent backends (ops-agent × 3, security-agent × 3)..."
kubectl apply -f "$DEMO_DIR/mock-a2a-agent.yaml"

info "Waiting for ops-agent rollout (up to 120s)..."
kubectl rollout status deployment/ops-agent -n "$NAMESPACE" --timeout=120s
info "Waiting for security-agent rollout (up to 120s)..."
kubectl rollout status deployment/security-agent -n "$NAMESPACE" --timeout=120s
ok "Mock A2A agent pods ready"

# ─── 5. Apply A2A Gateway ─────────────────────────────────────────────────────
info "Applying A2A Gateway (a2a-gateway)..."
kubectl apply -f "$DEMO_DIR/a2a-gateway.yaml"
ok "a2a-gateway applied — AKO will create a dedicated Avi VS"

# ─── 6. Apply A2A policies ────────────────────────────────────────────────────
info "Applying A2A HTTPRoutes + AIA2ARoutePolicies..."
kubectl apply -f "$DEMO_DIR/a2a-gateway-policies.yaml"
ok "A2A policies applied — AKO will attach DataScripts to each child VS"

# ─── 7. Wait for A2A gateway VIP ─────────────────────────────────────────────
info "Waiting for a2a-gateway LoadBalancer address (up to 120s)..."
A2A_VIP=""
for i in $(seq 1 24); do
  A2A_VIP=$(kubectl get gateway a2a-gateway -n "$NAMESPACE" \
    -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)
  [[ -n "$A2A_VIP" ]] && break
  sleep 5
done
[[ -n "$A2A_VIP" ]] || die "Timed out waiting for a2a-gateway VIP. Check Avi is assigning addresses."
ok "A2A gateway VIP: $A2A_VIP"

# ─── 8. Smoke test (from demo-shell inside the cluster) ───────────────────────
info "Smoke-testing agent card discovery from demo-shell..."
HTTP_CODE=$(kubectl exec -n "$NAMESPACE" demo-shell -- \
  curl -sk -o /dev/null -w "%{http_code}" \
  -H "Host: ops-agent.demo.local" \
  "https://${A2A_VIP}/.well-known/agent.json" 2>/dev/null || echo "000")
if [[ "$HTTP_CODE" == "200" ]]; then
  ok "Agent card reachable (HTTP 200)"
else
  echo "⚠️  Agent card returned HTTP $HTTP_CODE — check AKO logs and Avi VS status"
fi

echo ""
echo "══════════════════════════════════════════════════════════════"
echo " A2A setup complete. Run demo.sh from demo-shell for Part C."
echo ""
echo " A2A VIP : $A2A_VIP"
echo ""
echo " To test manually from demo-shell:"
echo "   kubectl exec -it -n inference demo-shell -- bash"
echo "   TOKEN=\$(curl -sf http://jwt-issuer.inference.svc.cluster.local:8080/token?sub=orchestrator\\&agent_id=orchestrator | python3 -c \"import sys,json; print(json.load(sys.stdin)['token'])\")"
echo "   curl -sk -H 'Host: ops-agent.demo.local' \\"
echo "     -H 'Content-Type: application/json' \\"
echo "     -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tasks/send\",\"params\":{\"message\":{\"role\":\"user\",\"parts\":[{\"text\":\"hello\"}]}}}' \\"
echo "     \"https://${A2A_VIP}/?jwt=\$TOKEN\""
echo "══════════════════════════════════════════════════════════════"
