#!/usr/bin/env bash
set -euo pipefail

fake_oras_store=${FAKE_ORAS_STORE:?FAKE_ORAS_STORE is required}
mkdir -p "$fake_oras_store/tags" "$fake_oras_store/blobs"

fake_oras_fail() {
  local verb=$1
  case ",${FAKE_ORAS_FAIL:-}," in
    *,"$verb",*) printf 'fake oras: injected failure %s\n' "$verb" >&2; exit 1 ;;
  esac
}

fake_oras_log() {
  [[ -n "${FAKE_ORAS_LOG:-}" ]] || return 0
  printf '%q ' "$0" "$@" >>"$FAKE_ORAS_LOG"
  printf '\n' >>"$FAKE_ORAS_LOG"
}

fake_oras_digest_for_ref() {
  local ref=$1 tag safe
  if [[ $ref == *@sha256:* ]]; then
    printf '%s\n' "${ref#*@}"
    return 0
  fi
  tag=${ref##*:}
  safe=${tag//\//_}
  [[ -f $fake_oras_store/tags/$safe ]] || return 1
  tr -d '\n' <"$fake_oras_store/tags/$safe"
  printf '\n'
}

fake_oras_require_digest() {
  local ref=$1 digest hex
  if ! digest=$(fake_oras_digest_for_ref "$ref"); then
    printf 'Error: %s: not found\n' "$ref" >&2
    return 1
  fi
  hex=${digest#sha256:}
  [[ -f $fake_oras_store/blobs/$hex/oci-manifest.json ]] || {
    printf 'Error: %s: not found\n' "$ref" >&2
    return 1
  }
  printf '%s\n' "$digest"
}

fake_oras_manifest_fetch() {
  local descriptor=0 registry_config='' ref digest hex manifest
  while (($#)); do
    case $1 in
      --descriptor) descriptor=1; shift ;;
      --registry-config) registry_config=$2; shift 2 ;;
      *) ref=$1; shift; break ;;
    esac
  done
  [[ -z $registry_config || -e $registry_config ]] || { printf 'registry config not found\n' >&2; return 1; }
  digest=$(fake_oras_digest_for_ref "$ref" 2>/dev/null || true)
  if [[ -z $digest || ! -f $fake_oras_store/blobs/${digest#sha256:}/oci-manifest.json ]]; then
    printf '%s\n' "${FAKE_ORAS_FETCH_ERROR:-Error: $ref: not found}" >&2
    return 1
  fi
  hex=${digest#sha256:}; manifest=$fake_oras_store/blobs/$hex/oci-manifest.json
  if ((descriptor)); then
    jq -cn --arg digest "$digest" --arg size "$(wc -c <"$manifest")" \
      '{mediaType:"application/vnd.oci.image.manifest.v1+json",digest:$digest,size:($size|tonumber)}'
  else
    cat "$manifest"
  fi
}

fake_oras_pull() {
  local registry_config='' out='' ref digest hex file
  while (($#)); do
    case $1 in
      --registry-config) registry_config=$2; shift 2 ;;
      -o) out=$2; shift 2 ;;
      *) ref=$1; shift; break ;;
    esac
  done
  [[ -z $registry_config || -e $registry_config ]] || { printf 'registry config not found\n' >&2; return 1; }
  digest=$(fake_oras_require_digest "$ref") || return
  hex=${digest#sha256:}; mkdir -p "$out"
  for file in "$fake_oras_store/blobs/$hex/files"/*; do
    [[ -e $file ]] || continue
    cp "$file" "$out/"
  done
  if [[ -n "${FAKE_ORAS_POST_PULL_HOOK:-}" ]]; then
    PULL_DIR=$out "$FAKE_ORAS_POST_PULL_HOOK"
  fi
}

fake_oras_push() {
  local ref=$1 artifact_type='' annotation file media_type digest hex safe manifest tmp
  shift
  [[ ${1:-} == --artifact-type ]] || { printf 'artifact type required\n' >&2; return 1; }
  artifact_type=$2; shift 2
  local -a annotations=() files=() media_types=()
  while (($#)); do
    if [[ $1 == --annotation ]]; then annotations+=("$2"); shift 2
    else
      file=${1%%:*}; media_type=${1#*:}; files+=("$file"); media_types+=("$media_type"); shift
    fi
  done
  tmp=$(mktemp); trap 'rm -f "$tmp"' RETURN
  printf '%s' '{}' >"$tmp"
  local config_digest; config_digest=$(sha256sum "$tmp" | cut -d' ' -f1)
  manifest=$(jq -cn --arg type "$artifact_type" --arg config "sha256:$config_digest" \
    --argjson layers '[]' --argjson anns '{}' '{schemaVersion:2,mediaType:"application/vnd.oci.image.manifest.v1+json",artifactType:$type,config:{mediaType:"application/vnd.oci.empty.v1+json",digest:$config,size:2},layers:$layers,annotations:$anns}')
  local i layer
  for i in "${!files[@]}"; do
    layer=$(jq -cn --arg mt "${media_types[$i]}" --arg d "sha256:$(sha256sum "${files[$i]}" | cut -d' ' -f1)" --arg title "$(basename "${files[$i]}")" --arg size "$(wc -c <"${files[$i]}")" '{mediaType:$mt,digest:$d,size:($size|tonumber),annotations:{"org.opencontainers.image.title":$title}}')
    manifest=$(jq -c --argjson layer "$layer" '.layers += [$layer]' <<<"$manifest")
  done
  for annotation in "${annotations[@]}"; do manifest=$(jq -c --arg a "$annotation" '($a|split("=")) as $p | .annotations[$p[0]]=$p[1]' <<<"$manifest"); done
  digest="sha256:$(printf '%s' "$manifest" | sha256sum | cut -d' ' -f1)"; hex=${digest#sha256:}
  mkdir -p "$fake_oras_store/blobs/$hex/files"
  printf '%s\n' "$manifest" >"$fake_oras_store/blobs/$hex/oci-manifest.json"
  for file in "${files[@]}"; do cp "$file" "$fake_oras_store/blobs/$hex/files/$(basename "$file")"; done
  safe=${ref##*:}; safe=${safe//\//_}; printf '%s\n' "$digest" >"$fake_oras_store/tags/$safe"
  printf 'Pushed [registry] %s\nArtifactType: %s\nDigest: %s\n' "$ref" "$artifact_type" "$digest"
}

if [[ "${FAKE_ORAS_AS_LIB:-0}" == 1 ]]; then
fake_oras_seed_bundle() {
  local store=$1 tag=$2 artifact_type=$3; shift 3
  local file media_type; local -a args=("repo:$tag" --artifact-type "$artifact_type")
  for file in "$@"; do
    media_type=application/vnd.lepinoid.world-bundle.archive.v1.tar+gzip
    [[ $(basename "$file") == manifest.json ]] && media_type=application/vnd.lepinoid.world-bundle.manifest.v1+json
    args+=("$file:$media_type")
  done
  FAKE_ORAS_STORE=$store fake_oras_push "${args[@]}" >/dev/null
}
fi

if [[ "${BASH_SOURCE[0]}" == "$0" && "${1:-}" == --self-test ]]; then
  store=$(mktemp -d); work=$(mktemp -d); a=$work/a.tar.gz; m=$work/manifest.json
  printf archive >"$a"; printf manifest >"$m"
  FAKE_ORAS_STORE=$store "$0" push repo:v1 --artifact-type test "$a":archive "$m":manifest >/dev/null
  d=$(FAKE_ORAS_STORE=$store "$0" resolve repo:v1); got=$(FAKE_ORAS_STORE=$store "$0" manifest fetch repo:v1); [[ sha256:$(printf %s "$got" | sha256sum | cut -d' ' -f1) == "$d" ]]
  FAKE_ORAS_STORE=$store "$0" manifest fetch --descriptor repo:v1 >/dev/null
  FAKE_ORAS_STORE=$store "$0" pull -o "$work/out" repo:v1; cmp "$a" "$work/out/$(basename "$a")"; cmp "$m" "$work/out/$(basename "$m")"
  d2=$(FAKE_ORAS_STORE=$store "$0" push repo:v2 --artifact-type test "$a":archive "$m":manifest | awk '/^Digest:/{print $2}'); [[ $d == "$d2" ]]
  printf 'fake-oras self-test passed\n'; exit 0
fi

fake_oras_log "$@"
case ${1:-} in
  login) fake_oras_fail login; cat >/dev/null; exit 0 ;;
  resolve) fake_oras_fail resolve; fake_oras_require_digest "$2" ;;
  manifest) fake_oras_fail manifest-fetch; [[ ${2:-} == fetch ]] || { printf 'fake oras: unknown manifest command\n' >&2; exit 1; }; shift 2; fake_oras_manifest_fetch "$@" ;;
  pull) fake_oras_fail pull; shift; fake_oras_pull "$@" ;;
  push) fake_oras_fail push; shift; fake_oras_push "$@" ;;
  version) printf 'oras version 1.2.3-fake\n' ;;
  *) printf 'fake oras: unknown command\n' >&2; exit 1 ;;
esac
