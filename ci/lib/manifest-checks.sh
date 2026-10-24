# shellcheck shell=bash
# マニフェストの静的チェック。ci/check-manifests.sh から使う。
# 各 check_* は問題を 1 行ずつ stderr に出し、問題があれば 1 を返す。
# 依存: yq (mikefarah/yq v4)。render_and_validate はさらに kustomize と kubeconform を使う。

# この版の Kubernetes の API スキーマで検証する。クラスタ（turtton/infra）の更新に合わせて上げる。
MANIFEST_KUBERNETES_VERSION=${MANIFEST_KUBERNETES_VERSION:-1.35.0}
# kubeconform の標準スキーマに含まれない CRD。CRD スキーマは検証しない。
MANIFEST_SKIP_KINDS=${MANIFEST_SKIP_KINDS:-HelmRelease,HelmRepository,CiliumNetworkPolicy}
# tag だけでなく digest まで固定しなければならない image。
# plugin-updater は initContainer・sidecar・CronJob で同じ版が動く前提のため、可変 tag を許さない。
MANIFEST_DIGEST_REQUIRED_IMAGES=${MANIFEST_DIGEST_REQUIRED_IMAGES:-ghcr.io/lepinoid/plugin-updater}

# 先頭がドットでないトップレベルディレクトリ直下の YAML を、ROOT からの相対パスで列挙する。
manifest_files() {
  local root=$1
  (cd "$root" && find . -mindepth 2 -maxdepth 2 -type f \( -name '*.yaml' -o -name '*.yml' \) -not -path './.*' |
    sed 's|^\./||' | LC_ALL=C sort)
}

# kustomization.yaml の resources から参照されていないマニフェストを列挙する
# （rescue Pod のように kubectl apply で手動適用するもの）。
manifest_standalone_files() {
  local root=$1 file resource referenced
  local -a resources
  mapfile -t resources < <(yq e '.resources[]' "$root/kustomization.yaml")
  while IFS= read -r file; do
    referenced=false
    for resource in "${resources[@]}"; do
      resource=${resource#./}
      resource=${resource%/}
      if [[ $file == "$resource" || $file == "$resource"/* ]]; then
        referenced=true
        break
      fi
    done
    "$referenced" || printf '%s\n' "$file"
  done < <(manifest_files "$root")
}

# Pod を作る全コンテナ（initContainers を含む）に requests/limits の cpu・memory があること。
# ResourceQuota により、1 つでも欠けた Pod は作成を拒否される。
check_resources() {
  local root=$1 file line status=0
  shift
  for file in "$@"; do
    while IFS= read -r line; do
      [[ -n $line ]] || continue
      echo "resources: $file: $line" >&2
      status=1
    done < <(yq e -N '
      . as $doc
      | .. | select(tag == "!!map") | select((.containers | tag) == "!!seq")
      | ((.containers // []) + (.initContainers // [])) | .[]
      | . as $c
      | [
          {"field": "requests.cpu", "missing": ($c.resources.requests.cpu == null)},
          {"field": "requests.memory", "missing": ($c.resources.requests.memory == null)},
          {"field": "limits.cpu", "missing": ($c.resources.limits.cpu == null)},
          {"field": "limits.memory", "missing": ($c.resources.limits.memory == null)}
        ]
      | map(select(.missing)) | map(.field)
      | select(length > 0)
      | $doc.kind + "/" + $doc.metadata.name + " container " + $c.name + " is missing " + join(", ")
    ' "$root/$file")
  done
  return "$status"
}

# *.sops.yaml は SOPS で暗号化済み（age の受信者と MAC があり、data/stringData の値がすべて ENC[...]）であること。
# それ以外のファイルに、値を持つ Secret を置かないこと。
check_sops() {
  local root=$1 file line status=0
  shift
  for file in "$@"; do
    case "$file" in
      *.sops.yaml | *.sops.yml)
        # yq の文字列リテラルは入力が空でも出力されるため、select の結果に代入して出す。
        while IFS= read -r line; do
          [[ -n $line ]] || continue
          echo "sops: $file: $line" >&2
          status=1
        done < <(yq e -N '
          select(.sops.mac == null or ((.sops.age // []) | length) == 0)
          | . = "missing sops metadata (age recipients and mac); run sops --encrypt --in-place"
        ' "$root/$file"; yq e -N '
          ((.data // {}) * (.stringData // {})) | to_entries | .[]
          | select(.value | tostring | test("^ENC\\[") | not)
          | "value of " + .key + " is not encrypted"
        ' "$root/$file")
        ;;
      *)
        while IFS= read -r line; do
          [[ -n $line ]] || continue
          echo "sops: $file: $line" >&2
          status=1
        done < <(yq e -N '
          select(.kind == "Secret" and (.data != null or .stringData != null))
          | "Secret/" + .metadata.name + " has values outside a *.sops.yaml file"
        ' "$root/$file")
        ;;
    esac
  done
  return "$status"
}

# image 参照を "FILE<TAB>IMAGE" で列挙する。
manifest_images() {
  local root=$1 file image
  shift
  for file in "$@"; do
    while IFS= read -r image; do
      [[ -n $image ]] && printf '%s\t%s\n' "$file" "$image"
    done < <(yq e -N '.. | select(tag == "!!map") | select((.image | tag) == "!!str") | .image' "$root/$file")
  done
}

# image 参照から tag と digest を除いた repository を返す。
image_repository() {
  local name=${1%%@*}
  # ":" が最後の "/" より後ろにあるときだけ tag とみなす（registry のポート番号を残す）。
  if [[ ${name##*/} == *:* ]]; then
    name=${name%:*}
  fi
  printf '%s\n' "$name"
}

# digest 固定のルール:
# - MANIFEST_DIGEST_REQUIRED_IMAGES の image は tag@sha256:<64 桁> で固定する
# - digest を付けるなら sha256 の完全な値にする
# - 同じトップレベルディレクトリで digest 固定した同じ repository は、すべて同じ参照にする
check_digest_pins() {
  local root=$1 file image repo key status=0
  shift
  local -A first_ref first_file
  while IFS=$'\t' read -r file image; do
    repo=$(image_repository "$image")
    if [[ $image == *@* && ! $image =~ @sha256:[0-9a-f]{64}$ ]]; then
      echo "digest: $file: $image has an invalid digest" >&2
      status=1
      continue
    fi
    if [[ " $MANIFEST_DIGEST_REQUIRED_IMAGES " == *" $repo "* && $image != *@* ]]; then
      echo "digest: $file: $image must be pinned as <tag>@sha256:<digest>" >&2
      status=1
      continue
    fi
    [[ $image == *@* ]] || continue
    key="${file%%/*} $repo"
    if [[ -z ${first_ref[$key]:-} ]]; then
      first_ref[$key]=$image
      first_file[$key]=$file
    elif [[ ${first_ref[$key]} != "$image" ]]; then
      echo "digest: $file: $image differs from ${first_ref[$key]} in ${first_file[$key]}" >&2
      status=1
    fi
  done < <(manifest_images "$root" "$@")
  return "$status"
}

# kustomize build が通り、出力と手動適用マニフェストが Kubernetes のスキーマに合うこと。
render_and_validate() {
  local root=$1 rendered status=0
  local -a standalone
  rendered=$(mktemp)
  if ! kustomize build "$root" >"$rendered"; then
    echo "render: kustomize build failed" >&2
    rm -f "$rendered"
    return 1
  fi
  mapfile -t standalone < <(manifest_standalone_files "$root")
  if ((${#standalone[@]} > 0)); then
    (cd "$root" && yq e 'select(has("kind"))' "${standalone[@]}") | sed '1i---' >>"$rendered"
  fi
  # SOPS のメタデータ (sops:) は Kubernetes のフィールドではないため除いてから検証する。
  if ! yq e 'del(.sops)' "$rendered" |
    kubeconform -strict -summary -kubernetes-version "$MANIFEST_KUBERNETES_VERSION" -skip "$MANIFEST_SKIP_KINDS" -; then
    echo "render: kubeconform reported invalid manifests" >&2
    status=1
  fi
  rm -f "$rendered"
  return "$status"
}
