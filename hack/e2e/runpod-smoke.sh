#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KUBECTL="${KUBECTL:-kubectl}"
GO="${GO:-go}"
PYTHON="${PYTHON:-python3}"

NODE_NAME="${GPU_BURST_E2E_NODE_NAME:-aibrix-gpu-burst-runpod-e2e}"
NAMESPACE="${GPU_BURST_E2E_NAMESPACE:-default}"
BINARY="${GPU_BURST_E2E_BINARY:-/tmp/neocloud-node-e2e}"
LISTEN_ADDR="${GPU_BURST_E2E_LISTEN_ADDR:-:11251}"
GPU_TYPE="${RUNPOD_E2E_GPU_TYPE:-NVIDIA A40}"
SUCCESS_POD="${RUNPOD_E2E_SUCCESS_POD:-gpu-burst-runpod-e2e-success}"
CAPACITY_POD="${RUNPOD_E2E_CAPACITY_POD:-gpu-burst-runpod-e2e-capacity}"
SUCCESS_TIMEOUT_SECONDS="${RUNPOD_E2E_SUCCESS_TIMEOUT_SECONDS:-600}"
COMPLETION_TIMEOUT_SECONDS="${RUNPOD_E2E_COMPLETION_TIMEOUT_SECONDS:-900}"
CAPACITY_TIMEOUT_SECONDS="${RUNPOD_E2E_CAPACITY_TIMEOUT_SECONDS:-90}"
WAIT_FOR_COMPLETION="${RUNPOD_E2E_WAIT_FOR_COMPLETION:-0}"
RUN_CAPACITY_CHECK="${RUNPOD_E2E_CAPACITY_CHECK:-0}"
EXTERNAL_NAMES=()
CONTROLLER_PID=""

log() {
  printf '[runpod-e2e] %s\n' "$*" >&2
}

fail() {
  log "ERROR: $*"
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

require_env() {
  local name="$1"
  [[ -n "${!name:-}" ]] || fail "$name is required"
}

runpod_api() {
  local method="$1"
  local path="$2"
  "$PYTHON" - "$method" "$path" <<'PY'
import os
import sys
import urllib.error
import urllib.request

method = sys.argv[1]
path = sys.argv[2]
base = os.environ.get("RUNPOD_BASE_URL", "https://rest.runpod.io/v1").rstrip("/")
headers = {
    "Authorization": "Bearer " + os.environ["RUNPOD_API_KEY"],
    "Content-Type": "application/json",
}
request = urllib.request.Request(base + path, headers=headers, method=method)
try:
    with urllib.request.urlopen(request, timeout=30) as response:
        body = response.read().decode("utf-8", "replace")
        print(response.status)
        print(body)
except urllib.error.HTTPError as exc:
    body = exc.read().decode("utf-8", "replace")
    print(exc.code)
    print(body)
    sys.exit(0)
PY
}

runpod_list_by_name() {
  local name="$1"
  "$PYTHON" - "$name" <<'PY'
import json
import os
import sys
import urllib.parse
import urllib.request

name = sys.argv[1]
base = os.environ.get("RUNPOD_BASE_URL", "https://rest.runpod.io/v1").rstrip("/")
query = urllib.parse.urlencode({"name": name, "includeMachine": "true"})
request = urllib.request.Request(
    base + "/pods?" + query,
    headers={"Authorization": "Bearer " + os.environ["RUNPOD_API_KEY"]},
)
with urllib.request.urlopen(request, timeout=30) as response:
    pods = json.loads(response.read().decode("utf-8"))
for pod in pods:
    print(pod.get("id", ""))
PY
}

delete_runpod_by_id() {
  local pod_id="$1"
  [[ -n "$pod_id" ]] || return 0
  log "deleting RunPod pod $pod_id"
  runpod_api DELETE "/pods/${pod_id}" >/dev/null || true
}

delete_runpod_by_external_name() {
  local external_name="$1"
  [[ -n "$external_name" ]] || return 0
  local pod_id
  while IFS= read -r pod_id; do
    delete_runpod_by_id "$pod_id"
  done < <(runpod_list_by_name "$external_name" || true)
}

cleanup_kubernetes_objects() {
  "$KUBECTL" -n "$NAMESPACE" delete pod "$SUCCESS_POD" "$CAPACITY_POD" \
    --ignore-not-found=true --wait=true --timeout=60s >/dev/null 2>&1 || true

  local binding
  for pod_name in "$SUCCESS_POD" "$CAPACITY_POD"; do
    while IFS= read -r binding; do
      [[ -n "$binding" ]] || continue
      "$KUBECTL" -n "$NAMESPACE" patch cloudpodbinding "$binding" \
        --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
      "$KUBECTL" -n "$NAMESPACE" delete cloudpodbinding "$binding" \
        --ignore-not-found=true >/dev/null 2>&1 || true
    done < <("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
      -l "gpu-burst-node.aibrix.ai/pod-name=${pod_name}" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  done

  "$KUBECTL" delete node "$NODE_NAME" --ignore-not-found=true >/dev/null 2>&1 || true
}

cleanup() {
  local exit_code=$?
  set +e
  if [[ -n "$CONTROLLER_PID" ]]; then
    kill "$CONTROLLER_PID" >/dev/null 2>&1 || true
    wait "$CONTROLLER_PID" >/dev/null 2>&1 || true
  fi
  for external_name in "${EXTERNAL_NAMES[@]}"; do
    delete_runpod_by_external_name "$external_name"
  done
  cleanup_kubernetes_objects
  exit "$exit_code"
}
trap cleanup EXIT

managed_external_name() {
  local namespace="$1"
  local uid="$2"
  local raw="${namespace}/${uid}"
  raw="${raw,,}"
  raw="${raw//\//-}"
  raw="${raw//_/-}"
  raw="${raw//./-}"
  raw="${raw//:/-}"
  printf 'gpu-burst-node-%s\n' "$raw"
}

apply_control_plane_prereqs() {
  "$KUBECTL" create namespace gpu-burst-node-system \
    --dry-run=client -o yaml | "$KUBECTL" apply -f -
  awk 'BEGIN { doc=0 } /^---$/ { doc++; if (doc >= 2) exit } { print }' \
    "$ROOT_DIR/deploy/gpu-burst-node.yaml" | "$KUBECTL" apply -f -
}

build_binary() {
  log "building $BINARY"
  "$GO" build -o "$BINARY" "$ROOT_DIR/cmd/neocloud-node"
}

start_controller() {
  log "starting local RunPod controller on $NODE_NAME"
  RUNPOD_API_KEY="$RUNPOD_API_KEY" "$BINARY" \
    --kubeconfig="${KUBECONFIG:-$HOME/.kube/config}" \
    --node-name="$NODE_NAME" \
    --provider=runpod \
    --binding-store=kubernetes \
    --listen-addr="$LISTEN_ADDR" \
    --status-sync-interval="${RUNPOD_E2E_STATUS_SYNC_INTERVAL:-5s}" \
    --advertise-gpu-count=3 \
    --advertise-pod-capacity=10 \
    --max-gpu-count=1 &
  CONTROLLER_PID=$!
  local deadline=$((SECONDS + 60))
  until "$KUBECTL" get "node/${NODE_NAME}" >/dev/null 2>&1; do
    (( SECONDS < deadline )) || fail "timed out waiting for node/${NODE_NAME} to be created"
    sleep 1
  done
  "$KUBECTL" wait --for=condition=Ready "node/${NODE_NAME}" --timeout=60s
}

apply_runpod_pod() {
  local pod_name="$1"
  local cloud_type="$2"
  local market_type="$3"

  {
    cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${NAMESPACE}
  labels:
    gpu-burst-node.aibrix.ai/e2e: runpod
  annotations:
    gpu-burst-node.aibrix.ai/provider: runpod
    gpu-burst-node.aibrix.ai/gpu-type: ${GPU_TYPE}
YAML
    if [[ -n "$cloud_type" ]]; then
      printf '    gpu-burst-node.aibrix.ai/runpod-cloud-type: %s\n' "$cloud_type"
    fi
    if [[ -n "$market_type" ]]; then
      printf '    gpu-burst-node.aibrix.ai/market-type: %s\n' "$market_type"
    fi
    cat <<YAML
spec:
  restartPolicy: Never
  nodeSelector:
    gpu-burst-node.aibrix.ai/provider: runpod
  tolerations:
    - key: gpu-burst-node.aibrix.ai/provider
      operator: Equal
      value: runpod
      effect: NoSchedule
  containers:
    - name: smoke
      image: busybox:1.36
      command: ["sh", "-c"]
      args: ["echo ${pod_name}; sleep 5"]
      resources:
        limits:
          nvidia.com/gpu: "1"
YAML
  } | "$KUBECTL" apply -f -
}

remember_external_name() {
  local pod_name="$1"
  local uid
  uid="$("$KUBECTL" -n "$NAMESPACE" get pod "$pod_name" -o jsonpath='{.metadata.uid}')"
  EXTERNAL_NAMES+=("$(managed_external_name "$NAMESPACE" "$uid")")
}

pod_phase() {
  "$KUBECTL" -n "$NAMESPACE" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true
}

pod_reason() {
  "$KUBECTL" -n "$NAMESPACE" get pod "$1" -o jsonpath='{.status.reason}' 2>/dev/null || true
}

binding_remote_id_for_pod() {
  "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
    -l "gpu-burst-node.aibrix.ai/pod-name=${1}" \
    -o jsonpath='{range .items[*]}{.status.remoteID}{end}' 2>/dev/null || true
}

binding_condition_reason_for_pod() {
  "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
    -l "gpu-burst-node.aibrix.ai/pod-name=${1}" \
    -o jsonpath='{range .items[*]}{.status.conditions[0].reason}{end}' 2>/dev/null || true
}

wait_for_started_remote() {
  local pod_name="$1"
  local timeout="$2"
  local deadline=$((SECONDS + timeout))
  local phase reason remote_id

  "$KUBECTL" wait --for=jsonpath='{.spec.nodeName}'="$NODE_NAME" \
    "pod/${pod_name}" -n "$NAMESPACE" --timeout=60s >&2

  while (( SECONDS < deadline )); do
    phase="$(pod_phase "$pod_name")"
    reason="$(pod_reason "$pod_name")"
    remote_id="$(binding_remote_id_for_pod "$pod_name")"
    log "pod=${pod_name} phase=${phase:-unknown} reason=${reason:-none} remote=${remote_id:-none}"
    if [[ -n "$remote_id" && ( "$phase" == "Running" || "$phase" == "Succeeded" ) ]]; then
      printf '%s\n' "$remote_id"
      return 0
    fi
    if [[ "$phase" == "Failed" ]]; then
      "$KUBECTL" -n "$NAMESPACE" describe pod "$pod_name" >&2 || true
      "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
        -l "gpu-burst-node.aibrix.ai/pod-name=${pod_name}" -o yaml >&2 || true
      return 1
    fi
    sleep 3
  done
  "$KUBECTL" -n "$NAMESPACE" describe pod "$pod_name" >&2 || true
  fail "timed out waiting for ${pod_name} to start with remoteID"
}

wait_for_succeeded() {
  local pod_name="$1"
  local timeout="$2"
  local deadline=$((SECONDS + timeout))
  local phase reason

  while (( SECONDS < deadline )); do
    phase="$(pod_phase "$pod_name")"
    reason="$(pod_reason "$pod_name")"
    log "pod=${pod_name} phase=${phase:-unknown} reason=${reason:-none}"
    if [[ "$phase" == "Succeeded" ]]; then
      return 0
    fi
    if [[ "$phase" == "Failed" ]]; then
      "$KUBECTL" -n "$NAMESPACE" describe pod "$pod_name" >&2 || true
      "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
        -l "gpu-burst-node.aibrix.ai/pod-name=${pod_name}" -o yaml >&2 || true
      fail "${pod_name} failed while waiting for completion"
    fi
    sleep 5
  done
  "$KUBECTL" -n "$NAMESPACE" describe pod "$pod_name" >&2 || true
  "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
    -l "gpu-burst-node.aibrix.ai/pod-name=${pod_name}" -o yaml >&2 || true
  fail "timed out waiting for ${pod_name} to Succeed"
}

wait_for_failed_class() {
  local pod_name="$1"
  local expected_reason="$2"
  local timeout="$3"
  local deadline=$((SECONDS + timeout))
  local phase reason

  "$KUBECTL" wait --for=jsonpath='{.spec.nodeName}'="$NODE_NAME" \
    "pod/${pod_name}" -n "$NAMESPACE" --timeout=60s >&2

  while (( SECONDS < deadline )); do
    phase="$(pod_phase "$pod_name")"
    reason="$(binding_condition_reason_for_pod "$pod_name")"
    log "pod=${pod_name} phase=${phase:-unknown} conditionReason=${reason:-none}"
    if [[ "$phase" == "Failed" ]]; then
      [[ "$reason" == "$expected_reason" ]] || fail "expected ${expected_reason}, got ${reason:-empty}"
      return 0
    fi
    if [[ "$phase" == "Running" ]]; then
      log "${pod_name} became Running; capacity was available, deleting instead of failing"
      return 0
    fi
    sleep 3
  done
  fail "timed out waiting for ${pod_name} to fail with ${expected_reason}"
}

delete_pod_and_verify_cleanup() {
  local pod_name="$1"
  local remote_id="${2:-}"

  "$KUBECTL" -n "$NAMESPACE" delete pod "$pod_name" \
    --ignore-not-found=true --wait=true --timeout=90s

  local bindings
  bindings="$("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
    -l "gpu-burst-node.aibrix.ai/pod-name=${pod_name}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
  [[ -z "$bindings" ]] || fail "CloudPodBinding still exists for ${pod_name}: ${bindings}"

  if [[ -n "$remote_id" ]]; then
    local response status
    response="$(runpod_api GET "/pods/${remote_id}")"
    status="$(printf '%s\n' "$response" | sed -n '1p')"
    [[ "$status" == "404" ]] || fail "RunPod pod ${remote_id} still exists, GET status=${status}"
  fi
}

run_success_smoke() {
  log "running A40 create/delete smoke"
  apply_runpod_pod "$SUCCESS_POD" "" ""
  remember_external_name "$SUCCESS_POD"
  local remote_id
  remote_id="$(wait_for_started_remote "$SUCCESS_POD" "$SUCCESS_TIMEOUT_SECONDS")"
  log "created RunPod pod remoteID=${remote_id}"
  if [[ "$WAIT_FOR_COMPLETION" == "1" ]]; then
    log "waiting for RunPod completion; this requires provider status or runner callback support"
    wait_for_succeeded "$SUCCESS_POD" "$COMPLETION_TIMEOUT_SECONDS"
  fi
  delete_pod_and_verify_cleanup "$SUCCESS_POD" "$remote_id"
}

run_capacity_smoke() {
  log "running optional A40 COMMUNITY spot capacity classification check"
  apply_runpod_pod "$CAPACITY_POD" "COMMUNITY" "spot"
  remember_external_name "$CAPACITY_POD"
  wait_for_failed_class "$CAPACITY_POD" "Capacity" "$CAPACITY_TIMEOUT_SECONDS"
  delete_pod_and_verify_cleanup "$CAPACITY_POD" ""
}

final_orphan_check() {
  local external_name matches
  for external_name in "${EXTERNAL_NAMES[@]}"; do
    matches="$(runpod_list_by_name "$external_name" | sed '/^$/d' || true)"
    [[ -z "$matches" ]] || fail "RunPod resources still exist for ${external_name}: ${matches}"
  done
}

main() {
  [[ "${RUNPOD_E2E:-0}" == "1" ]] || fail "set RUNPOD_E2E=1 to run real RunPod e2e"
  require_env RUNPOD_API_KEY
  require_command "$KUBECTL"
  require_command "$GO"
  require_command "$PYTHON"

  apply_control_plane_prereqs
  build_binary
  cleanup_kubernetes_objects
  start_controller
  run_success_smoke
  if [[ "$RUN_CAPACITY_CHECK" == "1" ]]; then
    run_capacity_smoke
  fi
  final_orphan_check
  log "RunPod e2e smoke passed"
}

main "$@"
