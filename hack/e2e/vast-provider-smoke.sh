#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if [[ "${VAST_E2E:-0}" != "1" ]]; then
  printf '[vast-provider-e2e] ERROR: set VAST_E2E=1 to run the paid Vast.ai lifecycle test\n' >&2
  exit 1
fi
if [[ -z "${VAST_API_KEY:-}" ]]; then
  printf '[vast-provider-e2e] ERROR: VAST_API_KEY is required\n' >&2
  exit 1
fi

cd "$ROOT_DIR"
exec go test -count=1 -v ./pkg/providers/vast -run '^TestLiveVastLifecycle$'
