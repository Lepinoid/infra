#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
source ./world-manifest.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

valid='{"schemaVersion":1,"version":"2026.09.10-01234","createdAt":"2026-09-09T20:00:00Z","gitCommitSha":"0123456789012345678901234567890123456789","worlds":[{"name":"terrestrial","archivePath":"terrestrial.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":3}]}'
printf '%s\n' "$valid" > "$tmp/valid.json"
world_manifest_validate "$tmp/valid.json"

fail_json() { local expr=$1; jq "$expr" "$tmp/valid.json" > "$tmp/bad.json"; if world_manifest_validate "$tmp/bad.json"; then printf 'FAIL: %s\n' "$expr"; exit 1; fi; }
fail_json 'del(.schemaVersion)'
fail_json '.extra = true'
fail_json '.schemaVersion = 2'
fail_json '.version = "bad"'
fail_json '.version = "2026.09.10-99999"'
fail_json '.createdAt = "not-iso"'
fail_json '.worlds[0].extra = true'
fail_json '.worlds[0].name = "-bad"'
fail_json '.worlds[0].archivePath = "other.tar.gz"'
fail_json '.worlds[0].sha256 = "abc"'
fail_json '.worlds[0].sizeBytes = -1'
fail_json '.worlds[0].sizeBytes = 1.5'
fail_json '.worlds[0].sizeBytes = "3"'
fail_json '.version = "2026.09.09-01234"'
fail_json '.worlds += [{"name":"aardvark","archivePath":"aardvark.tar.gz","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":0}]'
fail_json '.worlds += [.worlds[0]]'

printf '%s\n' '{"worlds":[{"sizeBytes":3,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","archivePath":"terrestrial.tar.gz","name":"terrestrial"}],"gitCommitSha":"0123456789012345678901234567890123456789","createdAt":"2026-09-09T20:00:00Z","version":"2026.09.10-01234","schemaVersion":1}' > "$tmp/reordered.json"
world_manifest_equal "$tmp/valid.json" "$tmp/reordered.json"
jq '.version = "2026.09.11-01234"' "$tmp/valid.json" > "$tmp/changed.json"
if world_manifest_equal "$tmp/valid.json" "$tmp/changed.json"; then exit 1; fi

dir="$tmp/archives"; mkdir "$dir"; printf abc > "$dir/terrestrial.tar.gz"; sha=$(sha256sum "$dir/terrestrial.tar.gz" | cut -d' ' -f1); size=$(stat -c '%s' "$dir/terrestrial.tar.gz")
jq --arg sha "$sha" --argjson size "$size" '.worlds[0].sha256=$sha | .worlds[0].sizeBytes=$size' "$tmp/valid.json" > "$dir/manifest.json"
world_manifest_verify_archives "$dir/manifest.json" "$dir"
printf abc > "$dir/terrestrial.tar.gz"; jq '.worlds[0].sizeBytes=99' "$dir/manifest.json" > "$tmp/wrong.json"; cp "$tmp/wrong.json" "$dir/manifest.json"; if world_manifest_verify_archives "$dir/manifest.json" "$dir"; then exit 1; fi
printf abc > "$dir/terrestrial.tar.gz"; jq '.worlds[0].sizeBytes=3' "$tmp/valid.json" > "$dir/manifest.json"; touch "$dir/extra"; if world_manifest_verify_archives "$dir/manifest.json" "$dir"; then exit 1; fi
rm "$dir/extra" "$dir/terrestrial.tar.gz"; if world_manifest_verify_archives "$dir/manifest.json" "$dir"; then exit 1; fi
printf 'all world-manifest tests passed\n'
