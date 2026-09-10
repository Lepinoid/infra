WORLD_DIGEST_RE='^sha256:[0-9a-f]{64}$'
WORLD_VERSION_RE='^[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9a-f]{5}$'
WORLD_SHA_RE='^[0-9a-f]{40}$'

world_desired_write_configmap() {
  local digest=$1 version=$2 git_commit_sha=$3 out_file=$4
  [[ $digest =~ $WORLD_DIGEST_RE ]] || return 1
  [[ -z $version || $version =~ $WORLD_VERSION_RE ]] || return 1
  [[ -z $git_commit_sha || $git_commit_sha =~ $WORLD_SHA_RE ]] || return 1
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: world-versions\ndata:\n  digest: "%s"\n  version: "%s"\n  gitCommitSha: "%s"\n' \
    "$digest" "$version" "$git_commit_sha" > "$out_file"
}

world_desired_set_annotation() {
  local digest=$1 deployment=$2 tmp
  [[ $digest =~ $WORLD_DIGEST_RE ]] || return 1
  tmp=$(mktemp "${deployment}.tmp.XXXXXX") || return 1
  if ! awk -v digest="$digest" '
    BEGIN { template=0; template_metadata=0; annotations=0; key_count=0 }
    /^    template:[[:space:]]*$/ { template=1; next_line=0 }
    template && /^      metadata:[[:space:]]*$/ { template_metadata=1 }
    template_metadata && /^        annotations:[[:space:]]*$/ {
      annotations=1; print; next
    }
    template_metadata && /^        annotations:[[:space:]]*\{[[:space:]]*\}[[:space:]]*$/ {
      annotations=1; print "        annotations:"; print "          lepinoid.dev/world-bundle-digest: \"" digest "\""; next
    }
    template_metadata && /^        lepinoid\.dev\/world-bundle-digest:[[:space:]]*/ {
      key_count++; print "        lepinoid.dev/world-bundle-digest: \"" digest "\""; next
    }
    template_metadata && annotations && /^[^[:space:]]/ { if (key_count == 0) print "          lepinoid.dev/world-bundle-digest: \"" digest "\""; annotations=0 }
    { print }
    END {
      if (key_count > 1) exit 2
    }
  ' "$deployment" > "$tmp"; then
    rm -f "$tmp"; return 1
  fi
  # Add a missing annotations block immediately before the template metadata's labels/spec.
  if ! grep -q '^        lepinoid\.dev/world-bundle-digest:' "$tmp"; then
    awk -v digest="$digest" '
      /^      metadata:[[:space:]]*$/ && seen_template { print; print "        annotations:"; print "          lepinoid.dev/world-bundle-digest: \"" digest "\""; seen_template=0; next }
      /^    template:[[:space:]]*$/ { seen_template=1 }
      { print }
    ' "$tmp" > "${tmp}.2" && mv "${tmp}.2" "$tmp"
  fi
  mv "$tmp" "$deployment"
}

world_desired_update() {
  local digest=$1 version=$2 git_commit_sha=$3 repo_root=$4
  world_desired_write_configmap "$digest" "$version" "$git_commit_sha" "$repo_root/lepinoid/world-versions.yaml" || return 1
  world_desired_set_annotation "$digest" "$repo_root/lepinoid/deployment.yaml"
}
