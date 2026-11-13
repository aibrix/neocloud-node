#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KUBECTL="${KUBECTL:-kubectl}"
GO="${GO:-go}"
PYTHON="${PYTHON:-python3}"

NODE_NAME="${GPU_BURST_E2E_NODE_NAME:-neocloud-node-runpod-e2e}"
NAMESPACE="${GPU_BURST_E2E_NAMESPACE:-default}"
BINARY="${GPU_BURST_E2E_BINARY:-/tmp/neocloud-node-e2e}"
LISTEN_ADDR="${GPU_BURST_E2E_LISTEN_ADDR:-:11251}"
GPU_TYPE="${RUNPOD_E2E_GPU_TYPE:-NVIDIA A40}"
CLOUD_TYPE="${RUNPOD_E2E_CLOUD_TYPE:-SECURE}"
SUCCESS_POD="${RUNPOD_E2E_SUCCESS_POD:-neocloud-node-runpod-e2e-success}"
SPOT_POD="${RUNPOD_E2E_SPOT_POD:-neocloud-node-runpod-e2e-spot}"
SUCCESS_TIMEOUT_SECONDS="${RUNPOD_E2E_SUCCESS_TIMEOUT_SECONDS:-600}"
COMPLETION_TIMEOUT_SECONDS="${RUNPOD_E2E_COMPLETION_TIMEOUT_SECONDS:-900}"
SPOT_TIMEOUT_SECONDS="${RUNPOD_E2E_SPOT_TIMEOUT_SECONDS:-90}"
WAIT_FOR_COMPLETION="${RUNPOD_E2E_WAIT_FOR_COMPLETION:-0}"
RUN_SPOT_CHECK="${RUNPOD_E2E_SPOT_CHECK:-0}"
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
base = os.environ.get("RUNPOD_BASE_URL", "https://api.runpod.io/v2").rstrip("/")
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
base = os.environ.get("RUNPOD_BASE_URL", "https://api.runpod.io/v2").rstrip("/")
cursor = None
seen = set()
matching_ids = []
while True:
    query = urllib.parse.urlencode({"cursor": cursor}) if cursor else ""
    request = urllib.request.Request(
        base + "/pods" + ("?" + query if query else ""),
        headers={"Authorization": "Bearer " + os.environ["RUNPOD_API_KEY"]},
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        page = json.load(response)
    if not isinstance(page, dict) or not isinstance(page.get("pods"), list):
        raise RuntimeError("RunPod list response has no pods array")
    pagination = page.get("pagination")
    if not isinstance(pagination, dict) or not isinstance(pagination.get("hasNextPage"), bool):
        raise RuntimeError("RunPod list response has no valid pagination")
    for pod in page["pods"]:
        if pod.get("name") == name and pod.get("status") != "TERMINATED":
            if not pod.get("id"):
                raise RuntimeError("Matching RunPod resource has no ID")
            matching_ids.append(pod["id"])
    if not pagination["hasNextPage"]:
        break
    cursor = pagination.get("nextCursor")
    if not isinstance(cursor, str) or not cursor or cursor in seen:
        raise RuntimeError("RunPod pagination cursor is missing or repeated")
    seen.add(cursor)
for pod_id in matching_ids:
    print(pod_id)
PY
}

delete_runpod_by_id() {
  local pod_id="$1"
  [[ -n "$pod_id" ]] || return 0
  log "deleting RunPod pod $pod_id"
  local response status
  response="$(runpod_api DELETE "/pods/${pod_id}")" || return 1
  status="$(printf '%s\n' "$response" | sed -n '1p')"
  [[ "$status" == "204" || "$status" == "404" ]] || return 1
  response="$(runpod_api GET "/pods/${pod_id}")" || return 1
  status="$(printf '%s\n' "$response" | sed -n '1p')"
  [[ "$status" == "404" ]]
}

delete_runpod_by_external_name() {
  local external_name="$1"
  [[ -n "$external_name" ]] || return 0
  local pod_id matches failed=0
  matches="$(runpod_list_by_name "$external_name")" || return 1
  while IFS= read -r pod_id; do
    [[ -n "$pod_id" ]] || continue
    delete_runpod_by_id "$pod_id" || failed=1
  done <<< "$matches"
  return "$failed"
}

cleanup_kubernetes_objects() {
  "$KUBECTL" -n "$NAMESPACE" delete pod "$SUCCESS_POD" "$SPOT_POD" \
    --ignore-not-found=true --wait=true --timeout=60s >/dev/null 2>&1 || true

  local binding
  for pod_name in "$SUCCESS_POD" "$SPOT_POD"; do
    while IFS= read -r binding; do
      [[ -n "$binding" ]] || continue
      "$KUBECTL" -n "$NAMESPACE" patch cloudpodbinding "$binding" \
        --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
      "$KUBECTL" -n "$NAMESPACE" delete cloudpodbinding "$binding" \
        --ignore-not-found=true >/dev/null 2>&1 || true
    done < <("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
      -l "cloud.aibrix.ai/pod-name=${pod_name}" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  done

  "$KUBECTL" delete node "$NODE_NAME" --ignore-not-found=true >/dev/null 2>&1 || true
}

cleanup() {
  local exit_code=$?
  local cleanup_failed=0
  set +e
  if [[ -n "$CONTROLLER_PID" ]]; then
    kill "$CONTROLLER_PID" >/dev/null 2>&1 || true
    wait "$CONTROLLER_PID" >/dev/null 2>&1 || true
  fi
  for external_name in "${EXTERNAL_NAMES[@]}"; do
    if ! delete_runpod_by_external_name "$external_name"; then
      log "ERROR: remote cleanup could not be verified for $external_name"
      exit_code=1
      cleanup_failed=1
    fi
  done
  if [[ "$cleanup_failed" == "0" ]]; then
    cleanup_kubernetes_objects
  else
    log "Keeping Kubernetes bindings for cleanup recovery"
  fi
  exit "$exit_code"
}

managed_external_name() {
  local namespace="$1"
  local uid="$2"
  local raw="${namespace}/${uid}"
  raw="$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]')"
  raw="${raw//\//-}"
  raw="${raw//_/-}"
  raw="${raw//./-}"
  raw="${raw//:/-}"
  printf 'neocloud-node-%s\n' "$raw"
}

apply_control_plane_prereqs() {
  local crd
  crd="$("$KUBECTL" get crd cloudpodbindings.cloud.aibrix.ai --ignore-not-found -o name)" || fail "could not check the binding CRD"
  if [[ -n "$crd" ]]; then
    return
  fi
  "$KUBECTL" create namespace aibrix-system \
    --dry-run=client -o yaml | "$KUBECTL" apply -f -
  awk 'BEGIN { doc=0 } /^---$/ { doc++; if (doc >= 2) exit } { print }' \
    "$ROOT_DIR/deploy/neocloud-node.yaml" | "$KUBECTL" apply -f -
}

check_test_resources_absent() {
  local existing
  existing="$("$KUBECTL" get node "$NODE_NAME" --ignore-not-found -o name)" || fail "could not check the test node"
  [[ -z "$existing" ]] || fail "test node already exists: $NODE_NAME"
  existing="$("$KUBECTL" -n "$NAMESPACE" get pods "$SUCCESS_POD" "$SPOT_POD" --ignore-not-found -o name)" || fail "could not check test Pods"
  [[ -z "$existing" ]] || fail "test Pods already exist; choose unused names"
}

build_binary() {
  log "building $BINARY"
  "$GO" build -o "$BINARY" "$ROOT_DIR/cmd/neocloud-node"
}

start_controller() {
  log "starting local RunPod controller on $NODE_NAME"
  GPU_BURST_RUNPOD_BASE_URL="$RUNPOD_BASE_URL" RUNPOD_API_KEY="$RUNPOD_API_KEY" "$BINARY" \
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
    cloud.aibrix.ai/e2e: runpod
  annotations:
    cloud.aibrix.ai/provider: runpod
    cloud.aibrix.ai/gpu-type: ${GPU_TYPE}
YAML
    if [[ -n "$cloud_type" ]]; then
      printf '    cloud.aibrix.ai/runpod-cloud-type: %s\n' "$cloud_type"
    fi
    if [[ -n "$market_type" ]]; then
      printf '    cloud.aibrix.ai/market-type: %s\n' "$market_type"
    fi
    cat <<YAML
spec:
  nodeName: ${NODE_NAME}
  restartPolicy: Never
  nodeSelector:
    cloud.aibrix.ai/provider: runpod
  tolerations:
    - key: cloud.aibrix.ai/provider
      operator: Equal
      value: runpod
      effect: NoSchedule
  containers:
    - name: smoke
      image: busybox:1.36
      command: ["sh", "-c"]
      args: ["echo ${pod_name}; sleep 120"]
      resources:
        limits:
          nvidia.com/gpu: "1"
YAML
  } | "$KUBECTL" apply -f -
}

remember_external_name() {
  local pod_name="$1"
  local uid external_name
  uid="$("$KUBECTL" -n "$NAMESPACE" get pod "$pod_name" -o jsonpath='{.metadata.uid}')"
  external_name="$(managed_external_name "$NAMESPACE" "$uid")"
  EXTERNAL_NAMES+=("$external_name")
  log "tracking external name $external_name"
}

pod_phase() {
  "$KUBECTL" -n "$NAMESPACE" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true
}

pod_reason() {
  "$KUBECTL" -n "$NAMESPACE" get pod "$1" -o jsonpath='{.status.reason}' 2>/dev/null || true
}

pod_terminated_reason() {
  "$KUBECTL" -n "$NAMESPACE" get pod "$1" \
    -o jsonpath='{.status.containerStatuses[0].state.terminated.reason}' 2>/dev/null || true
}

binding_remote_id_for_pod() {
  "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
    -l "cloud.aibrix.ai/pod-name=${1}" \
    -o jsonpath='{range .items[*]}{.status.remoteID}{end}' 2>/dev/null || true
}

binding_condition_reason_for_pod() {
  "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
    -l "cloud.aibrix.ai/pod-name=${1}" \
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
      "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
        -l "cloud.aibrix.ai/pod-name=${pod_name}" -o yaml >&2 || true
      return 1
    fi
    sleep 3
  done
  "$KUBECTL" -n "$NAMESPACE" describe pod "$pod_name" >&2 || true
  fail "timed out waiting for ${pod_name} to start with remoteID"
}

# RunPod restarts an exited container in place without an exit code. The
# controller stops the Pod and reports it as Failed with reason ContainerExited.
# Any other result fails the smoke.
wait_for_remote_exit() {
  local pod_name="$1"
  local timeout="$2"
  local deadline=$((SECONDS + timeout))
  local phase reason

  while (( SECONDS < deadline )); do
    phase="$(pod_phase "$pod_name")"
    reason="$(pod_terminated_reason "$pod_name")"
    log "pod=${pod_name} phase=${phase:-unknown} reason=${reason:-none}"
    if [[ "$phase" == "Failed" && "$reason" == "ContainerExited" ]]; then
      return 0
    fi
    if [[ "$phase" == "Failed" || "$phase" == "Succeeded" ]]; then
      "$KUBECTL" -n "$NAMESPACE" describe pod "$pod_name" >&2 || true
      "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
        -l "cloud.aibrix.ai/pod-name=${pod_name}" -o yaml >&2 || true
      fail "${pod_name} ended with phase=${phase} reason=${reason:-none}, want Failed/ContainerExited"
    fi
    sleep 5
  done
  "$KUBECTL" -n "$NAMESPACE" describe pod "$pod_name" >&2 || true
  "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
    -l "cloud.aibrix.ai/pod-name=${pod_name}" -o yaml >&2 || true
  fail "timed out waiting for ${pod_name} to exit"
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
      fail "${pod_name} unexpectedly became Running"
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

  # Pod deletion and the controller's binding cleanup are asynchronous.
  local bindings deadline=$((SECONDS + 90))
  while true; do
    bindings="$("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
      -l "cloud.aibrix.ai/pod-name=${pod_name}" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')" || fail "could not check bindings for ${pod_name}"
    [[ -n "$bindings" ]] || break
    (( SECONDS < deadline )) || fail "CloudPodBinding still exists for ${pod_name}: ${bindings}"
    sleep 2
  done

  if [[ -n "$remote_id" ]]; then
    local response status
    response="$(runpod_api GET "/pods/${remote_id}")"
    status="$(printf '%s\n' "$response" | sed -n '1p')"
    [[ "$status" == "404" ]] || fail "RunPod pod ${remote_id} still exists, GET status=${status}"
  fi
}

run_success_smoke() {
  log "running ${GPU_TYPE} ${CLOUD_TYPE} create/delete smoke"
  apply_runpod_pod "$SUCCESS_POD" "$CLOUD_TYPE" ""
  remember_external_name "$SUCCESS_POD"
  local remote_id
  remote_id="$(wait_for_started_remote "$SUCCESS_POD" "$SUCCESS_TIMEOUT_SECONDS")"
  log "created RunPod pod remoteID=${remote_id}"
  if [[ "$WAIT_FOR_COMPLETION" == "1" ]]; then
    log "waiting for the RunPod container to exit"
    wait_for_remote_exit "$SUCCESS_POD" "$COMPLETION_TIMEOUT_SECONDS"
  fi
  delete_pod_and_verify_cleanup "$SUCCESS_POD" "$remote_id"
}

run_spot_smoke() {
  log "verifying spot is rejected without creating a RunPod resource"
  apply_runpod_pod "$SPOT_POD" "COMMUNITY" "spot"
  remember_external_name "$SPOT_POD"
  wait_for_failed_class "$SPOT_POD" "UnsupportedSpec" "$SPOT_TIMEOUT_SECONDS"
  [[ -z "$(binding_remote_id_for_pod "$SPOT_POD")" ]] || fail "spot rejection unexpectedly allocated a remote Pod"
  delete_pod_and_verify_cleanup "$SPOT_POD" ""
}

final_orphan_check() {
  local external_name matches
  for external_name in "${EXTERNAL_NAMES[@]}"; do
    matches="$(runpod_list_by_name "$external_name")" || fail "could not verify absence of ${external_name}"
    [[ -z "$matches" ]] || fail "RunPod resources still exist for ${external_name}: ${matches}"
  done
}

main() {
  [[ "${RUNPOD_E2E:-0}" == "1" ]] || fail "set RUNPOD_E2E=1 to run real RunPod e2e"
  require_env RUNPOD_API_KEY
  require_command "$KUBECTL"
  require_command "$GO"
  require_command "$PYTHON"

  export RUNPOD_BASE_URL="${RUNPOD_BASE_URL:-${GPU_BURST_RUNPOD_BASE_URL:-https://api.runpod.io/v2}}"
  [[ "${RUNPOD_BASE_URL%/}" != */v1 ]] || fail "RunPod REST v2 is required; remove the /v1 base URL override"
  apply_control_plane_prereqs
  build_binary
  check_test_resources_absent
  trap cleanup EXIT
  start_controller
  run_success_smoke
  if [[ "$RUN_SPOT_CHECK" == "1" ]]; then
    run_spot_smoke
  fi
  final_orphan_check
  log "RunPod e2e smoke passed"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
