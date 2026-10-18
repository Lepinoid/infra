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
fingerprint=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
importer=0123456789012345678901234567890123456789
tag=$version-${importer:0:7}
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
  fake_oras_push "$REPOSITORY:$tag" --artifact-type application/vnd.lepinoid.world-bundle.v1 \
    "$manifest:application/vnd.lepinoid.world-bundle.manifest.v1+json" \
    "$archive:application/vnd.lepinoid.world-bundle.archive.v1.tar+gzip" \
    --annotation "net.lepinoid.pool-fingerprint=$fingerprint" \
    --annotation "net.lepinoid.runtime-revision=$importer" >/dev/null
  trap - RETURN
}
seed
digest=$(oras resolve "$REPOSITORY:$tag")

assert_success() {
  INPUT_VERSION=$1 INPUT_DIGEST=$2 bash "$resolver" >"$work/stdout.json"
  jq -e --arg d "$digest" --arg v "$version" --arg g "$commit" \
    --arg f "$fingerprint" --arg i "$importer" \
    '. == {digest:$d,version:$v,gitCommitSha:$g,poolFingerprint:$f,importerSha:$i}' "$OUT" >/dev/null
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

assert_success "$tag" ''
assert_success '' "$digest"
ALLOW_MISSING=1 assert_success "$tag" ''
rm -f "$OUT"
status=0
ALLOW_MISSING=1 INPUT_VERSION=2026.09.12-abcde-0123456 INPUT_DIGEST='' bash "$resolver" >"$work/stdout.json" 2>"$work/stderr" || status=$?
[[ $status == 3 && ! -e "$OUT" && ! -s "$work/stdout.json" ]]
ALLOW_MISSING=1 FAKE_ORAS_FETCH_ERROR='requested access to the resource is denied' assert_failure 2026.09.12-abcde-0123456 ''
ALLOW_MISSING=1 FAKE_ORAS_FETCH_ERROR='dial tcp: registry host not found' assert_failure 2026.09.12-abcde-0123456 ''
ALLOW_MISSING=1 FAKE_ORAS_FAIL=pull assert_failure "$tag" ''
assert_failure "$tag" "$digest"
assert_failure '' ''

oci="$FAKE_ORAS_STORE/blobs/${digest#sha256:}/oci-manifest.json"
cp "$oci" "$work/valid-oci.json"
jq 'del(.annotations["net.lepinoid.pool-fingerprint"])' "$oci" > "$work/bad-oci.json"
cp "$work/bad-oci.json" "$oci"
assert_failure '' "$digest"
cp "$work/valid-oci.json" "$oci"

# Keep the tampered manifest valid so only the tag/version equality check rejects it.
jq '.version = "2026.09.11-abcde" | .createdAt = "2026-09-11T00:00:00Z"' "$manifest" >"$work/changed.json"
cp "$work/changed.json" "$manifest"
seed
assert_failure "$tag" ''
assert_failure 2026.09.12-abcde-0123456 ''

jq --arg v "$version" '.version = $v | .createdAt = "2026-09-10T00:00:00Z" | .worlds[0].sha256 = ("0" * 64)' "$manifest" >"$work/changed.json"
cp "$work/changed.json" "$manifest"
seed
assert_failure "$tag" ''
printf 'all resolve-world-bundle tests passed\n'
