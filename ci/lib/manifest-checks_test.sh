#!/usr/bin/env bash
# manifest-checks.sh の yq だけで動くチェックを fixture で検証する（kustomize/kubeconform は使わない）。
set -euo pipefail
cd "$(dirname "$0")"
source ./manifest-checks.sh

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/app" "$root/other" "$root/.github"
errors="$root/errors.txt"

expect_ok() {
  if ! "$@" 2>"$errors"; then
    echo "expected success: $*" >&2
    cat "$errors" >&2
    exit 1
  fi
  if [[ -s $errors ]]; then
    echo "expected no findings: $*" >&2
    cat "$errors" >&2
    exit 1
  fi
}

expect_fail() {
  local pattern=$1
  shift
  if "$@" 2>"$errors"; then
    echo "expected failure: $*" >&2
    exit 1
  fi
  if ! grep -qF -- "$pattern" "$errors"; then
    echo "expected finding '$pattern': $*" >&2
    cat "$errors" >&2
    exit 1
  fi
}

# --- manifest_files / manifest_standalone_files ---
cat >"$root/kustomization.yaml" <<'YAML'
resources:
  - app/deployment.yaml
  - ./other/
YAML
touch "$root/app/deployment.yaml" "$root/app/rescue.yaml" "$root/other/cm.yml" "$root/.github/ci.yaml" "$root/top.yaml"
[[ "$(manifest_files "$root")" == $'app/deployment.yaml\napp/rescue.yaml\nother/cm.yml' ]]
[[ "$(manifest_standalone_files "$root")" == "app/rescue.yaml" ]]

# --- check_resources ---
cat >"$root/app/deployment.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ok
spec:
  template:
    spec:
      initContainers:
        - name: init
          resources:
            requests: {cpu: 5m, memory: 16Mi}
            limits: {cpu: 1, memory: 32Mi}
      containers:
        - name: main
          resources:
            requests: {cpu: 10m, memory: 32Mi}
            limits: {cpu: 20m, memory: 64Mi}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: no-pods
data:
  containers: not a list
YAML
expect_ok check_resources "$root" app/deployment.yaml

cat >"$root/app/cronjob.yaml" <<'YAML'
apiVersion: batch/v1
kind: CronJob
metadata:
  name: job
spec:
  jobTemplate:
    spec:
      template:
        spec:
          initContainers:
            - name: init
              resources:
                requests: {cpu: 5m}
                limits: {cpu: 10m, memory: 32Mi}
          containers:
            - name: main
YAML
expect_fail "CronJob/job container init is missing requests.memory" check_resources "$root" app/cronjob.yaml
expect_fail "CronJob/job container main is missing requests.cpu, requests.memory, limits.cpu, limits.memory" \
  check_resources "$root" app/cronjob.yaml
rm "$root/app/cronjob.yaml"

# --- check_sops ---
cat >"$root/app/secret.sops.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  name: encrypted
stringData:
  token: ENC[AES256_GCM,data:abc,type:str]
data:
  key: ENC[AES256_GCM,data:def,type:str]
sops:
  age:
    - recipient: age1example
      enc: dummy
  mac: ENC[AES256_GCM,data:mac,type:str]
YAML
expect_ok check_sops "$root" app/secret.sops.yaml

cat >"$root/app/plain.sops.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  name: plain
stringData:
  token: hunter2
YAML
expect_fail "app/plain.sops.yaml: missing sops metadata" check_sops "$root" app/plain.sops.yaml
expect_fail "app/plain.sops.yaml: value of token is not encrypted" check_sops "$root" app/plain.sops.yaml

cat >"$root/app/partial.sops.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  name: partial
data:
  ok: ENC[AES256_GCM,data:abc,type:str]
  added-later: cGxhaW4=
sops:
  age:
    - recipient: age1example
      enc: dummy
  mac: ENC[AES256_GCM,data:mac,type:str]
YAML
expect_fail "value of added-later is not encrypted" check_sops "$root" app/partial.sops.yaml

cat >"$root/app/leak.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  name: leak
stringData:
  token: hunter2
YAML
expect_fail "app/leak.yaml: Secret/leak has values outside a *.sops.yaml file" check_sops "$root" app/leak.yaml
rm "$root/app/plain.sops.yaml" "$root/app/partial.sops.yaml" "$root/app/leak.yaml"

# --- image_repository ---
[[ "$(image_repository busybox:1.36)" == busybox ]]
[[ "$(image_repository ghcr.io/lepinoid/plugin-updater:1-a@sha256:00)" == ghcr.io/lepinoid/plugin-updater ]]
[[ "$(image_repository registry.local:5000/app)" == registry.local:5000/app ]]
[[ "$(image_repository registry.local:5000/app:v1)" == registry.local:5000/app ]]

# --- check_digest_pins ---
digest_a=sha256:$(printf 'a%.0s' {1..64})
digest_b=sha256:$(printf 'b%.0s' {1..64})
pod_with_images() {
  local name=$1
  shift
  printf 'apiVersion: v1\nkind: Pod\nmetadata:\n  name: %s\nspec:\n  containers:\n' "$name"
  local image
  for image in "$@"; do
    printf '    - name: c\n      image: %s\n' "$image"
  done
}
pod_with_images a "ghcr.io/lepinoid/plugin-updater:1-a@$digest_a" busybox:1.36 >"$root/app/a.yaml"
pod_with_images b "ghcr.io/lepinoid/plugin-updater:1-a@$digest_a" >"$root/app/b.yaml"
pod_with_images c "ghcr.io/lepinoid/plugin-updater:0-old@$digest_b" >"$root/other/c.yaml"
expect_ok check_digest_pins "$root" app/a.yaml app/b.yaml other/c.yaml

pod_with_images b "ghcr.io/lepinoid/plugin-updater:2-b@$digest_b" >"$root/app/b.yaml"
expect_fail "app/b.yaml: ghcr.io/lepinoid/plugin-updater:2-b@$digest_b differs from ghcr.io/lepinoid/plugin-updater:1-a@$digest_a in app/a.yaml" \
  check_digest_pins "$root" app/a.yaml app/b.yaml

pod_with_images b ghcr.io/lepinoid/plugin-updater:1-a >"$root/app/b.yaml"
expect_fail "app/b.yaml: ghcr.io/lepinoid/plugin-updater:1-a must be pinned" check_digest_pins "$root" app/b.yaml

pod_with_images b busybox@sha256:abc >"$root/app/b.yaml"
expect_fail "app/b.yaml: busybox@sha256:abc has an invalid digest" check_digest_pins "$root" app/b.yaml

echo "manifest-checks tests passed"
