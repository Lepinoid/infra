#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BB=$(command -v busybox) || { printf 'busybox is required\n' >&2; exit 1; }
TEMP=$(mktemp -d)
trap 'rm -rf "$TEMP"' EXIT
export FAKE_ORAS_STORE="$TEMP/bootstrap-store"
FAKE_ORAS_AS_LIB=1 source "$ROOT/ci/lib/fake-oras.sh" version >/dev/null
SCRIPT="$ROOT/lepinoid/world-sync-init.sh"
FAKE="$ROOT/ci/lib/fake-oras.sh"
TYPE=application/vnd.lepinoid.world-bundle.v1
CASE=setup

diagnostics() {
  trap - ERR
  printf 'FAIL: %s: %s\n' "$CASE" "$*" >&2
  if [[ -f ${S:-}/run.log ]]; then tail -n 80 "$S/run.log" >&2; fi
  if [[ -d ${S:-}/data ]]; then find "$S/data" -print >&2; fi
  exit 1
}
trap 'diagnostics "unexpected error at line $LINENO"' ERR
assert_eq() { [[ $1 == "$2" ]] || diagnostics "expected [$2], got [$1]"; }
tree_sha() (
  cd "$1"
  find . -type f ! -name checksums.json -print0 | sort -z | xargs -0 -r sha256sum | sha256sum | cut -d ' ' -f 1
)
new_case() {
  CASE=$1
  S=$(mktemp -d "$TEMP/scenario.XXXXXX")
  mkdir -p "$S/data/terrestrial" "$S/work" "$S/store/tags" "$S/store/blobs"
  printf 'legacy\n' > "$S/data/terrestrial/level.dat"
  printf '{}\n' > "$S/registry.json"
  export FAKE_ORAS_STORE="$S/store"
  fake_oras_store=$FAKE_ORAS_STORE
  unset FAKE_ORAS_FAIL FAKE_ORAS_POST_PULL_HOOK FAKE_ORAS_FETCH_ERROR
}
bundle() {
  local label=$1; shift
  local dir="$S/$label" name sha size worlds='[]'
  mkdir -p "$dir"
  for name in "$@"; do
    mkdir -p "$dir/$name/region"
    printf '%s %s level\n' "$label" "$name" > "$dir/$name/level.dat"
    printf '%s %s region\n' "$label" "$name" > "$dir/$name/region/r.0.0.mca"
    tar -czf "$dir/$name.tar.gz" -C "$dir/$name" .
    sha=$(sha256sum "$dir/$name.tar.gz" | cut -d ' ' -f 1)
    size=$(stat -c %s "$dir/$name.tar.gz")
    worlds=$(jq -c --arg n "$name" --arg sha "$sha" --argjson size "$size" \
      '. + [{name:$n,archivePath:($n+".tar.gz"),sha256:$sha,sizeBytes:$size}]' <<< "$worlds")
  done
  jq -cn --argjson worlds "$worlds" \
    '{schemaVersion:1,version:"2026.09.10-aaaaa",createdAt:"2026-09-10T00:00:00Z",gitCommitSha:("a"*40),worlds:$worlds}' > "$dir/manifest.json"
}
seed() {
  local label=$1 old hex tmp="$S/seed-return"
  fake_oras_seed_bundle "$S/store" "$label" "$TYPE" "$S/$label/manifest.json" "$S/$label/"*.tar.gz
  trap - RETURN
  old=$(cat "$S/store/tags/$label")
  # The fake hashes without its saved trailing newline; address the saved bytes.
  hex=$(sha256sum "$S/store/blobs/${old#sha256:}/oci-manifest.json" | cut -d ' ' -f 1)
  if [[ $hex != "${old#sha256:}" ]]; then
    cp -a "$S/store/blobs/${old#sha256:}" "$S/store/blobs/$hex"
  fi
  DIGEST="sha256:$hex"
}
mutate() {
  jq "$2" "$S/$1/manifest.json" > "$S/edited.json"
  mv "$S/edited.json" "$S/$1/manifest.json"
}
refresh_archive() {
  local label=$1 sha size
  sha=$(sha256sum "$S/$label/terrestrial.tar.gz" | cut -d ' ' -f 1)
  size=$(stat -c %s "$S/$label/terrestrial.tar.gz")
  mutate "$label" ".worlds[0].sha256=\"$sha\" | .worlds[0].sizeBytes=$size"
}
run_init() {
  local desired=$1 expected=$2 rc=0 result
  env DATA_DIR="$S/data" WORK_DIR="$S/work" REGISTRY_CONFIG="$S/registry.json" \
    WORLD_BUNDLE_REPOSITORY=ghcr.io/lepinoid/world-bundle WORLD_BUNDLE_DIGEST="$desired" \
    ORAS_BIN="$FAKE" BB="$BB" FAKE_ORAS_STORE="$S/store" FAKE_ORAS_LOG="$S/oras.log" \
    "$BB" sh "$SCRIPT" > "$S/run.log" 2>&1 || rc=$?
  assert_eq "$rc" 0
  result=$(sed -n 's/^RESULT=\([^ ]*\).*/\1/p' "$S/run.log")
  assert_eq "$result" "$expected"
}
marker() { jq -r .digest "$S/data/.worlds/current"; }
baseline() {
  bundle B terrestrial
  seed B
  B=$DIGEST
  run_init "$B" switched
  BEFORE=$(tree_sha "$S/data/terrestrial")
  MARKER_BEFORE=$(sha256sum "$S/data/.worlds/current")
}
unchanged() {
  assert_eq "$(tree_sha "$S/data/terrestrial")" "$BEFORE"
  assert_eq "$(sha256sum "$S/data/.worlds/current")" "$MARKER_BEFORE"
}
passed() { printf 'PASS %s\n' "$CASE"; }

new_case '01 empty digest: no-op, unchanged'
BEFORE=$(tree_sha "$S/data")
run_init '' no-op
assert_eq "$(tree_sha "$S/data")" "$BEFORE"
[[ ! -e "$S/data/.worlds-staging" ]] || diagnostics 'unexpected staging'
passed

new_case '02 malformed digest: pre-switch-failure, unchanged'
BEFORE=$(tree_sha "$S/data")
run_init 'sha256:../../bad' pre-switch-failure
assert_eq "$(tree_sha "$S/data")" "$BEFORE"
grep -q 'ERROR.*invalid digest' "$S/run.log"
passed

new_case '03 first boot: legacy replaced, marker written, staging removed'
bundle B terrestrial
seed B
B=$DIGEST
run_init "$B" switched
assert_eq "$(tree_sha "$S/data/terrestrial")" "$(tree_sha "$S/B/terrestrial")"
assert_eq "$(marker)" "$B"
[[ ! -e "$S/data/.worlds-staging" && ! -e "$S/data/.worlds-backup" ]] || diagnostics 'unexpected staging/backup'
passed

CASE='04 same digest: no-op without pull'
BEFORE=$(tree_sha "$S/data")
PULLS=$(grep -c ' pull ' "$S/oras.log")
run_init "$B" no-op
assert_eq "$(tree_sha "$S/data")" "$BEFORE"
assert_eq "$(grep -c ' pull ' "$S/oras.log")" "$PULLS"
passed

CASE='05 second switch: B to C'
bundle C terrestrial
seed C
run_init "$DIGEST" switched
assert_eq "$(tree_sha "$S/data/terrestrial")" "$(tree_sha "$S/C/terrestrial")"
assert_eq "$(marker)" "$DIGEST"
passed

for scenario in pull corrupt wrong-path duplicate bad-sha symlink traversal missing-level oci-tamper space; do
  case "$scenario" in
    pull) label='06 pull failure' ;;
    corrupt) label='07 corrupt store archive' ;;
    wrong-path) label='08a wrong archivePath' ;;
    duplicate) label='08b duplicate world names' ;;
    bad-sha) label='08c malformed archive sha256' ;;
    symlink) label='09 symlink archive member' ;;
    traversal) label='10 parent traversal archive member' ;;
    missing-level) label='11 missing level.dat' ;;
    oci-tamper) label='12 OCI manifest digest mismatch' ;;
    space) label='13 insufficient free space' ;;
  esac
  new_case "$label: pre-switch-failure, unchanged"
  baseline
  bundle C terrestrial
  case "$scenario" in
    wrong-path) mutate C '.worlds[0].archivePath="../evil.tar.gz"' ;;
    duplicate) mutate C '.worlds += [.worlds[0]]' ;;
    bad-sha) mutate C '.worlds[0].sha256="bad"' ;;
    symlink)
      ln -s level.dat "$S/C/terrestrial/link"
      tar -czf "$S/C/terrestrial.tar.gz" -C "$S/C/terrestrial" .
      refresh_archive C ;;
    traversal)
      tar -czf "$S/C/terrestrial.tar.gz" --transform='s|level.dat|../evil|' -C "$S/C/terrestrial" level.dat
      refresh_archive C ;;
    missing-level)
      rm "$S/C/terrestrial/level.dat"
      tar -czf "$S/C/terrestrial.tar.gz" -C "$S/C/terrestrial" .
      refresh_archive C ;;
    space) mutate C '.worlds[0].sizeBytes=9000000000000000' ;;
  esac
  seed C
  case "$scenario" in
    pull) export FAKE_ORAS_FAIL=pull ;;
    corrupt) printf corrupt >> "$S/store/blobs/${DIGEST#sha256:}/files/terrestrial.tar.gz" ;;
    oci-tamper) printf ' ' >> "$S/store/blobs/${DIGEST#sha256:}/oci-manifest.json" ;;
  esac
  run_init "$DIGEST" pre-switch-failure
  unchanged
  case "$scenario" in
    symlink) grep -q 'unsafe archive member type' "$S/run.log" ;;
    traversal) grep -q 'unsafe archive member path' "$S/run.log" ;;
    missing-level) grep -q 'missing level.dat' "$S/run.log" ;;
    space) grep -q 'insufficient free space' "$S/run.log" ;;
  esac
  [[ ! -e "$S/work/pull" && ! -e "$S/data/evil" ]] || diagnostics 'failed pull not cleaned or path escaped'
  passed
done

new_case '14 second-world switch failure: first world advanced, blocker retained, marker unchanged'
bundle B terrestrial lv1
seed B
B=$DIGEST
run_init "$B" switched
MARKER_BEFORE=$(sha256sum "$S/data/.worlds/current")
bundle C terrestrial lv1
seed C
C=$DIGEST
rm -rf "$S/data/lv1"
printf 'blocker\n' > "$S/data/lv1"
run_init "$C" post-switch-failure
assert_eq "$(tree_sha "$S/data/terrestrial")" "$(tree_sha "$S/C/terrestrial")"
assert_eq "$(cat "$S/data/lv1")" blocker
assert_eq "$(sha256sum "$S/data/.worlds/current")" "$MARKER_BEFORE"
grep -q 'best-effort restore' "$S/run.log"
passed

CASE='15 rerun after blocker removal: both worlds switched, marker advanced'
rm "$S/data/lv1"
run_init "$C" switched
for name in terrestrial lv1; do
  assert_eq "$(tree_sha "$S/data/$name")" "$(tree_sha "$S/C/$name")"
done
assert_eq "$(marker)" "$C"
[[ ! -e "$S/data/.worlds-staging" && ! -e "$S/work/pull" ]] || diagnostics 'temporary trees remain'
passed
printf 'all world-sync-init tests passed\n'
