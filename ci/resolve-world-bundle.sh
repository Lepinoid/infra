#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
ci=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$ci/lib/world-bundle-version.sh"
source "$ci/lib/world-manifest.sh"
INPUT_VERSION=${INPUT_VERSION:-}
INPUT_DIGEST=${INPUT_DIGEST:-}
REPOSITORY=${REPOSITORY:-ghcr.io/lepinoid/world-bundle}
ORAS_BIN=${ORAS_BIN:-oras}

if [[ -n "$INPUT_VERSION" && -n "$INPUT_DIGEST" ]] || [[ -z "$INPUT_VERSION" && -z "$INPUT_DIGEST" ]]; then
  echo "specify exactly one of version|digest" >&2
  exit 1
fi
if [[ -n "$INPUT_DIGEST" ]]; then
  [[ "$INPUT_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]]
  ref="$REPOSITORY@$INPUT_DIGEST"
else
  world_bundle_version_matches "$INPUT_VERSION"
  ref="$REPOSITORY:$INPUT_VERSION"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
printf '%s' "$GH_TOKEN" | "$ORAS_BIN" login ghcr.io -u "$GHCR_USER" --password-stdin >&2
digest=$("$ORAS_BIN" resolve "$ref")
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
pinned="$REPOSITORY@$digest"
"$ORAS_BIN" manifest fetch "$pinned" >"$work/oci.json"
jq -e '.artifactType == "application/vnd.lepinoid.world-bundle.v1" and (.layers | type == "array" and length >= 2)' "$work/oci.json" >/dev/null
"$ORAS_BIN" pull -o "$work/pulled" "$pinned" >&2
manifest="$work/pulled/manifest.json"
world_manifest_validate "$manifest"
world_manifest_verify_archives "$manifest" "$work/pulled"
version=$(jq -r .version "$manifest")
gitCommitSha=$(jq -r .gitCommitSha "$manifest")
if [[ -n "$INPUT_VERSION" ]]; then
  [[ "$INPUT_VERSION" == "$version" ]]
fi

if [[ -z "${OUT:-}" ]]; then
  if [[ -n "${RUNNER_TMP:-}" ]]; then
    OUT="$RUNNER_TMP/world-desired.json"
  else
    OUT=$(mktemp)
  fi
fi
jq -n --arg d "$digest" --arg v "$version" --arg g "$gitCommitSha" \
  '{digest:$d,version:$v,gitCommitSha:$g}' | tee "$OUT"
printf 'digest=%s\nversion=%s\ngitCommitSha=%s\n' "$digest" "$version" "$gitCommitSha" >&2
