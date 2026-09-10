#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$script_dir/lib/world-manifest.sh"
[[ $# == 1 ]] || { printf 'usage: %s <bundle-dir>\n' "$0" >&2; exit 1; }
bundle=$1
REPOSITORY=${REPOSITORY:-ghcr.io/lepinoid/world-bundle}
ORAS_BIN=${ORAS_BIN:-oras}
: "${GHCR_USER:?}" "${GHCR_TOKEN:?}"
fail() { printf '%s\n' "$*" >&2; exit 1; }
m=$(find "$bundle" -maxdepth 1 -name manifest.json)
[[ -n $m ]] || fail 'missing manifest.json'
world_manifest_validate "$bundle/manifest.json" || fail 'invalid manifest.json'
version=$(jq -r .version "$bundle/manifest.json")
ref=$REPOSITORY:$version
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
artifact_type=application/vnd.lepinoid.world-bundle.v1
manifest_mt=application/vnd.lepinoid.world-bundle.manifest.v1+json
archive_mt=application/vnd.lepinoid.world-bundle.archive.v1.tar+gzip

printf '%s' "$GHCR_TOKEN" | "$ORAS_BIN" login "${REPOSITORY%%/*}" -u "$GHCR_USER" --password-stdin >&2
if "$ORAS_BIN" manifest fetch "$ref" >"$work/oci.json" 2>"$work/fetch-error"; then
  digest=$("$ORAS_BIN" resolve "$ref")
  [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || fail 'invalid resolved digest'
  pinned=$REPOSITORY@$digest
  "$ORAS_BIN" manifest fetch "$pinned" >"$work/oci.json"
  layer_count=$(jq '.worlds | length + 1' "$bundle/manifest.json")
  jq -e --arg type "$artifact_type" --argjson count "$layer_count" '
    .artifactType == $type and (.layers | length == $count)
  ' "$work/oci.json" >/dev/null || fail 'existing artifactType or layer count mismatch'
  mkdir "$work/pulled"
  "$ORAS_BIN" pull -o "$work/pulled" "$pinned" >&2
  world_manifest_equal "$bundle/manifest.json" "$work/pulled/manifest.json" ||
    fail "immutable tag collision: $ref exists with different manifest.json"
  world_manifest_verify_archives "$work/pulled/manifest.json" "$work/pulled" ||
    fail 'existing artifact archive integrity failure'
  while IFS= read -r archive; do
    pulled_hash=$(sha256sum "$work/pulled/$archive" | cut -d' ' -f1)
    built_hash=$(sha256sum "$bundle/$archive" | cut -d' ' -f1)
    [[ $pulled_hash == "$built_hash" ]] || fail "archive checksum mismatch: $archive"
  done < <(jq -r '.worlds[].archivePath' "$bundle/manifest.json")
  printf 'reusing existing immutable artifact\n' >&2
else
  # Authentication/transport failures must not be mistaken for an absent tag.
  if ! grep -Eqi 'MANIFEST_UNKNOWN|manifest unknown|not found|404' "$work/fetch-error"; then
    cat "$work/fetch-error" >&2
    exit 1
  fi
  args=(push "$ref" --artifact-type "$artifact_type" "manifest.json:$manifest_mt")
  while IFS= read -r archive; do
    args+=("$archive:$archive_mt")
  done < <(jq -r '.worlds[].archivePath' "$bundle/manifest.json")
  args+=(--annotation "org.opencontainers.image.revision=$(jq -r .gitCommitSha "$bundle/manifest.json")"
    --annotation 'org.opencontainers.image.source=https://github.com/Lepinoid/Worlds'
    --annotation "org.opencontainers.image.created=$(jq -r .createdAt "$bundle/manifest.json")")
  if [[ -n ${BUILD_URL:-} ]]; then
    args+=(--annotation "net.lepinoid.build-url=$BUILD_URL")
  fi
  (cd "$bundle" && "$ORAS_BIN" "${args[@]}") >"$work/push-output"
  cat "$work/push-output" >&2
  digest=$(sed -n 's/^Digest: *\(sha256:[0-9a-f]*\).*/\1/p' "$work/push-output")
fi
[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || fail 'missing or invalid digest'
printf '%s\n' "$digest"
if [[ -n ${GITHUB_OUTPUT:-} ]]; then
  printf 'digest=%s\n' "$digest" >>"$GITHUB_OUTPUT"
fi
