#!/usr/bin/env bash
# setup.sh — one-shot setup for the AKO AI Gateway demo
#
# What it does:
#   1. Installs the two AI Gateway CRDs on the cluster
#   2. Generates an RSA-2048 keypair and stores it as a Kubernetes Secret
#   3. Deploys the mock LLM pods and JWT issuer
#   4. Waits for the jwt-issuer LoadBalancer IP
#   5. Creates the Avi JWTServerProfile + SSO Policy via REST API (Part B)
#   6. Patches ai-gateway-policies.yaml with the real issuer URL
#   7. Applies both AI Gateway policies
#
# Prerequisites:
#   - kubectl configured against your cluster (namespace "inference" exists or will be created)
#   - openssl, curl, python3 in PATH
#   - Avi Controller reachable from this machine (for Part B)
#
# Usage:
#   export AVI_CONTROLLER=10.x.x.x
#   export AVI_USERNAME=admin
#   export AVI_PASSWORD=yourpassword
#   export AVI_TENANT=admin          # optional, defaults to admin
#   ./setup.sh
#
# To run Part A only (token limits, no JWT):
#   SKIP_JWT=1 ./setup.sh

set -euo pipefail

NAMESPACE="inference"
REPO_ROOT="$(cd "$(dirname "$0")/../../../../.." && pwd)"
DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"

AVI_TENANT="${AVI_TENANT:-admin}"
SKIP_JWT="${SKIP_JWT:-0}"

info()  { echo "  [$(date +%H:%M:%S)] $*"; }
ok()    { echo "✅ $*"; }
warn()  { echo "⚠️  $*"; }
die()   { echo "❌ $*" >&2; exit 1; }

# ─── 0. Preflight ─────────────────────────────────────────────────────────────
info "Checking prerequisites..."
command -v kubectl >/dev/null || die "kubectl not found"
command -v openssl  >/dev/null || die "openssl not found"
command -v python3  >/dev/null || die "python3 not found"
if [[ "$SKIP_JWT" != "1" ]]; then
  command -v curl >/dev/null || die "curl not found"
  [[ -n "${AVI_CONTROLLER:-}" ]] || die "AVI_CONTROLLER is not set. Set it or run with SKIP_JWT=1."
  [[ -n "${AVI_USERNAME:-}"   ]] || die "AVI_USERNAME is not set."
  [[ -n "${AVI_PASSWORD:-}"   ]] || die "AVI_PASSWORD is not set."
fi
ok "Prerequisites OK"

# ─── 1. Namespace ─────────────────────────────────────────────────────────────
kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || \
  kubectl create namespace "$NAMESPACE"
ok "Namespace $NAMESPACE ready"

# ─── 2. Install AI Gateway CRDs ───────────────────────────────────────────────
info "Applying AI Gateway CRDs..."
kubectl apply -f "$REPO_ROOT/helm/ako/crds/ai.ako.vmware.com_aigatewayauthpolicies.yaml"
kubectl apply -f "$REPO_ROOT/helm/ako/crds/ai.ako.vmware.com_aitokenratelimitpolicies.yaml"
ok "CRDs installed"

# ─── 3. Deploy mock LLM pods ──────────────────────────────────────────────────
info "Deploying mock LLM pods..."
kubectl apply -f "$DEMO_DIR/mock-llm.yaml"
ok "Mock LLM pods deployed (2 replicas)"

# ─── 4. Generate RSA keypair ──────────────────────────────────────────────────
TMPDIR_KEYS="$(mktemp -d)"
KEY_FILE="$TMPDIR_KEYS/key.pem"
info "Generating RSA-2048 keypair..."
openssl genrsa -out "$KEY_FILE" 2048 2>/dev/null
ok "RSA keypair generated"

# Store as a Kubernetes Secret (idempotent: delete+recreate).
kubectl delete secret jwt-signing-key -n "$NAMESPACE" --ignore-not-found >/dev/null
kubectl create secret generic jwt-signing-key \
  -n "$NAMESPACE" \
  --from-file=key.pem="$KEY_FILE"
ok "Secret jwt-signing-key created in $NAMESPACE"

# ─── 5. Deploy JWT issuer ─────────────────────────────────────────────────────
if [[ "$SKIP_JWT" != "1" ]]; then
  info "Deploying JWT issuer..."
  kubectl apply -f "$DEMO_DIR/jwt-issuer.yaml"

  info "Waiting for jwt-issuer pod to be ready (up to 120s)..."
  kubectl rollout status deployment/jwt-issuer -n "$NAMESPACE" --timeout=120s
  ok "JWT issuer pod ready"

  info "Waiting for jwt-issuer LoadBalancer IP (up to 120s)..."
  ISSUER_VIP=""
  for i in $(seq 1 24); do
    ISSUER_VIP=$(kubectl get svc jwt-issuer -n "$NAMESPACE" \
      -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    [[ -n "$ISSUER_VIP" ]] && break
    sleep 5
  done
  [[ -n "$ISSUER_VIP" ]] || die "Timed out waiting for jwt-issuer LoadBalancer IP. Check Avi is assigning VIPs."
  ISSUER_URL="http://${ISSUER_VIP}:8080"
  ok "JWT issuer reachable at $ISSUER_URL"

  # Smoke-test the JWKS endpoint.
  JWKS=$(curl -sf "$ISSUER_URL/jwks") || die "Could not reach $ISSUER_URL/jwks"
  ok "JWKS fetched: $(echo "$JWKS" | python3 -c 'import sys,json; ks=json.load(sys.stdin)["keys"]; print(f"{len(ks)} key(s), kid={ks[0][\"kid\"]}")')"

  # ─── 6. Create Avi JWTServerProfile ────────────────────────────────────────
  info "Creating Avi JWTServerProfile 'inference-llm-auth-jwt'..."
  AVI_BASE="https://${AVI_CONTROLLER}"
  COOKIE_JAR="$TMPDIR_KEYS/avi-cookie.txt"

  # Login and capture session cookie.
  curl -sf -c "$COOKIE_JAR" -k \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"${AVI_USERNAME}\",\"password\":\"${AVI_PASSWORD}\"}" \
    "${AVI_BASE}/api/login" >/dev/null
  ok "Logged in to Avi Controller at $AVI_CONTROLLER"

  JWK_N=$(echo "$JWKS" | python3 -c 'import sys,json; print(json.load(sys.stdin)["keys"][0]["n"])')
  JWK_E=$(echo "$JWKS" | python3 -c 'import sys,json; print(json.load(sys.stdin)["keys"][0]["e"])')
  JWT_PROFILE_JSON=$(python3 - <<PYEOF
import json
print(json.dumps({
    "name": "inference-llm-auth-jwt",
    "issuer": "${ISSUER_URL}",
    "jwks_keys": {
        "type": "AVI_JWK_SET",
        "keys": [{
            "key_id": "demo-key-1",
            "kty": "RSA",
            "use": "sig",
            "alg": "RS256",
            "n": "${JWK_N}",
            "e": "${JWK_E}"
        }]
    }
}))
PYEOF
)

  # Upsert the JWTServerProfile (delete first if it exists).
  EXISTING_JWT=$(curl -sf -b "$COOKIE_JAR" -k \
    "${AVI_BASE}/api/jwtserverprofile?name=inference-llm-auth-jwt&tenant=${AVI_TENANT}" \
    | python3 -c 'import sys,json; r=json.load(sys.stdin); print(r["results"][0]["uuid"] if r["count"]>0 else "")' 2>/dev/null || echo "")

  if [[ -n "$EXISTING_JWT" ]]; then
    curl -sf -b "$COOKIE_JAR" -k -X DELETE \
      "${AVI_BASE}/api/jwtserverprofile/${EXISTING_JWT}?tenant=${AVI_TENANT}" >/dev/null
  fi

  JWT_PROFILE_UUID=$(curl -sf -b "$COOKIE_JAR" -k \
    -X POST -H "Content-Type: application/json" \
    -d "$JWT_PROFILE_JSON" \
    -H "X-Avi-Tenant: ${AVI_TENANT}" \
    "${AVI_BASE}/api/jwtserverprofile" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["uuid"])')
  ok "JWTServerProfile created (uuid: $JWT_PROFILE_UUID)"

  # ─── 7. Create Avi SSO Policy ───────────────────────────────────────────────
  info "Creating Avi SSO Policy 'inference-llm-auth-sso'..."
  SSO_JSON=$(python3 - <<PYEOF
import json
print(json.dumps({
    "name": "inference-llm-auth-sso",
    "type": "SSO_TYPE_JWT",
    "jwt_profile_ref": f"/api/jwtserverprofile/{\"${JWT_PROFILE_UUID}\"}"
}))
PYEOF
)

  EXISTING_SSO=$(curl -sf -b "$COOKIE_JAR" -k \
    "${AVI_BASE}/api/ssopolicy?name=inference-llm-auth-sso&tenant=${AVI_TENANT}" \
    | python3 -c 'import sys,json; r=json.load(sys.stdin); print(r["results"][0]["uuid"] if r["count"]>0 else "")' 2>/dev/null || echo "")

  if [[ -n "$EXISTING_SSO" ]]; then
    curl -sf -b "$COOKIE_JAR" -k -X DELETE \
      "${AVI_BASE}/api/ssopolicy/${EXISTING_SSO}?tenant=${AVI_TENANT}" >/dev/null
  fi

  curl -sf -b "$COOKIE_JAR" -k \
    -X POST -H "Content-Type: application/json" \
    -H "X-Avi-Tenant: ${AVI_TENANT}" \
    -d "$SSO_JSON" \
    "${AVI_BASE}/api/ssopolicy" >/dev/null
  ok "SSO Policy 'inference-llm-auth-sso' created"

  # ─── 8. Patch issuer URL into policy manifest ───────────────────────────────
  info "Patching issuer URL into ai-gateway-policies.yaml..."
  sed -i.bak "s|http://REPLACE_WITH_ISSUER_VIP:8080|${ISSUER_URL}|g" \
    "$DEMO_DIR/ai-gateway-policies.yaml"
  ok "Issuer URL patched → $ISSUER_URL"
fi

# ─── 9. Apply AI Gateway policies ─────────────────────────────────────────────
info "Applying AI Gateway policies..."
if [[ "$SKIP_JWT" == "1" ]]; then
  kubectl apply -f "$DEMO_DIR/ai-gateway-policies.yaml" -l demo-part=token-limits
else
  kubectl apply -f "$DEMO_DIR/ai-gateway-policies.yaml"
fi
ok "AI Gateway policies applied"

# ─── 10. Clean up temp files ─────────────────────────────────────────────────
rm -rf "$TMPDIR_KEYS"

echo ""
echo "══════════════════════════════════════════════════════════════"
echo " Setup complete. Run ./demo.sh to see the features in action."
echo "══════════════════════════════════════════════════════════════"
if [[ "$SKIP_JWT" != "1" ]]; then
  echo " Issuer URL : $ISSUER_URL"
  echo " Get a token: curl -sf $ISSUER_URL/token?sub=alice&tenant=acme | jq -r .token"
fi
