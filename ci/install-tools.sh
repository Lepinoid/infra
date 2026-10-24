#!/usr/bin/env bash
# CI で使う静的チェック用ツールを、版と sha256 を固定して BIN_DIR に入れる。
# ツールの版はこのファイルだけで管理する（workflow ごとに重複させない）。
# ローカルでは nix の devShell が同じツールを提供するため、このスクリプトは不要。
#
# usage: ci/install-tools.sh BIN_DIR TOOL...
#   TOOL: kustomize | kubeconform | yq | pinact
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 BIN_DIR TOOL..." >&2
  exit 2
fi
bin_dir=$1
shift
mkdir -p "$bin_dir"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# fetch URL SHA256 FILE: 取得して checksum を検証する。
fetch() {
  local url=$1 sha=$2 out=$3
  curl -fsSL --retry 3 "$url" -o "$out"
  echo "$sha  $out" | sha256sum -c - >/dev/null
}

for tool in "$@"; do
  case "$tool" in
    kustomize)
      fetch https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv5.8.3/kustomize_v5.8.3_linux_amd64.tar.gz \
        cb9e31198d3f63b44848bd0afc4c8efc56e6cfef9c93a4a39a211940e6decd61 "$work/kustomize.tar.gz"
      tar -xzf "$work/kustomize.tar.gz" -C "$bin_dir" kustomize
      ;;
    kubeconform)
      fetch https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-linux-amd64.tar.gz \
        9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883 "$work/kubeconform.tar.gz"
      tar -xzf "$work/kubeconform.tar.gz" -C "$bin_dir" kubeconform
      ;;
    yq)
      fetch https://github.com/mikefarah/yq/releases/download/v4.54.1/yq_linux_amd64 \
        8e34fc298390875de416e6a4afcb8cabeceb25d9aa8506c1a2f9353cf702ea5f "$work/yq"
      install -m 0755 "$work/yq" "$bin_dir/yq"
      ;;
    pinact)
      fetch https://github.com/suzuki-shunsuke/pinact/releases/download/v5.0.1/pinact_linux_amd64.tar.gz \
        12500c0fff5db94a0155e2e76ed959d1dc0e899056ee168d8fb7b4840e7c04f0 "$work/pinact.tar.gz"
      tar -xzf "$work/pinact.tar.gz" -C "$bin_dir" pinact
      ;;
    *)
      echo "unknown tool: $tool" >&2
      exit 2
      ;;
  esac
done
