#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KUBECTL="${KUBECTL:-kubectl}"
GO="${GO:-go}"
PYTHON="${PYTHON:-python3}"

NODE_NAME="${GPU_BURST_E2E_NODE_NAME:-neocloud-node-vast-e2e}"
NAMESPACE="${GPU_BURST_E2E_NAMESPACE:-default}"
BINARY="${GPU_BURST_E2E_BINARY:-/tmp/neocloud-node-vast-e2e}"
LISTEN_ADDR="${GPU_BURST_E2E_LISTEN_ADDR:-:11253}"
GPU_TYPE="${VAST_E2E_GPU_TYPE:-GTX 1070}"
MAX_PRICE="${VAST_E2E_MAX_PRICE:-0.10}"
DISK_GB="${VAST_E2E_DISK_GB:-20}"
VERIFIED="${VAST_E2E_VERIFIED:-true}"
SUCCESS_POD="${VAST_E2E_SUCCESS_POD:-neocloud-node-vast-e2e-success}"
SUCCESS_TIMEOUT_SECONDS="${VAST_E2E_SUCCESS_TIMEOUT_SECONDS:-600}"
DELETE_TIMEOUT_SECONDS="${VAST_E2E_DELETE_TIMEOUT_SECONDS:-180}"
EXTERNAL_NAMES=()
CONTROLLER_PID=""

log() {
  printf '[vast-e2e] %s\n' "$*" >&2
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

vast_list_by_label() {
  local label="$1"
  "$PYTHON" - "$label" <<'PY'
import json
import os
import sys
import urllib.parse
import urllib.request

label = sys.argv[1]
v0 = os.environ.get("VAST_BASE_URL", "https://console.vast.ai/api/v0").rstrip("/")
v1 = os.environ.get("VAST_V1_BASE_URL", "")
if not v1:
    prefix, marker, suffix = v0.rpartition("/api/")
    v1 = prefix + "/api/v1" if marker and suffix.startswith("v") else v0
query = urllib.parse.urlencode({
    "select_filters": json.dumps({"label": {"in": [label]}}, separators=(",", ":")),
    "order_by": json.dumps([{"col": "id", "dir": "asc"}], separators=(",", ":")),
    "limit": 25,
})
request = urllib.request.Request(
    v1.rstrip("/") + "/instances/?" + query,
    headers={"Authorization": "Bearer " + os.environ["VAST_API_KEY"]},
)
with urllib.request.urlopen(request, timeout=30) as response:
    payload = json.loads(response.read().decode("utf-8"))
for instance in payload.get("instances") or []:
    status = str(instance.get("actual_status") or "").lower()
    if instance.get("label") == label and status not in {"destroyed", "terminated", "deleted"}:
        print(instance.get("id", ""))
PY
}

vast_delete_by_id() {
  local instance_id="$1"
  [[ -n "$instance_id" ]] || return 0
  log "destroying Vast instance ${instance_id}"
  "$PYTHON" - "$instance_id" <<'PY'
import os
import sys
import urllib.error
import urllib.request

instance_id = sys.argv[1]
base = os.environ.get("VAST_BASE_URL", "https://console.vast.ai/api/v0").rstrip("/")
request = urllib.request.Request(
    base + "/instances/" + instance_id + "/",
    headers={"Authorization": "Bearer " + os.environ["VAST_API_KEY"]},
    method="DELETE",
)
try:
    with urllib.request.urlopen(request, timeout=30):
        pass
except urllib.error.HTTPError as exc:
    if exc.code != 404:
        raise
PY
}

vast_delete_by_external_name() {
  local external_name="$1"
  local instance_id
  while IFS= read -r instance_id; do
    vast_delete_by_id "$instance_id"
  done < <(vast_list_by_label "$external_name" || true)
}

vast_instance_exists() {
  local instance_id="$1"
  "$PYTHON" - "$instance_id" <<'PY'
import json
import os
import sys
import urllib.error
import urllib.request

instance_id = sys.argv[1]
base = os.environ.get("VAST_BASE_URL", "https://console.vast.ai/api/v0").rstrip("/")
request = urllib.request.Request(
    base + "/instances/" + instance_id + "/",
    headers={"Authorization": "Bearer " + os.environ["VAST_API_KEY"]},
)
try:
    with urllib.request.urlopen(request, timeout=30) as response:
        payload = json.loads(response.read().decode("utf-8"))
except urllib.error.HTTPError as exc:
    if exc.code == 404:
        sys.exit(1)
    raise
instance = payload.get("instances")
if not instance:
    sys.exit(1)
status = str(instance.get("actual_status") or "").lower()
sys.exit(1 if status in {"destroyed", "terminated", "deleted"} else 0)
PY
}

cleanup_kubernetes_objects() {
  "$KUBECTL" -n "$NAMESPACE" delete pod "$SUCCESS_POD" \
    --ignore-not-found=true --wait=true --timeout=60s >/dev/null 2>&1 || true

  local binding
  while IFS= read -r binding; do
    [[ -n "$binding" ]] || continue
    "$KUBECTL" -n "$NAMESPACE" patch cloudpodbinding "$binding" \
      --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
    "$KUBECTL" -n "$NAMESPACE" delete cloudpodbinding "$binding" \
      --ignore-not-found=true >/dev/null 2>&1 || true
  done < <("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
    -l "cloud.aibrix.ai/pod-name=${SUCCESS_POD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

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
    vast_delete_by_external_name "$external_name"
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
  printf '%.64s\n' "neocloud-node-${raw}"
}

apply_control_plane_prereqs() {
  "$KUBECTL" create namespace aibrix-system \
    --dry-run=client -o yaml | "$KUBECTL" apply -f -
  awk 'BEGIN { doc=0 } /^---$/ { doc++; if (doc >= 2) exit } { print }' \
    "$ROOT_DIR/deploy/neocloud-node.yaml" | "$KUBECTL" apply -f -
}

build_binary() {
  log "building ${BINARY}"
  "$GO" build -o "$BINARY" "$ROOT_DIR/cmd/neocloud-node"
}

start_controller() {
  log "starting local Vast controller on ${NODE_NAME}"
  VAST_API_KEY="$VAST_API_KEY" "$BINARY" \
    --kubeconfig="${KUBECONFIG:-$HOME/.kube/config}" \
    --node-name="$NODE_NAME" \
    --provider=vast \
    --binding-store=kubernetes \
    --listen-addr="$LISTEN_ADDR" \
    --status-sync-interval="${VAST_E2E_STATUS_SYNC_INTERVAL:-5s}" \
    --vast-disk-gb="$DISK_GB" \
    --vast-min-reliability="${VAST_E2E_MIN_RELIABILITY:-0.95}" \
    --advertise-gpu-count=1 \
    --advertise-pod-capacity=2 \
    --max-gpu-count=1 &
  CONTROLLER_PID=$!
  local deadline=$((SECONDS + 60))
  until "$KUBECTL" get "node/${NODE_NAME}" >/dev/null 2>&1; do
    (( SECONDS < deadline )) || fail "timed out waiting for node/${NODE_NAME}"
    sleep 1
  done
  "$KUBECTL" wait --for=condition=Ready "node/${NODE_NAME}" --timeout=60s
}

apply_vast_pod() {
  cat <<YAML | "$KUBECTL" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${SUCCESS_POD}
  namespace: ${NAMESPACE}
  labels:
    cloud.aibrix.ai/e2e: vast
  annotations:
    cloud.aibrix.ai/provider: vast
    cloud.aibrix.ai/gpu-type: ${GPU_TYPE}
    cloud.aibrix.ai/max-price: "${MAX_PRICE}"
    cloud.aibrix.ai/vast-verified: "${VERIFIED}"
    cloud.aibrix.ai/vast-disk-gb: "${DISK_GB}"
spec:
  restartPolicy: Never
  nodeSelector:
    cloud.aibrix.ai/provider: vast
  tolerations:
    - key: cloud.aibrix.ai/provider
      operator: Equal
      value: vast
      effect: NoSchedule
  containers:
    - name: smoke
      image: ${VAST_E2E_IMAGE:-python:3.12-slim}
      command: ["/bin/sh", "-lc"]
      args: ["echo neocloud-vast-e2e-ready; exec sleep 600"]
      resources:
        limits:
          nvidia.com/gpu: "1"
YAML
}

remember_external_name() {
  local uid
  uid="$("$KUBECTL" -n "$NAMESPACE" get pod "$SUCCESS_POD" -o jsonpath='{.metadata.uid}')"
  EXTERNAL_NAMES+=("$(managed_external_name "$NAMESPACE" "$uid")")
}

binding_remote_id() {
  "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
    -l "cloud.aibrix.ai/pod-name=${SUCCESS_POD}" \
    -o jsonpath='{range .items[*]}{.status.remoteID}{end}' 2>/dev/null || true
}

wait_for_started_remote() {
  local deadline=$((SECONDS + SUCCESS_TIMEOUT_SECONDS))
  local phase reason remote_id
  "$KUBECTL" wait --for=jsonpath='{.spec.nodeName}'="$NODE_NAME" \
    "pod/${SUCCESS_POD}" -n "$NAMESPACE" --timeout=60s >&2
  while (( SECONDS < deadline )); do
    phase="$("$KUBECTL" -n "$NAMESPACE" get pod "$SUCCESS_POD" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    reason="$("$KUBECTL" -n "$NAMESPACE" get pod "$SUCCESS_POD" -o jsonpath='{.status.reason}' 2>/dev/null || true)"
    remote_id="$(binding_remote_id)"
    log "pod=${SUCCESS_POD} phase=${phase:-unknown} reason=${reason:-none} remote=${remote_id:-none}"
    if [[ -n "$remote_id" && "$phase" == "Running" ]]; then
      printf '%s\n' "$remote_id"
      return 0
    fi
    if [[ "$phase" == "Failed" ]]; then
      "$KUBECTL" -n "$NAMESPACE" describe pod "$SUCCESS_POD" >&2 || true
      "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
        -l "cloud.aibrix.ai/pod-name=${SUCCESS_POD}" -o yaml >&2 || true
      return 1
    fi
    sleep 5
  done
  fail "timed out waiting for ${SUCCESS_POD} to run"
}

delete_pod_and_verify_cleanup() {
  local remote_id="$1"
  "$KUBECTL" -n "$NAMESPACE" delete pod "$SUCCESS_POD" \
    --ignore-not-found=true --wait=true --timeout=120s

  local bindings deadline
  bindings="$("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.cloud.aibrix.ai \
    -l "cloud.aibrix.ai/pod-name=${SUCCESS_POD}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
  [[ -z "$bindings" ]] || fail "CloudPodBinding still exists: ${bindings}"

  deadline=$((SECONDS + DELETE_TIMEOUT_SECONDS))
  while vast_instance_exists "$remote_id"; do
    (( SECONDS < deadline )) || fail "Vast instance ${remote_id} still exists after deletion timeout"
    sleep 3
  done
}

final_orphan_check() {
  local external_name matches
  for external_name in "${EXTERNAL_NAMES[@]}"; do
    matches="$(vast_list_by_label "$external_name" | sed '/^$/d' || true)"
    [[ -z "$matches" ]] || fail "Vast resources still exist for ${external_name}: ${matches}"
  done
}

main() {
  [[ "${VAST_E2E:-0}" == "1" ]] || fail "set VAST_E2E=1 to run the paid Vast.ai e2e"
  require_env VAST_API_KEY
  require_command "$KUBECTL"
  require_command "$GO"
  require_command "$PYTHON"

  apply_control_plane_prereqs
  build_binary
  cleanup_kubernetes_objects
  start_controller
  apply_vast_pod
  remember_external_name
  local remote_id
  remote_id="$(wait_for_started_remote)"
  log "created Vast instance remoteID=${remote_id}"
  delete_pod_and_verify_cleanup "$remote_id"
  final_orphan_check
  log "Vast.ai Kubernetes e2e smoke passed"
}

main "$@"
