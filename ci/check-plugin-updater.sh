#!/usr/bin/env bash
set -euo pipefail

# VerifyJar enforces the runtime jar owner (UID 1000), so integration tests
# must create fixtures as that same user. GitHub's host runner uses UID 1001.
# Match updater/Dockerfile and keep the checkout read-only during validation.
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
docker run --rm \
  --user 1000:1000 \
  --env GOCACHE=/tmp/go-build \
  --env GOPATH=/tmp/go \
  --mount "type=bind,source=$repo_root,target=/workspace,readonly" \
  --workdir /workspace/updater \
  golang:1.25.13-bookworm \
  sh -ec 'test -z "$(gofmt -l .)"; go test -race -shuffle=on -count=1 ./...; go vet ./...'
