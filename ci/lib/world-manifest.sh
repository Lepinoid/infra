#!/usr/bin/env bash

world_manifest_validate() {
  local file=$1
  jq -e '
    (. | keys | sort) == ["createdAt","gitCommitSha","schemaVersion","version","worlds"] and
    .schemaVersion == 1 and (.gitCommitSha | type == "string" and test("^[0-9a-f]{40}$")) and
    (.version | type == "string" and test("^[0-9]{4}\\.[0-9]{2}\\.[0-9]{2}-[0-9a-f]{5}$")) and
    (.version | split("-")[1]) == (.gitCommitSha[0:5]) and
    (.createdAt | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
    (.worlds | type == "array" and length > 0 and all(.[]; (keys | sort) == ["archivePath","name","sha256","sizeBytes"] and
      (.name | type == "string" and test("^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")) and .archivePath == (.name + ".tar.gz") and
      (.sha256 | type == "string" and test("^[0-9a-f]{64}$")) and (.sizeBytes | type == "number" and floor == . and . >= 0))) and
    ([.worlds[].name] | . == (sort | unique))
  ' "$file" >/dev/null || return 1
  local date_part expected
  date_part=$(jq -r '.createdAt' "$file") || return 1
  expected=$(TZ=Asia/Tokyo date -d "$date_part" +%Y.%m.%d) || return 1
  [[ "$expected" == "$(jq -r '.version | split("-")[0]' "$file")" ]]
}

world_manifest_equal() { [[ "$(jq -S -c . "$1")" == "$(jq -S -c . "$2")" ]]; }

world_manifest_verify_archives() {
  local manifest=$1 dir=$2 path name expected actual size
  [[ -f "$manifest" && -d "$dir" ]] || return 1
  world_manifest_validate "$manifest" || return 1
  while IFS=$'\t' read -r name path expected size; do
    [[ -f "$dir/$path" && ! -L "$dir/$path" ]] || return 1
    actual=$(sha256sum "$dir/$path" | cut -d' ' -f1) || return 1
    [[ "$actual" == "$expected" && "$(stat -c '%s' "$dir/$path")" == "$size" ]] || return 1
    printf '%s: sha256=%s sizeBytes=%s\n' "$name" "$actual" "$size" >&2
  done < <(jq -r '.worlds[] | [.name,.archivePath,.sha256,.sizeBytes] | @tsv' "$manifest")
  [[ "$(find "$dir" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort)" == "$( { printf 'manifest.json\n'; jq -r '.worlds[].archivePath' "$manifest"; } | sort)" ]]
}
