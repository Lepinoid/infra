# Lepinoid GitHub Actions runner

ARC controller、listener、runner をすべて `lepinoid` namespace に配置する。
controller は `watchSingleNamespace: lepinoid` でこの namespace だけを管理する。
GitHub Actions は組織 `Lepinoid` の runner group `lepinoid-ci`（ID: 7）に登録する。
runner group は `Lepinoid/lepinoid` だけに公開し、公開リポジトリからは利用させない。

| scale set / `runs-on` | CPU request・limit | メモリ request・limit | 最大同時実行 | 待機数 |
|---|---:|---:|---:|---:|
| `lepi-ci-small` | 2 | 4 GiB | 2 | 0 |
| `lepi-ci-medium` | 4 | 8 GiB | 2 | 0 |

4 ジョブの合計は 12 CPU / 24 GiB。別途 controller と listener 2 台の予約として
200m CPU / 512 MiB が必要になる。既存ワークロードと合わせてクラスタ側の
ResourceQuota に収まることを確認してから有効にする。

runner は `toliworker-1/2/3` への node affinity と hostname による spread を持つ。
各 Pod は `longhorn-single` の generic ephemeral PVC を 40 GiB ずつ作成する。
workspace、Gradle cache、tool cache、`/nix`、`/tmp` はこの PVC の別ディレクトリを使い、
Pod 削除時に回収される。4 並列で最大 160 GiB の論理容量が必要となる。
データを再取得できる CI 用途なのでレプリカ数は 1 とし、ジョブ間の cache 共有はしない。

## クラスタ側の前提

以下は `turtton/infra` 側で管理する。

- ARC 0.15.1 の CRD。controller HelmRelease は install / upgrade とも CRD を `Skip` する。
- `lepinoid` の Helm 操作用 ServiceAccount `lepinoid-helm` と namespace 内の RBAC。
- runner 4 台と既存サービスを収容する ResourceQuota。
- 既存の `longhorn-single` StorageClass と Cilium。

このディレクトリには namespace、CRD、ClusterRole、PV を作成するリソースを含めない。
Helm chart は OCI digest で固定し、`operation: copy` で chart の tarball を取り込む。
single-namespace 設定により controller chart からも ClusterRole / ClusterRoleBinding は生成しない。

## GitHub App と Secret

既存 App `lepinoid-vuddy`（App ID: 4518305、installation ID: 151985587）を使う。
この App は `Lepinoid` にインストールされ、組織の self-hosted runners の管理権限を持つ。
`github-app-secret.sops.yaml` には暗号化済みの Secret `arc-github-app` があり、
キーは `github_app_id`、`github_app_installation_id`、`github_app_private_key`。
各 scale set は Secret の名前だけを参照する。秘密鍵を Helm values や README に記載しない。

## 通信と実行権限

runner は権限を持たない `arc-runner` ServiceAccount を使い、API token は mount しない。
Docker daemon、privileged container、hostPath、ホストの Docker socket は使わない。
Nix が生成する画像は archive として扱い、daemon を必要としない処理で検証・転送する。
Nix installer はコンテナ向けの非 systemd / sandbox 無効設定を使う。
Skopeo の大きな一時ファイルには `TMPDIR` だけでは不十分なので、各コマンドに
`--tmpdir "$RUNNER_TEMP/skopeo"` を指定して PVC に置く。
client harness のような追加依存があるジョブは別途動作検証する。

`arc-runner-isolation` の Cilium deny policy は、テナント共通の同一 namespace allow より優先される。
runner が使えるのは CoreDNS の TCP/UDP 53 と公開 IPv4 向けの通信。
同一 namespace のアプリ、他 namespace の Pod、Kubernetes API、ホスト、LAN、Tailscale 宛ては遮断する。
ジョブ内プロセス同士の loopback 通信は利用できる。
controller / listener は Kubernetes API、DNS、GitHub 等の公開 IPv4 に接続する。

## 試験導入

1. クラスタ側の CRD / RBAC / quota を準備し、暗号化 Secret を配置する。
2. `ci/check-manifests.sh` を実行する。controller と runner chart を `helm template` で別途 render し、
   cluster-scope リソースがなく、すべての container / initContainer に CPU・memory の requests / limits があることも確認する。
3. controller の Ready と runner group、Secret、通信分離を確認する。
4. 両 runner HelmRelease は有効、`minRunners: 0` なのでジョブがなければ runner は起動しない。
5. small と medium でチェックを行い、同一 revision・cache 条件で CPU / worker 数を比較する。
   続いて small 2 台 + medium 2 台を同時実行し、OOM / eviction / DiskPressure がなく、
   既存サービスの応答とノードのメモリ余力を保てることを確認する。
6. 新規ジョブの受け付けを止めるときは、
   両 scale set の `minRunners` / `maxRunners` を 0 にして反映し、未完了ジョブと runner Pod / PVC の回収を確認する。
   HelmRelease の suspend だけでは、インストール済みの ARC による runner 作成は停止しない。

GitHub Actions では `runs-on: lepi-ci-small` または `runs-on: lepi-ci-medium` で scale set を選ぶ。
controller の資格情報を runner に渡す設定は不要。
