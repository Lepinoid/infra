#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
publisher=$script_dir/../publish-world-bundle.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export FAKE_ORAS_STORE=$work/store FAKE_ORAS_LOG=$work/oras.log
export ORAS_BIN=$script_dir/fake-oras.sh
export GHCR_USER=u GHCR_TOKEN=t REPOSITORY=ghcr.io/lepinoid/world-bundle
export BUILD_URL=https://example.test/build/1 GITHUB_OUTPUT=$work/github-output
unset FAKE_ORAS_FAIL FAKE_ORAS_FETCH_ERROR FAKE_ORAS_POST_PULL_HOOK FAKE_ORAS_AS_LIB
version=2026.09.10-aaaaa

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
no_push() { ! grep -q ' push ' "$FAKE_ORAS_LOG" || fail 'unexpected push'; }
make_bundle() {
  local dir=$1 content=$2 name hash size worlds='[]'
  mkdir -p "$dir" "$work/source"
  for name in alpha beta; do
    printf '%s\n' "$name $content" >"$work/source/level.dat"
    tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
      -cf - -C "$work/source" level.dat | gzip -n >"$dir/$name.tar.gz"
    hash=$(sha256sum "$dir/$name.tar.gz" | cut -d' ' -f1)
    size=$(stat -c %s "$dir/$name.tar.gz")
    worlds=$(jq -cn --argjson worlds "$worlds" --arg name "$name" \
      --arg hash "$hash" --argjson size "$size" \
      '$worlds + [{name:$name,archivePath:($name + ".tar.gz"),sha256:$hash,sizeBytes:$size}]')
  done
  jq -n --arg version "$version" --argjson worlds "$worlds" \
    '{schemaVersion:1,version:$version,gitCommitSha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      createdAt:"2026-09-10T00:00:00Z",worlds:$worlds}' >"$dir/manifest.json"
}
run_publish() {
  : >"$FAKE_ORAS_LOG"
  bash "$publisher" "$1" >"$work/stdout" 2>"$work/stderr"
}
expect_failure() {
  local status=0
  run_publish "$1" || status=$?
  [[ $status == 1 ]] || fail "expected exit 1, got $status"
  [[ ! -s $work/stdout ]] || fail 'failure emitted stdout'
  no_push
}

make_bundle "$work/bundle" original
run_publish "$work/bundle" || { cat "$work/stderr" >&2; fail 'initial publish'; }
digest=$(<"$work/stdout")
[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || fail 'invalid digest stdout'
[[ $(wc -l <"$work/stdout") == 1 ]] || fail 'extra stdout lines'
grep -q ' push ' "$FAKE_ORAS_LOG" || fail 'missing push'
[[ $(<"$FAKE_ORAS_STORE/tags/$version") == "$digest" ]] || fail 'tag not stored'
[[ $(<"$GITHUB_OUTPUT") == "digest=$digest" ]] || fail 'missing GitHub output'
oci=$FAKE_ORAS_STORE/blobs/${digest#sha256:}/oci-manifest.json
jq -e '
  .artifactType == "application/vnd.lepinoid.world-bundle.v1" and
  [.layers[].annotations["org.opencontainers.image.title"]] == ["manifest.json","alpha.tar.gz","beta.tar.gz"] and
  .layers[0].mediaType == "application/vnd.lepinoid.world-bundle.manifest.v1+json" and
  all(.layers[1:][]; .mediaType == "application/vnd.lepinoid.world-bundle.archive.v1.tar+gzip") and
  .annotations["org.opencontainers.image.revision"] == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" and
  .annotations["org.opencontainers.image.source"] == "https://github.com/Lepinoid/Worlds" and
  .annotations["org.opencontainers.image.created"] == "2026-09-10T00:00:00Z" and
  .annotations["net.lepinoid.build-url"] == "https://example.test/build/1"
' "$oci" >/dev/null || fail 'incorrect OCI metadata'
printf 'case 1 passed: absent tag pushes and returns digest\n'

make_bundle "$work/identical" original
run_publish "$work/identical" || { cat "$work/stderr" >&2; fail 'reuse'; }
no_push
grep -q 'reusing existing immutable artifact' "$work/stderr" || fail 'missing reuse message'
[[ $(<"$work/stdout") == "$digest" ]] || fail 'reuse digest changed'
grep -Fq "manifest fetch $REPOSITORY@$digest" "$FAKE_ORAS_LOG" || fail 'missing pinned fetch'
grep -Fq "$REPOSITORY@$digest" "$FAKE_ORAS_LOG" || fail 'missing pinned reference'
printf 'case 2 passed: identical bundle reuses digest without push\n'

make_bundle "$work/different" changed
expect_failure "$work/different"
grep -q collision "$work/stderr" || fail 'missing collision message'
printf 'case 3 passed: different bundle with same version rejects collision\n'

FAKE_ORAS_STORE=$work/absent-store
export FAKE_ORAS_FETCH_ERROR='requested access to the resource is denied'
expect_failure "$work/bundle"
grep -q 'requested access to the resource is denied' "$work/stderr" || fail 'auth error not propagated'
unset FAKE_ORAS_FETCH_ERROR
printf 'case 4 passed: auth denial fails without push\n'

FAKE_ORAS_STORE=$work/wrong-type-store
hex=${digest#sha256:}
mkdir -p "$FAKE_ORAS_STORE/tags" "$FAKE_ORAS_STORE/blobs/$hex/files"
printf '%s\n' "$digest" >"$FAKE_ORAS_STORE/tags/$version"
jq '.artifactType = "application/vnd.example.other"' "$oci" >"$FAKE_ORAS_STORE/blobs/$hex/oci-manifest.json"
cp "$work/bundle/"* "$FAKE_ORAS_STORE/blobs/$hex/files/"
expect_failure "$work/bundle"
printf 'case 5 passed: existing artifact with wrong artifactType is rejected\n'
printf 'all publish-world-bundle tests passed\n'
