#!/usr/bin/env bash
# mock-weights.sh — change the simulated load profile of a demo vLLM pod
# WITHOUT restarting it, by POSTing to the pod's /admin HTTP endpoint.
#
# Usage:
#   mock-weights.sh <preset> <pod-number>
#   mock-weights.sh show
#   mock-weights.sh reset [pod-number]
#
# Presets:
#   idle      waiting=0  kv_cache=0.05  token_rate=200   → best score, highest ratio
#   normal    waiting=3  kv_cache=0.40  token_rate=100   → mid score
#   overload  waiting=20 kv_cache=0.80  token_rate=50    → worst score, lowest ratio
#
# reset pushes the env-var defaults from the Deployment spec back into the
# running pod so the runtime state matches the declared spec (no restart).
#
# Expected approximate ratios (3 pods, alpha=1.0 beta=1.0 epsilon=1):
#   idle / normal / overload  →  ~83 / 12 / 5
#   demo defaults (1/2/3)     →  varies with each pod's env vars

set -euo pipefail

NAMESPACE="${NAMESPACE:-inference}"

usage() {
  sed -n '/^# Usage:/,/^[^#]/p' "$0" | grep '^#' | sed 's/^# \{0,\}//'
  exit 1
}

die() { echo "ERROR: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Helper — POST JSON to a pod's /admin endpoint via kubectl exec (no direct
# network access to pod overlay IPs required from the jump box).
# ---------------------------------------------------------------------------
admin_post() {
  local pod_num="$1"
  local json="$2"

  local pod_name
  pod_name="$(kubectl get pod -n "${NAMESPACE}" \
    -l "app=vllm-mock,pod=${pod_num}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [[ -z "$pod_name" ]] && die "no running pod found for vllm-mock-${pod_num} in namespace '${NAMESPACE}'"

  local result
  result=$(kubectl exec -n "${NAMESPACE}" "${pod_name}" -- \
    python3 -c "
import urllib.request, json, sys
req = urllib.request.Request(
    'http://127.0.0.1:8000/admin',
    data=json.dumps(${json}).encode(),
    headers={'Content-Type': 'application/json'},
    method='POST')
resp = urllib.request.urlopen(req, timeout=5)
print(resp.read().decode())
" 2>&1) || die "admin POST failed for ${pod_name}: ${result}"

  echo "  → ${pod_name}: ${json}"
  echo "  → ${result}"
}

# ---------------------------------------------------------------------------
# Preset definitions (Python dict literals passed directly into the exec)
# ---------------------------------------------------------------------------
preset_dict() {
  case "$1" in
    idle)     echo '{"waiting":0,"kv_cache":0.05,"token_rate":200}' ;;
    normal)   echo '{"waiting":3,"kv_cache":0.40,"token_rate":100}' ;;
    overload) echo '{"waiting":20,"kv_cache":0.80,"token_rate":50}' ;;
    *) die "unknown preset '$1'. Choose: idle | normal | overload" ;;
  esac
}

# reset_dict reads the env vars from the Deployment spec so the running state
# matches what's declared (no need to hardcode defaults here).
reset_dict() {
  local pod_num="$1"
  local dep="vllm-mock-${pod_num}"

  local waiting kv rate
  waiting=$(kubectl get deployment "${dep}" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="WAITING")].value}' 2>/dev/null)
  kv=$(kubectl get deployment "${dep}" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="KV_CACHE")].value}' 2>/dev/null)
  rate=$(kubectl get deployment "${dep}" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="TOKEN_RATE")].value}' 2>/dev/null)

  [[ -z "$waiting" || -z "$kv" || -z "$rate" ]] && \
    die "could not read env vars from deployment/${dep}"

  echo "{\"waiting\":${waiting},\"kv_cache\":${kv},\"token_rate\":${rate}}"
}

# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------
cmd_show() {
  echo "=== Inference pods in namespace '${NAMESPACE}' ==="
  kubectl get pods -n "${NAMESPACE}" \
    -o custom-columns='NAME:.metadata.name,STATUS:.status.phase,IP:.status.podIP,NODE:.status.hostIP,READY:.status.containerStatuses[0].ready'
  echo
  echo "=== Live runtime state (from /admin) ==="
  for i in 1 2 3; do
    local pod_name
    pod_name="$(kubectl get pod -n "${NAMESPACE}" \
      -l "app=vllm-mock,pod=${i}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
    [[ -z "$pod_name" ]] && { printf "vllm-mock-%-2s  (not found)\n" "${i}"; continue; }
    local state
    state=$(kubectl exec -n "${NAMESPACE}" "${pod_name}" -- \
      python3 -c "
import urllib.request
resp = urllib.request.urlopen('http://127.0.0.1:8000/admin', timeout=3)
print(resp.read().decode())
" 2>/dev/null || echo '{"error":"unreachable"}')
    printf "vllm-mock-%-2s  %s\n" "${i}" "${state}"
  done
}

cmd_preset() {
  local preset="$1"
  local pod_num="$2"
  [[ "$pod_num" =~ ^[123]$ ]] || die "pod number must be 1, 2, or 3 (got '$pod_num')"
  echo "Setting mock-${pod_num} → preset '${preset}' (no restart)"
  admin_post "${pod_num}" "$(preset_dict "$preset")"
  echo "  (takes effect on next scrape cycle, ~15 s)"
}

cmd_reset() {
  local pod_num="${1:-}"
  if [[ -z "$pod_num" ]]; then
    echo "Resetting all pods to their Deployment env-var defaults (no restart)…"
    for i in 1 2 3; do
      admin_post "${i}" "$(reset_dict "$i")"
    done
  else
    [[ "$pod_num" =~ ^[123]$ ]] || die "pod number must be 1, 2, or 3 (got '$pod_num')"
    echo "Resetting mock-${pod_num} to its Deployment env-var default (no restart)…"
    admin_post "${pod_num}" "$(reset_dict "$pod_num")"
  fi
  echo "  (takes effect on next scrape cycle, ~15 s)"
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
[[ $# -ge 1 ]] || usage

case "$1" in
  show)
    cmd_show
    ;;
  reset)
    cmd_reset "${2:-}"
    ;;
  idle|normal|overload)
    [[ $# -ge 2 ]] || die "usage: $0 <preset> <pod-number>"
    cmd_preset "$1" "$2"
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    die "unknown command '$1'. Valid commands: idle | normal | overload | reset | show"
    ;;
esac
