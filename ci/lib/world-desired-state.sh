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
    {
      # indentation = number of leading spaces
      n = 0
      while (substr($0, n + 1, 1) == " ") n++
      content = substr($0, n + 1)
      line_is_comment = (content ~ /^#/)

      # Pop context when we dedent to a sibling/parent level.
      if (in_annotations && !line_is_comment && n <= annotations_indent) in_annotations = 0
      if (in_template_metadata && !line_is_comment && n <= template_metadata_indent) {
        if (!found_key && had_annotations) print_key(annotations_indent + 2)
        in_template_metadata = 0
      }
      if (in_template && !line_is_comment && n <= template_indent) in_template = 0
    }
    in_template == 0 && content == "template:" && $0 ~ /^  template:[[:space:]]*$/ {
      in_template = 1; template_indent = n
    }
    in_template && content == "metadata:" && $0 ~ ("^" spaces_of(template_indent + 2) "metadata:[[:space:]]*$") {
      in_template_metadata = 1; template_metadata_indent = n
    }
    in_template_metadata && content == "annotations:" && $0 ~ ("^" spaces_of(template_metadata_indent + 2) "annotations:[[:space:]]*$") {
      in_annotations = 1; annotations_indent = n; had_annotations = 1
      print; next
    }
    in_annotations && !line_is_comment && content ~ /^lepinoid\.dev\/world-bundle-digest:/ {
      key_count++
      print spaces_of(n) "lepinoid.dev/world-bundle-digest: \"" digest "\""
      found_key = 1
      next
    }
    { print }
    END {
      if (key_count > 1) exit 2
      # annotations block existed but key was absent and block ran to EOF
      if (!found_key && in_template_metadata && had_annotations) print_key(annotations_indent + 2)
    }
    function print_key(indent) {
      print spaces_of(indent) "lepinoid.dev/world-bundle-digest: \"" digest "\""
    }
    function spaces_of(count,   i, s) {
      s = ""
      for (i = 0; i < count; i++) s = s " "
      return s
    }
  ' "$deployment" > "$tmp"; then
    rm -f "$tmp"; return 1
  fi
  # If no annotations block existed at all, create one under spec.template.metadata.
  if ! grep -q 'lepinoid\.dev/world-bundle-digest:' "$tmp"; then
    awk -v digest="$digest" '
      {
        n = 0
        while (substr($0, n + 1, 1) == " ") n++
        content = substr($0, n + 1)
      }
      $0 ~ /^      metadata:[[:space:]]*$/ && in_template {
        print
        printf "        annotations:\n"
        printf "          lepinoid.dev/world-bundle-digest: \"%s\"\n", digest
        done = 1
        next
      }
      $0 ~ /^  template:[[:space:]]*$/ { in_template = 1 }
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
