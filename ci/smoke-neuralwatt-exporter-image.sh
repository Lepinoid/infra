#!/usr/bin/env bash
# neuralwatt-exporter のイメージを、設定なし（NEURALWATT_API_KEY_FILE なし）で起動し、
# scratch 上で静的バイナリが動き、設定エラーで終了することだけを確かめる（API は呼ばない）。
#
# usage: ci/smoke-neuralwatt-exporter-image.sh IMAGE
set -euo pipefail

image=${1:?usage: $0 IMAGE}
if out=$(docker run --rm "$image" 2>&1); then
  echo "the image started without NEURALWATT_API_KEY_FILE; expected a configuration error" >&2
  exit 1
fi
if ! grep -q 'NEURALWATT_API_KEY_FILE is required' <<<"$out"; then
  echo "unexpected output from the image:" >&2
  echo "$out" >&2
  exit 1
fi
echo "smoke test passed: $image"
