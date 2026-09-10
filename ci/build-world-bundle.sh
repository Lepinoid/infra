#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
unset GZIP

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$SCRIPT_DIR/lib/world-bundle-version.sh"
source "$SCRIPT_DIR/lib/world-config.sh"
source "$SCRIPT_DIR/lib/world-manifest.sh"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ $# == 3 ]] || fail "Usage: $0 <worlds-checkout-dir> <config.yaml> <out-dir>"
w=$1
config=$2
out=$3
names=$(world_config_enabled_names "$config")
commit_sha=$(git -C "$w" rev-parse HEAD)
[[ "$commit_sha" =~ ^[0-9a-f]{40}$ ]] || fail 'HEAD must be a 40-character Git SHA'
cts=$(git -C "$w" log -1 --format=%ct)
version="$(TZ=Asia/Tokyo date -d "@$cts" +%Y.%m.%d)-${commit_sha:0:5}"
createdAt=$(date -u -d "@$cts" +%Y-%m-%dT%H:%M:%SZ)
world_bundle_version_matches "$version" || fail 'Invalid bundle version'
mkdir -p "$out"
worlds='[]'

while IFS= read -r name; do
  git -C "$w" ls-tree -d HEAD -- "$name" | grep -q . \
    || fail "World directory not present in HEAD: $name"
  test -f "$w/$name/level.dat" || fail "Missing level.dat: $name"
  entries=$(git -C "$w" ls-tree -r HEAD -- "$name")
  while IFS=$' \t' read -r mode rest; do
    case "$mode" in
      100644|100755|040000) ;;
      *) fail "Non-regular Git entry in $name: $mode $rest" ;;
    esac
  done <<< "$entries"

  tar --format=gnu --sort=name --mtime="@$cts" --owner=0 --group=0 --numeric-owner \
      --mode='u=rwX,go=rX' --no-acls --no-xattrs -C "$w/$name" -cf - . \
    | gzip -n -6 > "$out/$name.tar.gz"

  listing=$(tar -tzvf "$out/$name.tar.gz")
  while IFS= read -r entry; do
    [[ "${entry:0:1}" == d || "${entry:0:1}" == - ]] \
      || fail "Non-regular archive entry in $name: $entry"
    [[ "$entry" != *' -> '* ]] || fail "Archive link in $name: $entry"
  done <<< "$listing"
  paths=$(tar -tzf "$out/$name.tar.gz")
  while IFS= read -r path; do
    [[ "$path" != /* && "/$path/" != *'/../'* ]] \
      || fail "Unsafe archive path in $name: $path"
  done <<< "$paths"

  sha=$(sha256sum "$out/$name.tar.gz" | cut -d' ' -f1)
  size=$(stat -c %s "$out/$name.tar.gz")
  worlds=$(jq --arg name "$name" --arg ap "${name}.tar.gz" \
    --arg sha256 "$sha" --argjson sizeBytes "$size" \
    '. + [{name:$name,archivePath:$ap,sha256:$sha256,sizeBytes:$sizeBytes}]' <<< "$worlds")
done <<< "$names"

jq -n -S --arg v "$version" --arg c "$createdAt" --arg s "$commit_sha" \
  --argjson w "$worlds" \
  '{schemaVersion:1,version:$v,createdAt:$c,gitCommitSha:$s,worlds:$w}' > "$out/manifest.json"
world_manifest_validate "$out/manifest.json"
printf 'version: %s\ngitCommitSha: %s\n' "$version" "$commit_sha" >&2
jq -r '.worlds[] | "\(.name): sha256=\(.sha256) sizeBytes=\(.sizeBytes)"' \
  "$out/manifest.json" >&2
