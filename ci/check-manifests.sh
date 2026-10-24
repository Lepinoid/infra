#!/usr/bin/env bash
# マニフェストの静的チェックをまとめて実行する。CI (static-checks.yml) とローカルで同じものを使う。
# 依存: kustomize, kubeconform, yq（nix の devShell、または ci/install-tools.sh）。
#
# usage: ci/check-manifests.sh [ROOT]
set -euo pipefail

root=$(cd "${1:-$(dirname "${BASH_SOURCE[0]}")/..}" && pwd)
source "$(dirname "${BASH_SOURCE[0]}")/lib/manifest-checks.sh"

for tool in kustomize kubeconform yq; do
  command -v "$tool" >/dev/null || {
    echo "missing tool: $tool (use the nix devShell or ci/install-tools.sh)" >&2
    exit 2
  }
done

mapfile -t files < <(manifest_files "$root")
failed=()
run() {
  local name=$1
  shift
  echo "::group::$name"
  if "$@"; then
    echo "ok: $name"
  else
    failed+=("$name")
  fi
  echo "::endgroup::"
}

run "kustomize build + kubeconform" render_and_validate "$root"
run "container resources" check_resources "$root" "${files[@]}"
run "plugin-updater / digest pins" check_digest_pins "$root" "${files[@]}"
run "SOPS encryption" check_sops "$root" "${files[@]}"

if ((${#failed[@]} > 0)); then
  printf 'failed: %s\n' "${failed[@]}" >&2
  exit 1
fi
echo "all manifest checks passed"
