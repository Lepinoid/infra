#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
lib=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
resolver="$lib/../resolve-world-bundle.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export FAKE_ORAS_STORE="$work/store"
unset FAKE_ORAS_FAIL FAKE_ORAS_LOG FAKE_ORAS_POST_PULL_HOOK FAKE_ORAS_FETCH_ERROR
FAKE_ORAS_AS_LIB=1 source "$lib/fake-oras.sh" version >/dev/null
mkdir -p "$work/bin" "$work/bundle"
ln -s "$lib/fake-oras.sh" "$work/bin/oras"
export PATH="$work/bin:$PATH" ORAS_BIN=oras
export GHCR_USER=test GH_TOKEN=test REPOSITORY=ghcr.io/lepinoid/world-bundle
export OUT="$work/desired.json"
version=2026.09.10-abcde
commit=abcdef0123456789abcdef0123456789abcdef01
archive="$work/bundle/world.tar.gz"
manifest="$work/bundle/manifest.json"
tar -czf "$archive" --files-from /dev/null
jq -n --arg v "$version" --arg g "$commit" \
  --arg sha "$(sha256sum "$archive" | cut -d' ' -f1)" \
  --argjson size "$(stat -c %s "$archive")" \
  '{schemaVersion:1,version:$v,gitCommitSha:$g,createdAt:"2026-09-10T00:00:00Z",worlds:[{name:"world",archivePath:"world.tar.gz",sha256:$sha,sizeBytes:$size}]}' >"$manifest"
seed() {
  # fake-oras leaves a RETURN trap referencing tmp after its push helper returns.
  local tmp="$work/seed-tmp"
  fake_oras_seed_bundle "$FAKE_ORAS_STORE" "$version" \
    application/vnd.lepinoid.world-bundle.v1 "$manifest" "$archive"
  trap - RETURN
}
seed
digest=$(oras resolve "$REPOSITORY:$version")

assert_success() {
  INPUT_VERSION=$1 INPUT_DIGEST=$2 bash "$resolver" >"$work/stdout.json"
  jq -e --arg d "$digest" --arg v "$version" --arg g "$commit" \
    '. == {digest:$d,version:$v,gitCommitSha:$g}' "$OUT" >/dev/null
  cmp "$OUT" "$work/stdout.json"
}
assert_failure() {
  local status=0
  rm -f "$OUT"
  INPUT_VERSION=$1 INPUT_DIGEST=$2 bash "$resolver" >"$work/stdout.json" 2>"$work/stderr" || status=$?
  if [[ $status != 1 || -e "$OUT" || -s "$work/stdout.json" ]]; then
    printf 'expected exit 1 without output for version=%s digest=%s; got %s\n' "$1" "$2" "$status" >&2
    return 1
  fi
}

assert_success "$version" ''
assert_success '' "$digest"
assert_failure "$version" "$digest"
assert_failure '' ''

# Keep the tampered manifest valid so only the tag/version equality check rejects it.
jq '.version = "2026.09.11-abcde" | .createdAt = "2026-09-11T00:00:00Z"' "$manifest" >"$work/changed.json"
cp "$work/changed.json" "$manifest"
seed
assert_failure "$version" ''
assert_failure 2026.09.12-abcde ''

jq --arg v "$version" '.version = $v | .createdAt = "2026-09-10T00:00:00Z" | .worlds[0].sha256 = ("0" * 64)' "$manifest" >"$work/changed.json"
cp "$work/changed.json" "$manifest"
seed
assert_failure "$version" ''
printf 'all resolve-world-bundle tests passed\n'
