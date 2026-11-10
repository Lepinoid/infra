#!/usr/bin/env bash
# neuralwatt-exporter（exporters/neuralwatt）の gofmt・テスト・vet。CI（neuralwatt-exporter-tests.yml と
# publish workflow）とローカルで同じものを使う。依存: docker。
set -euo pipefail

# チェックアウトは読み取り専用でマウントし、ビルドキャッシュは /tmp に置く（check-plugin-updater.sh と同じ流儀）。
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
docker run --rm \
  --user 1000:1000 \
  --env GOCACHE=/tmp/go-build \
  --env GOPATH=/tmp/go \
  --mount "type=bind,source=$repo_root,target=/workspace,readonly" \
  --workdir /workspace/exporters/neuralwatt \
  golang:1.25.13-bookworm \
  sh -ec 'test -z "$(gofmt -l .)"; go test -race -shuffle=on -count=1 ./...; go vet ./...'
