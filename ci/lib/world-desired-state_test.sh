#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
source ./world-desired-state.sh

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
version=2026.09.10-abcde
sha=0123456789012345678901234567890123456789

cat > "$tmp/deployment.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    metadata:
      annotations:
        lepinoid.dev/world-bundle-digest: "old"
        other: "keep"
    spec: {}
EOF
world_desired_set_annotation "$digest" "$tmp/deployment.yaml"
before=$(sha256sum "$tmp/deployment.yaml")
world_desired_set_annotation "$digest" "$tmp/deployment.yaml"
! world_desired_set_annotation sha256:bad "$tmp/deployment.yaml"
sed 's/^        other:/        lepinoid.dev\/world-bundle-digest: "duplicate"\n        other:/' "$tmp/deployment.yaml" > "$tmp/duplicate.yaml"
! world_desired_set_annotation "$digest" "$tmp/duplicate.yaml"
cat > "$tmp/no-annotations.yaml" <<'EOF'
spec:
  template:
    metadata:
      labels: {app: test}
    spec: {}
EOF
world_desired_set_annotation "$digest" "$tmp/no-annotations.yaml"
world_desired_write_configmap "$digest" "$version" "$sha" "$tmp/world.yaml"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: world-versions\ndata:\n  digest: "%s"\n  version: "%s"\n  gitCommitSha: "%s"\n' "$digest" "$version" "$sha" > "$tmp/expected.yaml"
cmp "$tmp/world.yaml" "$tmp/expected.yaml"
world_desired_write_configmap "$digest" '' '' "$tmp/initial.yaml"
if command -v yq >/dev/null; then yq '.' "$tmp/deployment.yaml" >/dev/null; elif command -v python3 >/dev/null && python3 -c 'import yaml' 2>/dev/null; then python3 -c 'import yaml,sys; yaml.safe_load(open(sys.argv[1]))' "$tmp/deployment.yaml"; fi
echo "all world-desired-state tests passed"
