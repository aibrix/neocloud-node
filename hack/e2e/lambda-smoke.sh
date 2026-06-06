#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KUBECTL="${KUBECTL:-kubectl}"
GO="${GO:-go}"
PYTHON="${PYTHON:-python3}"
CURL="${CURL:-curl}"

NODE_NAME="${GPU_BURST_E2E_NODE_NAME:-aibrix-gpu-burst-lambda-e2e}"
NAMESPACE="${GPU_BURST_E2E_NAMESPACE:-default}"
BINARY="${GPU_BURST_E2E_BINARY:-/tmp/gpu-burst-node-e2e}"
LISTEN_ADDR="${GPU_BURST_E2E_LISTEN_ADDR:-:11252}"
CALLBACK_ADDR="${LAMBDA_E2E_CALLBACK_ADDR:-${GPU_BURST_RUNNER_CALLBACK_ADDR:-}}"
CALLBACK_URL="${LAMBDA_E2E_CALLBACK_URL:-${GPU_BURST_LAMBDA_CALLBACK_URL:-}}"
POD_NAME="${LAMBDA_E2E_POD:-gpu-burst-lambda-e2e-success}"
SUCCESS_TIMEOUT_SECONDS="${LAMBDA_E2E_SUCCESS_TIMEOUT_SECONDS:-900}"
RUNNER_TIMEOUT_SECONDS="${LAMBDA_E2E_RUNNER_TIMEOUT_SECONDS:-1800}"
LAMBDA_BASE_URL="${LAMBDA_BASE_URL:-${GPU_BURST_LAMBDA_BASE_URL:-https://cloud.lambda.ai/api/v1}}"
INSTANCE_TYPE="${LAMBDA_E2E_INSTANCE_TYPE:-}"
REGION="${LAMBDA_E2E_REGION:-}"
EXTERNAL_NAMES=()
CONTROLLER_PID=""

log() {
  printf '[lambda-e2e] %s\n' "$*" >&2
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

lambda_api() {
	local method="$1"
	local path="$2"
	local body="${3:-}"
	local body_file
	body_file="$(mktemp)"
	local status
	status="$(lambda_request "$body_file" "$method" "$path" "$body" "Bearer ${LAMBDA_API_KEY}")"
	if ! is_success_status "$status"; then
		status="$(lambda_request "$body_file" "$method" "$path" "$body" "$(lambda_basic_auth_header)")"
	fi
	printf '%s\n' "$status"
	cat "$body_file"
	rm -f "$body_file"
}

lambda_basic_auth_header() {
	"$PYTHON" <<'PY'
import base64
import os

api_key = os.environ["LAMBDA_API_KEY"]
print("Basic " + base64.b64encode((api_key + ":").encode()).decode())
PY
}

lambda_request() {
	local output_file="$1"
	local method="$2"
	local path="$3"
	local body="$4"
	local authorization="$5"
	local url="${LAMBDA_BASE_URL%/}${path}"
	local args=(
		-sS
		-m 30
		-X "$method"
		-H "Accept: application/json"
		-H "Authorization: ${authorization}"
		-o "$output_file"
		-w "%{http_code}"
	)
	if [[ -n "$body" ]]; then
		args+=(-H "Content-Type: application/json" --data "$body")
	fi
	args+=("$url")
	local status
	if ! status="$("$CURL" "${args[@]}")"; then
		printf '000'
		return 0
	fi
	printf '%s' "$status"
}

is_success_status() {
	local status="$1"
	[[ "$status" =~ ^2[0-9][0-9]$ ]]
}

select_lambda_capacity() {
	local body_file
	body_file="$(mktemp)"
	local status
	status="$(lambda_request "$body_file" GET "/instance-types" "" "Bearer ${LAMBDA_API_KEY}")"
	if ! is_success_status "$status"; then
		status="$(lambda_request "$body_file" GET "/instance-types" "" "$(lambda_basic_auth_header)")"
	fi
	if ! is_success_status "$status"; then
		local detail
		detail="$(tr '\n' ' ' <"$body_file" | cut -c1-300)"
		rm -f "$body_file"
		fail "Lambda instance-types request failed: status=${status} detail=${detail}"
	fi
	"$PYTHON" - "$body_file" <<'PY'
import json
import os
import sys

with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)

items = data.get("data", {})
if isinstance(items, list):
    normalized = {}
    for entry in items:
        name = entry.get("name") or entry.get("instance_type", {}).get("name")
        if name:
            normalized[name] = entry
    items = normalized

preferred = []
requested = os.environ.get("LAMBDA_E2E_INSTANCE_TYPE", "").strip()
if requested:
    preferred.append(requested)
preferred.extend(["gpu_1x_a10", "gpu_1x_a100"])

for name in preferred:
    entry = items.get(name)
    if not entry:
        continue
    instance_type = entry.get("instance_type", entry)
    specs = instance_type.get("specs", {})
    if int(specs.get("gpus") or 0) != 1:
        continue
    regions = entry.get("regions_with_capacity_available") or []
    region = os.environ.get("LAMBDA_E2E_REGION", "").strip()
    if region:
        if any(item.get("name") == region for item in regions):
            print(name, region, instance_type.get("price_cents_per_hour", 0))
            sys.exit(0)
        continue
    if regions:
        print(name, regions[0]["name"], instance_type.get("price_cents_per_hour", 0))
        sys.exit(0)

raise SystemExit("no allowed single-GPU Lambda capacity found for gpu_1x_a10 or gpu_1x_a100")
PY
	rm -f "$body_file"
}

lambda_list_by_name() {
	local name="$1"
	lambda_list_by_name_with_status "$name" | awk '{print $1}'
}

lambda_list_by_name_with_status() {
	local name="$1"
	local body_file
	body_file="$(mktemp)"
	local status
	status="$(lambda_request "$body_file" GET "/instances" "" "Bearer ${LAMBDA_API_KEY}")"
	if ! is_success_status "$status"; then
		status="$(lambda_request "$body_file" GET "/instances" "" "$(lambda_basic_auth_header)")"
	fi
	if ! is_success_status "$status"; then
		rm -f "$body_file"
		return 0
	fi
	"$PYTHON" - "$name" "$body_file" <<'PY'
import json
import sys

name = sys.argv[1]
with open(sys.argv[2], encoding="utf-8") as f:
    data = json.load(f)

for instance in data.get("data", []):
    if instance.get("name") == name:
        print(instance.get("id", ""), instance.get("status", ""))
PY
	rm -f "$body_file"
}

terminate_lambda_by_id() {
  local instance_id="$1"
  [[ -n "$instance_id" ]] || return 0
  log "terminating Lambda instance $instance_id"
  lambda_api POST "/instance-operations/terminate" "{\"instance_ids\":[\"${instance_id}\"]}" >/dev/null || true
}

terminate_lambda_by_external_name() {
  local external_name="$1"
  [[ -n "$external_name" ]] || return 0
  local instance_id
  while IFS= read -r instance_id; do
    terminate_lambda_by_id "$instance_id"
  done < <(lambda_list_by_name "$external_name" || true)
}

cleanup_kubernetes_objects() {
  "$KUBECTL" -n "$NAMESPACE" delete pod "$POD_NAME" \
    --ignore-not-found=true --wait=true --timeout=90s >/dev/null 2>&1 || true

  local binding
  while IFS= read -r binding; do
    [[ -n "$binding" ]] || continue
    "$KUBECTL" -n "$NAMESPACE" patch cloudpodbinding "$binding" \
      --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
    "$KUBECTL" -n "$NAMESPACE" delete cloudpodbinding "$binding" \
      --ignore-not-found=true >/dev/null 2>&1 || true
  done < <("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
    -l "gpu-burst-node.aibrix.ai/pod-name=${POD_NAME}" \
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
  for external_name in "${EXTERNAL_NAMES[@]+"${EXTERNAL_NAMES[@]}"}"; do
    terminate_lambda_by_external_name "$external_name"
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
  "$GO" build -o "$BINARY" "$ROOT_DIR/cmd/gpu-burst-node"
}

start_controller() {
	log "starting local Lambda controller on $NODE_NAME with ${INSTANCE_TYPE}/${REGION}"
	local args=(
		--kubeconfig="${KUBECONFIG:-$HOME/.kube/config}"
		--node-name="$NODE_NAME"
		--provider=lambda
		--binding-store=kubernetes
		--listen-addr="$LISTEN_ADDR"
		--lambda-region="$REGION"
		--lambda-instance-type="$INSTANCE_TYPE"
		--lambda-ssh-key-names="${LAMBDA_E2E_SSH_KEY_NAMES:-${LAMBDA_CLOUD_SSH_KEYS:-}}"
		--status-sync-interval="${LAMBDA_E2E_STATUS_SYNC_INTERVAL:-5s}"
		--advertise-gpu-count=1
		--advertise-pod-capacity=5
		--max-gpu-count=1
	)
	if [[ -n "$CALLBACK_URL" ]]; then
		args+=(--lambda-callback-url="$CALLBACK_URL")
	fi
	if [[ -n "$CALLBACK_ADDR" ]]; then
		args+=(--runner-callback-addr="$CALLBACK_ADDR")
	fi
	LAMBDA_API_KEY="${LAMBDA_API_KEY:-${LAMBDA_CLOUD_API_KEY:-}}" \
		GPU_BURST_LAMBDA_BASE_URL="$LAMBDA_BASE_URL" \
		"$BINARY" "${args[@]}" &
	CONTROLLER_PID=$!
	local deadline=$((SECONDS + 60))
	until "$KUBECTL" get "node/${NODE_NAME}" >/dev/null 2>&1; do
		((SECONDS < deadline)) || fail "timed out waiting for node/${NODE_NAME} to be created"
		sleep 1
	done
	"$KUBECTL" wait --for=condition=Ready "node/${NODE_NAME}" --timeout=60s
}

apply_lambda_pod() {
  cat <<YAML | "$KUBECTL" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  namespace: ${NAMESPACE}
  labels:
    gpu-burst-node.aibrix.ai/e2e: lambda
  annotations:
    gpu-burst-node.aibrix.ai/provider: lambda
    gpu-burst-node.aibrix.ai/gpu-type: ${INSTANCE_TYPE}
    gpu-burst-node.aibrix.ai/region: ${REGION}
    gpu-burst-node.aibrix.ai/lambda-instance-type: ${INSTANCE_TYPE}
spec:
  restartPolicy: Never
  nodeSelector:
    gpu-burst-node.aibrix.ai/provider: lambda
  tolerations:
    - key: gpu-burst-node.aibrix.ai/provider
      operator: Equal
      value: lambda
      effect: NoSchedule
  containers:
    - name: smoke
      image: nvidia/cuda:12.4.1-base-ubuntu22.04
      command: ["bash", "-lc"]
      args: ["nvidia-smi || true; sleep 5"]
      resources:
        limits:
          nvidia.com/gpu: "1"
YAML
}

wait_for_binding_running() {
  local deadline=$((SECONDS + SUCCESS_TIMEOUT_SECONDS))
  local phase=""
  local remote_id=""
  while (( SECONDS < deadline )); do
    phase="$("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
      -l "gpu-burst-node.aibrix.ai/pod-name=${POD_NAME}" \
      -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)"
    remote_id="$("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
      -l "gpu-burst-node.aibrix.ai/pod-name=${POD_NAME}" \
      -o jsonpath='{.items[0].status.remoteID}' 2>/dev/null || true)"
    log "binding phase=${phase:-<none>} remoteID=${remote_id:-<none>}"
    if [[ -n "$remote_id" && ( "$phase" == "Running" || "$phase" == "Succeeded" ) ]]; then
      return 0
    fi
    if [[ "$phase" == "Failed" ]]; then
      "$KUBECTL" -n "$NAMESPACE" describe pod "$POD_NAME" || true
      "$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
        -l "gpu-burst-node.aibrix.ai/pod-name=${POD_NAME}" -o yaml || true
      fail "Lambda binding failed"
    fi
    sleep 10
  done
  fail "timed out waiting for Lambda binding to run"
}

wait_for_runner_succeeded() {
	local deadline=$((SECONDS + RUNNER_TIMEOUT_SECONDS))
	local phase=""
	while ((SECONDS < deadline)); do
		phase="$("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
			-l "gpu-burst-node.aibrix.ai/pod-name=${POD_NAME}" \
			-o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)"
		log "runner binding phase=${phase:-<none>}"
		if [[ "$phase" == "Succeeded" ]]; then
			return 0
		fi
		if [[ "$phase" == "Failed" ]]; then
			"$KUBECTL" -n "$NAMESPACE" describe pod "$POD_NAME" || true
			"$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
				-l "gpu-burst-node.aibrix.ai/pod-name=${POD_NAME}" -o yaml || true
			fail "Lambda runner failed"
		fi
		sleep 10
	done
	fail "timed out waiting for Lambda runner callback"
}

delete_pod_and_wait_cleanup() {
  "$KUBECTL" -n "$NAMESPACE" delete pod "$POD_NAME" --wait=true --timeout=120s
  local deadline=$((SECONDS + 180))
  while (( SECONDS < deadline )); do
    local count
    count="$("$KUBECTL" -n "$NAMESPACE" get cloudpodbindings.gpu-burst-node.aibrix.ai \
      -l "gpu-burst-node.aibrix.ai/pod-name=${POD_NAME}" \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')"
    [[ "$count" == "0" ]] && return 0
    sleep 5
  done
  fail "binding was not deleted after pod deletion"
}

final_orphan_check() {
	local external_name="$1"
	local deadline=$((SECONDS + 300))
	local matches active
	while ((SECONDS < deadline)); do
		matches="$(lambda_list_by_name_with_status "$external_name" | sed '/^$/d' || true)"
		[[ -z "$matches" ]] && return 0
		active="$(awk '$2 != "terminated" { print }' <<<"$matches")"
		[[ -z "$active" ]] && return 0
		log "waiting for Lambda termination: ${active//$'\n'/; }"
		sleep 10
	done
	fail "Lambda instance(s) still not terminated for ${external_name}: ${active:-unknown}"
}

main() {
  [[ "${LAMBDA_E2E:-0}" == "1" ]] || fail "set LAMBDA_E2E=1 to run the paid Lambda smoke test"
  if [[ -z "${LAMBDA_API_KEY:-}" && -n "${LAMBDA_CLOUD_API_KEY:-}" ]]; then
    export LAMBDA_API_KEY="$LAMBDA_CLOUD_API_KEY"
  fi
  require_env LAMBDA_API_KEY
  if [[ -z "${LAMBDA_E2E_SSH_KEY_NAMES:-}" && -n "${LAMBDA_CLOUD_SSH_KEYS:-}" ]]; then
    export LAMBDA_E2E_SSH_KEY_NAMES="$LAMBDA_CLOUD_SSH_KEYS"
  fi
  require_env LAMBDA_E2E_SSH_KEY_NAMES
	require_command "$KUBECTL"
	require_command "$GO"
	require_command "$PYTHON"
	require_command "$CURL"

  if [[ -z "$INSTANCE_TYPE" || -z "$REGION" ]]; then
    read -r INSTANCE_TYPE REGION price_cents < <(select_lambda_capacity)
    log "selected ${INSTANCE_TYPE}/${REGION} price_cents_per_hour=${price_cents:-unknown}"
  fi
  [[ "$INSTANCE_TYPE" == "gpu_1x_a10" || "$INSTANCE_TYPE" == "gpu_1x_a100" ]] || fail "refusing non-allowed Lambda instance type $INSTANCE_TYPE"

  cleanup_kubernetes_objects
  apply_control_plane_prereqs
  build_binary
  start_controller
  apply_lambda_pod

  local uid external_name
  uid="$("$KUBECTL" -n "$NAMESPACE" get pod "$POD_NAME" -o jsonpath='{.metadata.uid}')"
  external_name="$(managed_external_name "$NAMESPACE" "$uid")"
  EXTERNAL_NAMES+=("$external_name")
  log "tracking external name $external_name"

  wait_for_binding_running
	if [[ -n "$CALLBACK_URL" ]]; then
		wait_for_runner_succeeded
	fi
  delete_pod_and_wait_cleanup
  final_orphan_check "$external_name"
  log "Lambda e2e smoke passed"
}

main "$@"
