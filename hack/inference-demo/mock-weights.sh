#!/usr/bin/env bash
# mock-weights.sh — quickly change the simulated load profile of a demo vLLM pod
# so the AKO inference scraper picks up different metrics and re-weights the Avi
# Pool Group members within one scrape interval (~15 s).
#
# Usage:
#   mock-weights.sh <preset> <pod-number>
#   mock-weights.sh show
#   mock-weights.sh reset [pod-number]
#
# Presets (all values are env-vars read by the pod's mock-vllm container):
#
#   idle      WAITING=0  KV_CACHE=0.05  TOKEN_RATE=200   → best score, highest ratio
#   normal    WAITING=3  KV_CACHE=0.40  TOKEN_RATE=100   → mid score
#   overload  WAITING=20 KV_CACHE=0.80  TOKEN_RATE=50    → worst score, lowest ratio
#
# reset restores the original demo defaults per pod:
#   pod-1  WAITING=20  KV_CACHE=0.80  TOKEN_RATE=100
#   pod-2  WAITING=5   KV_CACHE=0.80  TOKEN_RATE=50
#   pod-3  WAITING=0   KV_CACHE=0.10  TOKEN_RATE=200
#
# Expected approximate ratios (3 pods, alpha=0.5 beta=0.3 epsilon=1):
#   idle / normal / overload  →  ~83 / 12 / 5
#   overload / normal / idle  →  ~5  / 12 / 83
#   demo defaults (1/2/3)     →  ~7  / 21 / 72
#
# Requirements: kubectl, access to the cluster, and KUBECONFIG set if non-default.

set -euo pipefail

NAMESPACE="${NAMESPACE:-inference}"

usage() {
  sed -n '/^# Usage:/,/^[^#]/p' "$0" | grep '^#' | sed 's/^# \{0,\}//'
  exit 1
}

die() { echo "ERROR: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Preset definitions
# ---------------------------------------------------------------------------
preset_env() {
  local preset="$1"
  case "$preset" in
    idle)     echo "WAITING=0  KV_CACHE=0.05 TOKEN_RATE=200" ;;
    normal)   echo "WAITING=3  KV_CACHE=0.40 TOKEN_RATE=100" ;;
    overload) echo "WAITING=20 KV_CACHE=0.80 TOKEN_RATE=50"  ;;
    *) die "unknown preset '$preset'. Choose: idle | normal | overload" ;;
  esac
}

# Original demo defaults per pod (used by 'reset')
reset_env() {
  local pod="$1"
  case "$pod" in
    1) echo "WAITING=20 KV_CACHE=0.80 TOKEN_RATE=100" ;;
    2) echo "WAITING=5  KV_CACHE=0.80 TOKEN_RATE=50"  ;;
    3) echo "WAITING=0  KV_CACHE=0.10 TOKEN_RATE=200" ;;
    *) die "unknown pod number '$pod'. Choose: 1 | 2 | 3" ;;
  esac
}

# Apply env vars from a space-separated KEY=VALUE string to a deployment.
apply_env() {
  local deployment="$1"
  local env_str="$2"
  # Convert "WAITING=0 KV_CACHE=0.05 TOKEN_RATE=200" → separate args
  # shellcheck disable=SC2086
  kubectl set env "deployment/${deployment}" -n "${NAMESPACE}" ${env_str}
  echo "  → ${deployment}: ${env_str}"
  echo "  (pod will roll; new metrics picked up within the next scrape interval)"
}

# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------
cmd_show() {
  echo "=== Inference pods in namespace '${NAMESPACE}' ==="
  kubectl get pods -n "${NAMESPACE}" -l app=vllm \
    -o custom-columns='NAME:.metadata.name,STATUS:.status.phase,IP:.status.podIP,NODE:.status.hostIP,READY:.status.containerStatuses[0].ready'
  echo
  echo "=== Deployment env vars ==="
  for i in 1 2 3; do
    local dep="vllm-pod-${i}"
    if kubectl get deployment "${dep}" -n "${NAMESPACE}" &>/dev/null; then
      printf "%-14s  " "${dep}"
      kubectl set env deployment/"${dep}" -n "${NAMESPACE}" --list 2>/dev/null \
        | grep -E 'WAITING|KV_CACHE|TOKEN_RATE' | tr '\n' '  '
      echo
    fi
  done
}

cmd_preset() {
  local preset="$1"
  local pod_num="$2"
  [[ "$pod_num" =~ ^[123]$ ]] || die "pod number must be 1, 2, or 3 (got '$pod_num')"
  local env_str
  env_str="$(preset_env "$preset")"
  echo "Setting pod-${pod_num} → preset '${preset}'"
  apply_env "vllm-pod-${pod_num}" "${env_str}"
}

cmd_reset() {
  local pod_num="${1:-}"
  if [[ -z "$pod_num" ]]; then
    echo "Resetting all pods to demo defaults…"
    for i in 1 2 3; do
      apply_env "vllm-pod-${i}" "$(reset_env "$i")"
    done
  else
    [[ "$pod_num" =~ ^[123]$ ]] || die "pod number must be 1, 2, or 3 (got '$pod_num')"
    echo "Resetting pod-${pod_num} to demo default…"
    apply_env "vllm-pod-${pod_num}" "$(reset_env "$pod_num")"
  fi
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
