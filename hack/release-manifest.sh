#!/usr/bin/env bash

set -euo pipefail

tag="${1:-}"
output="${2:-}"

if [[ -z "${tag}" || -z "${output}" ]]; then
  echo "usage: $0 <vMAJOR.MINOR.PATCH[-PRERELEASE]> <output>" >&2
  exit 2
fi

version="${tag#v}"
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  echo "invalid release tag: ${tag}" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

cp -R "${repo_root}/config" "${work_dir}/config"
(
  cd "${work_dir}/config/default"
  kustomize edit set image \
    "ghcr.io/aibrix/neocloud-node=ghcr.io/aibrix/neocloud-node:${version}"
)

mkdir -p "$(dirname "${output}")"
kustomize build "${work_dir}/config/default" >"${output}"
