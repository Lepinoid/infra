#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$SCRIPT_DIR/world-manifest.sh"
BUILD="$SCRIPT_DIR/../build-world-bundle.sh"
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
umask 022
w="$tmp/worlds"
config="$tmp/config.yaml"
mkdir -p "$w/terrestrial/region" "$w/terrestrial/DIM-1/data" "$w/lv1"
git -C "$w" init -q --object-format=sha1

fixture_commit() {
  git -C "$w" add -A
  GIT_AUTHOR_DATE=2026-09-09T20:00:00+00:00 \
    GIT_COMMITTER_DATE=2026-09-09T20:00:00+00:00 \
    git -C "$w" -c user.name='Bundle Test' -c user.email='bundle@example.invalid' \
      -c commit.gpgsign=false commit -q -m "$1"
}

write_config() {
  cat > "$config" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: world-sync-config
data:
  config.json: |
    {"schemaVersion":1,"worlds":$1}
EOF
}

expect_failure() {
  local label=$1
  if bash "$BUILD" "$w" "$config" "$tmp/failure-$label" >"$tmp/$label.log" 2>&1; then
    printf 'FAIL: %s unexpectedly succeeded\n' "$label" >&2
    exit 1
  fi
  printf 'PASS: %s rejected\n' "$label"
}

printf 'level data\n' > "$w/terrestrial/level.dat"
printf 'region data\n' > "$w/terrestrial/region/r.0.0.mca"
printf 'nether data\n' > "$w/terrestrial/DIM-1/data/x.dat"
printf 'excluded world\n' > "$w/lv1/level.dat"
printf '*.tmp\n' > "$w/.gitignore"
fixture_commit 'Fixed world fixture'
commit_sha=$(git -C "$w" rev-parse HEAD)
write_config '[{"name":"terrestrial","enabled":true},{"name":"lv1","enabled":false}]'
bash "$BUILD" "$w" "$config" "$tmp/out1"
test -f "$tmp/out1/manifest.json"
world_manifest_validate "$tmp/out1/manifest.json"
world_manifest_verify_archives "$tmp/out1/manifest.json" "$tmp/out1"
jq -e --arg sha "$commit_sha" '
  [.worlds[].name] == ["terrestrial"] and
  .version == ("2026.09.10-" + $sha[0:5]) and
  .createdAt == "2026-09-09T20:00:00Z" and
  .gitCommitSha == $sha
' "$tmp/out1/manifest.json" >/dev/null
printf 'PASS: manifest metadata and enabled worlds\n'

find "$w" -exec touch -d '+2 years' {} +
(
  umask 077
  GZIP=-9 bash "$BUILD" "$w" "$config" "$tmp/out2"
)
cmp "$tmp/out1/terrestrial.tar.gz" "$tmp/out2/terrestrial.tar.gz"
printf 'PASS: cmp out1/terrestrial.tar.gz out2/terrestrial.tar.gz (identical)\n'
cmp "$tmp/out1/manifest.json" "$tmp/out2/manifest.json"
printf 'PASS: cmp out1/manifest.json out2/manifest.json (identical)\n'

write_config '[{"name":"absent","enabled":true}]'
expect_failure missing-world
write_config '[{"name":"terrestrial","enabled":false}]'
expect_failure empty-enabled-list
write_config '[{"name":"terrestrial","enabled":true}]'
mv "$w/terrestrial/level.dat" "$tmp/level.dat"
expect_failure missing-level-dat
mv "$tmp/level.dat" "$w/terrestrial/level.dat"
ln -s level.dat "$w/terrestrial/link.dat"
fixture_commit 'Symlink fixture'
expect_failure committed-symlink

echo 'all build-world-bundle tests passed'
