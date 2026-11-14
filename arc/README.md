# Lepinoid GitHub Actions runner

ARC controller、listener、runner をすべて `lepinoid` namespace に配置する。
controller は `watchSingleNamespace: lepinoid` でこの namespace だけを管理する。
GitHub Actions は組織 `Lepinoid` の runner group `lepinoid-ci`（ID: 7）に登録する。
runner group は非公開リポジトリ `Lepinoid/lepinoid`（ID: 307067795）と
`Lepinoid/lepinoid-client`（ID: 518140849）だけに公開し、公開リポジトリからは利用させない。

| scale set / `runs-on` | CPU request・limit | メモリ request・limit | 最大同時実行 | 待機数 |
|---|---:|---:|---:|---:|
| `lepi-ci-small` | 2 | 4 GiB | 2 | 0 |
| `lepi-ci-medium` | 4 | 8 GiB | 3 | 0 |

この最大 5 ジョブの枠は両リポジトリで共有し、リポジトリごとの枠は設けない。

5 ジョブの合計は 16 CPU / 32 GiB。別途 controller と listener 2 台の予約として
200m CPU / 512 MiB が必要になる。既存ワークロードと合わせてクラスタ側の
ResourceQuota に収まることを確認してから有効にする。

runner は `toliworker-1/2/3` への node affinity と hostname による spread を持つ。
各 Pod は `longhorn-single` の generic ephemeral PVC を 40 GiB ずつ作成する。
workspace、Gradle cache、tool cache、`/nix`、`/tmp` はこの PVC の別ディレクトリを使い、
Pod 削除時に回収される。5 並列で PVC 5 本、最大 200 GiB の論理容量が必要となる。
データを再取得できる CI 用途なのでレプリカ数は 1 とし、ジョブ間の cache 共有はしない。

## クラスタ側の前提

以下は `turtton/infra` 側で管理する。

- ARC 0.15.1 の CRD。controller HelmRelease は install / upgrade とも CRD を `Skip` する。
- `lepinoid` の Helm 操作用 ServiceAccount `lepinoid-helm` と namespace 内の RBAC。
- runner 5 台と既存サービスを収容する ResourceQuota。
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
   続いて small 2 台 + medium 3 台を同時実行し、OOM / eviction / DiskPressure がなく、
   既存サービスの応答とノードのメモリ余力を保てることを確認する。
6. 新規ジョブの受け付けを止めるときは、
   両 scale set の `minRunners` / `maxRunners` を 0 にして反映し、未完了ジョブと runner Pod / PVC の回収を確認する。
   HelmRelease の suspend だけでは、インストール済みの ARC による runner 作成は停止しない。

GitHub Actions では `runs-on: lepi-ci-small` または `runs-on: lepi-ci-medium` で scale set を選ぶ。
controller の資格情報を runner に渡す設定は不要。

## 2026-11-04 の試験結果

同一ソース・空のcache・memory 8Gi・Gradle workers=2のruntime全チェックは、
2 CPUで33分52秒、4 CPUで28分55秒となり、両方で524 test suitesが成功した。
4 CPUは約15%短縮したため、mediumは4 CPU/8Gi、workers=2を採用する。
small 2 CPU/4Giでassets/world-importの2ジョブも成功し、最大working setは約3.2〜3.5Giだった。
small 2台・medium 2台の実ジョブ試験は全ジョブ成功し、157秒間の4並列を確認した。
runtimeの比較時working set最大は約5.8Gi、workers=4の並行試験では約6.5Giだった。

ARCの自動起動試験では、4台がすべて `lepinoid` に作成され、115秒の同時稼働を確認した。
非公開repo checkout、CPU/memory上限、DNS/公開HTTPS、同namespaceアプリ・API・LANの拒否、
loopback通信がすべて成功した。終了後はrunner Podと4つのPVCが自動回収され、元の8 PVCだけが残った。
待機時の実使用はcontroller約20Mi、listener各約8Miだった。

各試験の実行時間内ではNode Pressure、eviction、既存Podの再起動増加は検出されなかった。
配置先ノードの `MemAvailable` 最小は約7.9Giだった。これは現行負荷での結果であり、
既存Podのmemory limits合計がノード容量を超える設定は別途監視する。
ARC試験終了の約21分後（06:25:40 UTC）にlivesync-bridgeが同期処理の未実装メソッド例外で再起動した。
終了理由はError / exit code 1でOOMKilledではなく、CI負荷との関連を示す証拠は確認できなかった。

- 実ワークロード比較: https://github.com/Lepinoid/lepinoid/actions/runs/41774649754
- ARC自動起動・分離・回収: https://github.com/Lepinoid/lepinoid/actions/runs/41782904976

Skopeoは非root・capabilityなし・read-only rootfsの1 CPU/1Gi Podで、
両archiveの全layer copyとconfig一致を43秒で確認した。一時領域最大は約2.15Gi。
GHCRへの本番pushとclient harnessは今回の試験対象外。
