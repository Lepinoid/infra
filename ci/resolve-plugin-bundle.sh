#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
repository=ghcr.io/lepinoid/lepinoid-tools
if [[ "$GITHUB_EVENT_NAME" == workflow_dispatch ]]; then
  version=$INPUT_VERSION
else
  version=$(jq -er '.client_payload.version' "$GITHUB_EVENT_PATH")
fi
if [[ "$version" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  ref="$repository@$version"
else
  [[ "$version" =~ ^[0-9]{4}\.[0-9]{2}\.[0-9]{2}-1\.21\.1-[0-9a-f]{5}$ ]]
  ref="$repository:$version"
fi
printf '%s' "$GH_TOKEN" | oras login ghcr.io -u "$GHCR_USER" --password-stdin
digest=$(oras resolve "$ref")
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
pinned="$repository@$digest"
oras manifest fetch "$pinned" > "$RUNNER_TEMP/bundle-manifest.json"
jq -e '.schemaVersion == 2 and (.layers|length) == 1' "$RUNNER_TEMP/bundle-manifest.json" > /dev/null
mkdir "$RUNNER_TEMP/bundle-pull" "$RUNNER_TEMP/bundle-extract"
oras pull "$pinned" -o "$RUNNER_TEMP/bundle-pull"
mapfile -d '' files < <(find "$RUNNER_TEMP/bundle-pull" -mindepth 1 -maxdepth 1 -print0)
test "${#files[@]}" -eq 1
test -f "${files[0]}" && test ! -L "${files[0]}"
tar -tf "${files[0]}" | sort > "$RUNNER_TEMP/members"
printf '%s\n' LepinoidTools.jar Multiverse-Core.jar compatibility.json > "$RUNNER_TEMP/expected-members"
diff -u "$RUNNER_TEMP/expected-members" "$RUNNER_TEMP/members"
tar -tvf "${files[0]}" > "$RUNNER_TEMP/verbose-members"
if grep -v '^-rw-' "$RUNNER_TEMP/verbose-members"; then exit 1; fi
tar --no-same-owner --no-same-permissions -xf "${files[0]}" -C "$RUNNER_TEMP/bundle-extract"
metadata="$RUNNER_TEMP/bundle-extract/compatibility.json"
jq -e '
  (keys|sort) == (["schemaVersion","lepinoidTools","multiverseCore","supportedMinecraft"]|sort) and
  .schemaVersion == 1 and
  (.lepinoidTools|keys|sort) == (["file","version","commitSha","sha256"]|sort) and
  (.multiverseCore|keys|sort) == (["file","version","sha256"]|sort) and
  .lepinoidTools.file == "LepinoidTools.jar" and .multiverseCore.file == "Multiverse-Core.jar" and
  (.lepinoidTools.version|test("^[0-9]{4}\\.[0-9]{2}\\.[0-9]{2}-1\\.21\\.1-[0-9a-f]{5}$")) and
  (.lepinoidTools.commitSha|test("^[0-9a-f]{40}$")) and
  (.lepinoidTools.sha256|test("^[0-9a-f]{64}$")) and
  (.multiverseCore.sha256|test("^[0-9a-f]{64}$")) and
  (.multiverseCore.version|type == "string" and length > 0) and
  (.supportedMinecraft|type == "array" and length > 0 and all(.[];type == "string"))
' "$metadata" > /dev/null
for component in lepinoidTools multiverseCore; do
  name=$(jq -er ".$component.file" "$metadata")
  expected=$(jq -er ".$component.sha256" "$metadata")
  printf '%s  %s\n' "$expected" "$RUNNER_TEMP/bundle-extract/$name" | sha256sum -c -
done
jq --arg digest "$digest" --arg repository "$repository" '{schemaVersion:1,version:.lepinoidTools.version,ociRepository:$repository,digest:$digest,pluginCommitSha:.lepinoidTools.commitSha,lepinoidToolsSha256:.lepinoidTools.sha256,multiverseSha256:.multiverseCore.sha256,multiverseVersion:.multiverseCore.version,supportedMinecraft:.supportedMinecraft}' "$metadata" > "$RUNNER_TEMP/desired.json"
if [[ "$version" != sha256:* ]]; then
  test "$version" = "$(jq -er .version "$RUNNER_TEMP/desired.json")"
fi
if [[ "$GITHUB_EVENT_NAME" == repository_dispatch ]]; then
  jq -e --slurpfile desired "$RUNNER_TEMP/desired.json" '
    .client_payload as $p | $desired[0] as $d |
    $p.version == $d.version and $p.digest == $d.digest and
    $p.commit_sha == $d.pluginCommitSha and
    $p.lepinoid_tools_sha256 == $d.lepinoidToolsSha256 and
    $p.multiverse_sha256 == $d.multiverseSha256 and
    $p.multiverse_version == $d.multiverseVersion and
    $p.supported_minecraft == $d.supportedMinecraft
  ' "$GITHUB_EVENT_PATH" > /dev/null
fi
