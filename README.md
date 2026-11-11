# Lepinoid Infra

[turtton/infra](https://github.com/turtton/infra) クラスタ上の `lepinoid` テナント用 Kubernetes マニフェストリポジトリ。

Flux CD がこのリポジトリを監視し、`lepinoid` namespace にリソースを自動デプロイする。

## Components

| コンポーネント | 概要 |
|---------------|------|
| **CouchDB** | Obsidian LiveSync バックエンド。ユーザー自動セットアップ用サイドカー付き |
| **[LiveSync Bridge](livesync-bridge/README.md)** | CouchDB と Git リポジトリ間の vault 同期ブリッジ。Obsidian からの Git 操作・復旧手順 |
| **Cloudflared** | Cloudflare Tunnel による外部アクセス（token ベース） |
| **Grafana** | Prometheus 接続のモニタリングダッシュボード。Cloudflare Access 認証 |
| **Dev server (lepinoid)** | Velocity + Minestom runtime pool の dev サーバーとワールド配布（下記） |
| **[dsh-agent](#dsh-agent-lepinoid-dev-agent)** | dev の調整作業を LLM エージェントと共同で行うための DeepSeek Harness（`dsh`）の専用 Pod。公開せず `kubectl port-forward` で使う（PoC） |
| **[neuralwatt-exporter](#neuralwatt-exporter)** | Neuralwatt（dsh-agent のモデル提供元）の残高・当月の使用量・キーの上限を Prometheus の指標にする exporter と、Grafana のダッシュボード「Neuralwatt」。scrape・アラートは turtton/infra 側 |
| **[Build server](build-server/README.md)** | PaperMC のビルドサーバー。LepinoidTools は [plugin-updater](updater/README.md) が更新する |

## Secret Management

Secret は [SOPS](https://github.com/getsops/sops) + [Age](https://github.com/FiloSottile/age) で暗号化管理する。

```bash
# 暗号化（新規作成・プレーンテキスト編集後）
sops --encrypt --in-place path/to/secret.sops.yaml

# 復号して編集（保存時に自動再暗号化）
sops path/to/secret.sops.yaml
```

`*.sops.yaml` ファイルの `data` / `stringData` フィールドのみが暗号化対象。

## Deploy

1. マニフェストを編集・commit・push
2. Flux CD が自動で reconcile し、`lepinoid` namespace にデプロイ

## CI

| Workflow | 実行タイミング | 内容 |
|----------|----------------|------|
| `static-checks.yml` | すべての PR と main への push | マニフェストと workflow の静的チェック（下記） |
| `shell-tests.yml` | `ci/**`・`lepinoid/*.sh` の変更 | `ci/lib/*_test.sh` と shell の構文チェック |
| `plugin-updater-tests.yml` | `updater/**` の変更 | plugin-updater の `go test` / `go vet` |
| `neuralwatt-exporter-tests.yml` | `exporters/neuralwatt/**`・ダッシュボードの変更 | neuralwatt-exporter の `gofmt` / `go test -race` / `go vet`、イメージのビルドと起動確認（push しない） |
| `neuralwatt-exporter-image.yml` | タグ `neuralwatt-exporter-*`・手動実行 | テストの後、`ghcr.io/lepinoid/neuralwatt-exporter:<yy.mm.dd>-<sha5>` を公開して digest を出す（[手順](#neuralwatt-exporter)） |
| `terraform-check.yml` | Terraform・関連 workflow の変更 PR | Secret を使わない `tofu fmt` / `tofu validate` |
| `terraform-apply.yml` | `turtton` が `main` から手動実行 | plan の件数確認後、保存した plan を apply して暗号化 state を更新 |

OpenTofu の本番変更は PR をマージ後、Actions の **OpenTofu Apply** で `main` の
`mode=plan` を実行し、結果を確認してから `mode=apply` を実行する。
PR コメントの `/tf-apply` は使用しない。属性値を含む詳細 plan が必要な場合はローカルで確認する。

`static-checks.yml` はパスで絞らずに動くため、branch protection の必須チェックにできる。

### Static checks / Manifests (`ci/check-manifests.sh`)

1. `kustomize build .` が通り、出力と手動適用のマニフェスト（`kustomization.yaml` に含めないもの）が kubeconform の strict 検証に通る。対象の Kubernetes の版と、検証しない CRD（HelmRelease・HelmRepository・CiliumNetworkPolicy）は `ci/lib/manifest-checks.sh` で指定する
2. Pod を作る全コンテナ（initContainers を含む）に `requests` / `limits` の `cpu` と `memory` がある
3. `ghcr.io/lepinoid/plugin-updater`・`dsh-agent`・`neuralwatt-exporter`（`MANIFEST_DIGEST_REQUIRED_IMAGES`）が `<tag>@sha256:<digest>` で固定され、digest で固定した同じ image が同じトップレベルディレクトリ内で同じ参照になっている
4. `*.sops.yaml` が SOPS で暗号化済み（age の受信者と MAC があり、`data` / `stringData` の値がすべて `ENC[...]`）で、それ以外のファイルに値を持つ Secret がない
5. プレースホルダ digest（0 を 64 個。イメージの公開前に雛形を置くときの印）が、`kustomization.yaml` から参照されるマニフェストに入っていない（`kustomization.yaml` に含めないマニフェストでは許す）

### Static checks / GitHub Actions workflows

- [pinact](https://github.com/suzuki-shunsuke/pinact): `pinact run --check --verify` で、action が commit SHA で固定され、併記した版のコメントがその SHA と一致することを確認する。固定やコメントの付与は `pinact run` で行う
- [zizmor](https://github.com/zizmorcore/zizmor): workflow のセキュリティ監査（template injection、過剰な権限、認証情報の残留など）。抑制する場合は該当行に `# zizmor: ignore[<audit>]` と理由を書く

### ローカルでの実行

Nix の devShell に必要なツールが入っている。CI で使う版と sha256 は `ci/install-tools.sh` で固定しており、devShell の版とは異なることがある。

```bash
ci/check-manifests.sh                                    # Manifests
bash ci/lib/manifest-checks_test.sh                      # チェック自体のテスト
ci/check-neuralwatt-exporter.sh                          # neuralwatt-exporter の gofmt・テスト・vet（docker が要る）
GITHUB_TOKEN=$(gh auth token) pinact run --check --verify
GH_TOKEN=$(gh auth token) zizmor .
```

kubeconform は Kubernetes のスキーマを GitHub から取得するため、ネットワークが必要。

## Dev server (lepinoid-dev)

`lepinoid` Service の Tailscale hostname `lepinoid-dev` を入口に、Online 認証の Velocity が Minestom runtime の pool へ振り分ける。runtime は 1 台以上、最大 3 台で、各台の定員は 8 人・描画距離は 8 chunk。この値は小人数向け dev 設定であり、24 人での性能保証を示すものではない。

`lepinoid/runtime-version.yaml` に採用した runtime image の版を記録し、`lepinoid/deployment.yaml` の本体・認証情報選択 initContainer と `lepinoid/velocity.yaml` の proxy image を同じ版に揃える。runtime 更新は StatefulSet の RollingUpdate で反映するため、更新対象の接続中プレイヤーは再接続が必要になる場合がある。自動増減の replicas は Velocity が管理する。

app の main で両 image の公開が完了すると `runtime-image-update` が届く。`update-lepinoid.yml` は公開済みの組を確認し、`world-bundle.yml` に渡す。world の準備が完了した後で、image・world・fingerprintを一つの commit で切り替える。手動更新は `update-lepinoid.yml` の `source_sha` に公開済み app commit の完全な SHA を指定する。

DB は `lepinoid-runtime-db` Service 経由の PostgreSQL 16 を使い、新規 PVC `lepinoid-runtime-db-data` に runtime が schema を作成する。Fabric の旧 DB を移行する手順はない。runtime 各台の看板・採取地点などの変更は `world-edits-lepinoid-runtime-pool-<ordinal>` PVC に保存され、Pod 再作成後も保持する。共通ワールドに反映したい編集は Worlds 側へ取り込んでから bundle を更新する。

転送・forwarding・制作 seed・各台の NATS 認証情報は `lepinoid/runtime-secrets.sops.yaml`、参加 UUID の認証は既存の `lepinoid/auth-secret.sops.yaml` と `Lepinoid/OperatorInfo` の `operators.yml` で管理する。制作 seed は DB と組で保持する。

runtime が内蔵する resource pack を `lepinoid-resourcepack:8082` の `/resourcepack.zip` で配信する。Cloudflare Tunnel の `pack-dev.lepinoid.net` をこの Service へ向け、公開 URL `https://pack-dev.lepinoid.net/resourcepack.zip?runtime=<runtime SHA7>` をクライアントへ案内する。Cloudflare の旧版キャッシュを避けるため、release 更新時は image・world と同じ commit で URL の runtime 版も切り替える。

### 設定 DB (lepinoid_config)

runtime の設定 revision ストア（weapon-tuning 画面の「確定」などが使う設定 DB）を dev で有効にしている。構成は次のとおり。

- database: 同じ `lepinoid-runtime-db` に、プレイヤー情報とは別の database `lepinoid_config` を置く。Deployment のサイドカー `create-config-db`（`postgres:16`）が、Pod 起動のたびに存在を確認して、なければ作る（冪等）。作成後は終了せず待機するので、再起動を繰り返さない。PVC は初期化済みで `initdb.d` が再実行されないため、サイドカーにしている。テーブルは runtime が dev プロファイルの起動時に作る（`Schema`・`spawn`／`weapon` の初期 revision。冪等で、複数 Pod の同時起動でも revision は増えない）。
- Secret: `lepinoid/spawn-config.sops.yaml`（Secret `lepinoid-spawn-config`、キー `spawnconfig.json`）。`enabled=true`・`environment=dev`・接続先 `//lepinoid-runtime-db:5432`・database `lepinoid_config`・ユーザーとパスワードを持つ。runtime コンテナに `/run/spawn-config/spawnconfig.json` として読み取り専用で mount し（`defaultMode: 0440`、uid `10001` が `fsGroup` で読む）、環境変数 `LEPINOID_SPAWN_CONFIG_PATH` で指す。runtime はこのファイルを起動時に 1 回だけ読むので、変更後は Pod の再作成が要る。
- 資格情報は二重管理になる: ユーザーとパスワードは `lepinoid/db-credentials.sops.yaml`（`POSTGRES_USER`／`POSTGRES_PASSWORD`）と同じ値を `spawnconfig.json` にも書く。パスワードを変えるときは両方の Secret を直す。専用ロールや読み取り専用ユーザーは作らない（dev では runtime の全 Pod が同じ資格情報を持つため。本番と共有するときに導入する）。
- 未配備のもの: `LEPINOID_SERVER_CONFIG_PATH`（`naturalSpawn` の allowlist）は設定しないので、自然湧きは無効のままで、採取の高品質率も設定されない。
- この設定を有効にするには、runtime image が Lepinoid/lepinoid#775（起動時のスキーマ作成と初期 revision 作成）を含む版であること。含まない版では、スキーマがないため weapon-tuning の確定が「基点を読み込めません」で止まる。

## dsh-agent (lepinoid-dev-agent)

DeepSeek Harness（[`dsh`](https://github.com/deepseek-ai/deepseek-harness) 0.2.2-rc.1、web profile）を常駐させる専用 Pod の PoC（[Lepinoid/infra#79](https://github.com/Lepinoid/infra/issues/79)）。イメージ `ghcr.io/lepinoid/dsh-agent`（非公開）は [Lepinoid/dsh-agent](https://github.com/Lepinoid/dsh-agent) が作る。

- 構成: StatefulSet `dsh-agent`（replicas 1）、PVC `data-dsh-agent-0`（`longhorn`、20Gi。`DSH_HOME=/data/dsh`、`HOME=/data/home`、サンドボックスの作業ルート `/data/work`）、公開しない headless Service。Pod のラベルは `app.kubernetes.io/name: lepinoid-dev-agent`（runtime の MCP（8083）の ingress が選ぶ名前）。
- サンドボックス: Landlock（restricted 相当の `securityContext` のまま成立する。bwrap は使えない）。
- モデル: Neuralwatt（`https://api.neuralwatt.com/v1`）の `deepseek-v4.1-flash`。ルートの設定はイメージのシードが持ち、API キーは Secret `dsh-agent-secrets`（キー `NEURALWATT_API_KEY`、`dsh-agent/secrets.sops.yaml`）から環境変数で受ける。このファイルの暗号化は**キーを持つ人が自分で行う**（実装者・エージェントは値を扱わない）。UI からキーを保存しない（`.credentials.yaml` の保存値が環境変数より優先されるため）。キーを替えたら `kubectl -n lepinoid rollout restart statefulset/dsh-agent`。
- MCP の token: Secret `lepinoid-dev-mcp` のキー `token` を環境変数 `LEPINOID_MCP_TOKEN` で受ける（`optional: true`。Secret がなくても起動する）。
- MCP の接続（[Lepinoid/infra#83](https://github.com/Lepinoid/infra/issues/83)）: dev 限定の MCP サーバー（runtime の `lepinoid-runtime-pool-0` の `http://lepinoid-runtime-pool-0.lepinoid-runtime-pool:8083/mcp`。Streamable HTTP）へ、dsh の `@deepseek-ai/dsh-mcp-client` の行（`id: mcp-lepinoid`、`serverName: lepinoid`。ツール名は `mcp__lepinoid__<tool>`）で接続する。
  - 行はイメージのシードが、プロファイル `web` の `cordis.patch.yml` に `- insert:` で持つ（[Lepinoid/dsh-agent#3](https://github.com/Lepinoid/dsh-agent/issues/3)）。**シードは新しい PVC の初回だけ**効くので、既存の PVC（`data-dsh-agent-0`）には、dsh-agent の [`AGENTS.md`](https://github.com/Lepinoid/dsh-agent/blob/main/AGENTS.md) の「既存の PVC へ MCP の接続を足す（一回限り）」の手順で、バックアップと sha256 の照合のうえ 1 回だけ追記する（`kubectl exec`、使えなければユーザー本人が手元で同じコマンドを実行する。同梱の web UI に MCP を登録する画面はない）。
  - `headers.Authorization` と `disabled` は `!!js` の式で、Pod の環境変数 `LEPINOID_MCP_TOKEN` を読む（token の値はファイルにもログにも出ない）。`disabled: !!js '!process.env.LEPINOID_MCP_TOKEN'` により、token が未設定のときは行そのものが載らず、MCP へ通信しない（`Bearer undefined` を送らない）。
  - 接続に失敗しても dsh のログには何も出ない。失敗は runtime の監査行（`LEPINOID_MCP call tool=… outcome=…`）、ツールが見えないこと、401 で判定する。dsh は `MCP-Protocol-Version: 2026-07-28` のプローブを先に送り、runtime が 400 で断ると、旧版の `initialize`・`tools/list` に落ちる。
  - **token の回転**: `!!js` は dsh の起動時に 1 回だけ評価されるので、Secret `lepinoid-dev-mcp` を更新したら、runtime（`lepinoid-runtime-pool-0`）と dsh（`kubectl -n lepinoid rollout restart statefulset/dsh-agent`）の両方の Pod を再起動する。
  - `@deepseek-ai/dsh-mcp-manager`（`mcp-manager` の行、`${credential:…}` のテンプレート）は dsh の master にだけあり、公開版 0.2.2-rc.1 にはないので使わない（dsh の次のリリースに含まれたときに移る別の変更）。
- ネットワーク: 外向きは DNS、私設アドレス帯を除く公開の TCP 443、runtime の 8083 だけ。同 namespace などへの egress は base が許可しているので、`dsh-agent/network-policy.yaml` の `CiliumNetworkPolicy`（`egressDeny`）で明示的に拒否する。

### 使い方

dsh は `127.0.0.1:3080` だけで待ち受ける（ワイルドカードは dsh が拒否する）。`kubectl port-forward` は Pod の loopback へ届く。

```sh
kubectl -n lepinoid logs dsh-agent-0 | grep -i token                      # 起動ログの launch token の URL（値を共有・記録しない）
kubectl -n lepinoid port-forward pod/dsh-agent-0 3080:3080
```

URL のホストを `127.0.0.1:3080` にしてブラウザで開く。認証は launch token と、`$DSH_HOME/.credentials.yaml` の署名鍵で署名した cookie（Pod を作り直しても残る。cookie はホストとポートに結びつくので、同じローカルポートを使う）。

### イメージの更新

1. [Lepinoid/dsh-agent](https://github.com/Lepinoid/dsh-agent) の CI が公開した `<dsh の版>-<コミット SHA7>` のタグと digest を確かめる。
2. `dsh-agent/statefulset.yaml` の `image` を `ghcr.io/lepinoid/dsh-agent:<tag>@sha256:<digest>` に書き換える PR を出す（tag だけの参照は `ci/check-manifests.sh` が拒否する）。
3. マージ後、StatefulSet が RollingUpdate で Pod を作り直す。PVC は残り、セッションと cookie の署名鍵も保持される。

### `ghcr-pull-secret` がパッケージを読めるか

イメージは非公開で、Pod は `ghcr-pull-secret`（`kubernetes.io/dockerconfigjson`、`ghcr.io` の資格情報）で引く。Secret の値は表示しない。

- 適用後（結果の確認）: `kubectl -n lepinoid get pod dsh-agent-0` が `ImagePullBackOff`／`ErrImagePull` でなく、`kubectl -n lepinoid describe pod dsh-agent-0` の Events に `unauthorized`／`denied`／`forbidden` がないこと。`Successfully pulled image` が出ていれば読めている。
- 適用前（先に確かめる。クラスタへの書き込みを伴うので、鍵の保持者が行う）: 使い捨ての Pod で同じ参照を引く。確認後に削除する。

  ```sh
  kubectl -n lepinoid apply -f - <<'YAML'
  apiVersion: v1
  kind: Pod
  metadata:
    name: dsh-agent-pull-check
  spec:
    restartPolicy: Never
    automountServiceAccountToken: false
    imagePullSecrets: [{name: ghcr-pull-secret}]
    securityContext: {runAsNonRoot: true, runAsUser: 10001, seccompProfile: {type: RuntimeDefault}}
    containers:
      - name: c
        image: ghcr.io/lepinoid/dsh-agent:0.2.2-rc.1-61d9e39@sha256:5f43c99b1aaba60fe745c920a80f5970027e26b96d214d4d2555db89ede0d18e
        imagePullPolicy: Always   # ノードにキャッシュ済みのイメージで成功して、権限の不足を見逃さない
        command: [sleep, "30"]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, readOnlyRootFilesystem: true}
        resources: {requests: {cpu: 50m, memory: 64Mi}, limits: {cpu: 100m, memory: 128Mi}}
  YAML
  kubectl -n lepinoid wait --for=condition=Ready pod/dsh-agent-pull-check --timeout=120s   # 成功すれば読める
  kubectl -n lepinoid describe pod dsh-agent-pull-check | sed -n '/Events:/,$p'              # 失敗時は unauthorized / denied
  kubectl -n lepinoid delete pod dsh-agent-pull-check
  ```

- `unauthorized`／`denied` のとき（鍵の保持者の作業）: 原因は次の 3 つに分かれる。どれが足りないかを切り分けて調整する（実装者は token の値を使った確認をしていない）。
  1. token の scope: `ghcr-pull-secret` の token が `read:packages` を持つこと（classic PAT。fine-grained PAT は Packages に対応しない）。
  2. token 所有ユーザーの package 権限: パッケージ `lepinoid/dsh-agent`（Package settings）で、その token の所有ユーザー（またはそのユーザーを含む Team）に Read が付いていること。
  3. Actions のリポジトリアクセス: Package settings の「Manage Actions access」は、**GitHub Actions のワークフロー（`GITHUB_TOKEN`）がパッケージを使う**ための設定で、`ghcr-pull-secret` の token（PAT）の権限とは別。クラスタからの pull には効かないので、ここを直しても pull は通らない。公開側の workflow が push できているかとは分けて考える。

  token を差し替える場合は `lepinoid/ghcr-pull-secret.sops.yaml` を更新する（ほかの非公開イメージも引くため、権限を減らさない）。

## neuralwatt-exporter

dsh-agent が使う Neuralwatt の**残高・当月の使用量・キーの上限**を Prometheus の指標にし、Grafana のダッシュボード「Neuralwatt」（`grafana.lepinoid.net`、uid `neuralwatt`）で見られるようにする（[Lepinoid/infra#84](https://github.com/Lepinoid/infra/issues/84)）。月 20 ドル程度の予算（段階 2 の停止線: 累計 10 ドルで見直し、16 ドルで任意項目を止める）を、Neuralwatt のポータルを開かずに確認するためのもの。

- コード: [`exporters/neuralwatt/`](exporters/neuralwatt)（Go。依存は `prometheus/client_golang`）。`GET https://api.neuralwatt.com/v1/quota` をバックグラウンドで 1 本だけ、`NEURALWATT_POLL_INTERVAL`（既定 60 秒、下限 10 秒。この配備では `neuralwatt-exporter/deployment.yaml` で **15 分**）ごとに呼び、最後に成功した値を `/metrics` で返す（scrape では API を呼ばない）。**取得に失敗したときは値を 0 にせず**、直前の成功の値を残し、`neuralwatt_exporter_fetch_success 0` と `neuralwatt_exporter_fetches_total{result=…}` で失敗を表す。一度も成功していなければデータ系列は出ない。値が `null` の系列（サブスクリプションなし、キーの上限なしなど）も出さない。
- 配備: `neuralwatt-exporter/deployment.yaml`（1 レプリカ。Service なし）。Pod のラベル `app.kubernetes.io/name: neuralwatt-exporter`、コンテナポート 8080（名前 `metrics`）、パス `/metrics`。`neuralwatt-exporter/network-policy.yaml` で外向きを DNS と公開の TCP 443 だけにする（形は dsh-agent と同じ。ingress は変更せず、base の許可で Prometheus が scrape できる）。
- API キー: dsh-agent と**同じ** Secret `dsh-agent-secrets` のキー `NEURALWATT_API_KEY` だけを、読み取り専用のファイル（`/var/run/neuralwatt/api-key`）として投影する。実装者・エージェントはキーの値を扱わない。ログ・`/metrics`・エラー文にキーは出ない（テストで検査している）。
- scrape とアラート（PodMonitor・PrometheusRule）は、`lepinoid-deployer` に `monitoring.coreos.com` の権限がないため [turtton/infra#195](https://github.com/turtton/infra/issues/195) で扱う。それが着地するまで、ダッシュボードは Prometheus にデータがなく空になる。
- `daily free energy`（無料枠の当日の残量）は公開 API がなく、ダッシュボードには出ない（ポータルで見る）。

### 指標

| 指標 | 型 | ラベル |
|---|---|---|
| `neuralwatt_exporter_fetch_success` | gauge（1=直近の取得が成功、0=失敗。最初の取得が終わるまで出ない） | — |
| `neuralwatt_exporter_last_success_timestamp_seconds` | gauge（未成功は 0） | — |
| `neuralwatt_exporter_fetch_duration_seconds` | gauge | — |
| `neuralwatt_exporter_fetches_total` | counter（0 で初期化） | `result`＝`ok`・`unauthorized`・`payment_required`・`rate_limited`・`server_error`・`client_error`・`network_error`・`invalid_response`・`credential_error` |
| `neuralwatt_quota_snapshot_timestamp_seconds` | gauge | — |
| `neuralwatt_credits_remaining_usd`・`neuralwatt_credits_total_usd` | gauge | — |
| `neuralwatt_account_info` | gauge（常に 1） | `accounting_method`・`rate_limit_tier` |
| `neuralwatt_usage_cost_usd_total`・`_requests_total`・`_tokens_total`・`_energy_kwh_total` | counter（`current_month` は暦月の境界で 0 に戻る） | `window`＝`current_month`・`lifetime` |
| `neuralwatt_limits_overage_limit_usd` | gauge（`null` なら出ない） | — |
| `neuralwatt_subscription_info` | gauge（常に 1。サブスクリプションなしなら出ない） | `plan`・`status`・`billing_interval` |
| `neuralwatt_subscription_kwh_included`・`_kwh_used`・`_kwh_remaining`・`neuralwatt_subscription_in_overage`・`neuralwatt_subscription_period_end_timestamp_seconds` | gauge | — |
| `neuralwatt_key_allowance_limit_usd`・`_spent_usd`・`_remaining_usd`・`neuralwatt_key_allowance_blocked`・`neuralwatt_key_allowance_reset_timestamp_seconds` | gauge（キーに上限がなければ出ない） | `key_name`・`period` |
| `neuralwatt_monthly_budget_usd` | gauge（`NEURALWATT_MONTHLY_BUDGET_USD` から。未設定なら出ない） | — |
| `process_start_time_seconds` | gauge（標準の process collector。起動時刻） | — |

`result` の分類: `401`＝`unauthorized`、`402`＝`payment_required`、`429`＝`rate_limited`（`Retry-After` の間は再取得しない。指定された時間は短縮しない。なければ指数バックオフ＋ジッター、上限 10 分）、`5xx`＝`server_error`、その他の `4xx`＝`client_error`、接続失敗・タイムアウト（10 秒）＝`network_error`、不正な JSON・`balance.credits_remaining_usd` の欠落・1 MiB 超・リダイレクト（追従しない）・想定外の 2xx・ラベルになる文字列（`key.name`・`period`・`plan` など）に送信したキーが入っている応答（`/metrics` への漏洩を防ぐため棄却）＝`invalid_response`、キーのファイルが読めない・空・ヘッダに使えない文字＝`credential_error`。

### 確認方法

Service がないので、Pod への proxy で `/metrics` を見る。

```sh
kubectl -n lepinoid get deploy,pods -l app.kubernetes.io/name=neuralwatt-exporter
kubectl -n lepinoid logs deploy/neuralwatt-exporter            # 状態の変化（成功↔失敗）と結果の分類だけが出る。キーは出ない
pod=$(kubectl -n lepinoid get pod -l app.kubernetes.io/name=neuralwatt-exporter -o name | head -1 | cut -d/ -f2)
kubectl get --raw /api/v1/namespaces/lepinoid/pods/$pod:8080/proxy/metrics | grep -E '^neuralwatt_(exporter_fetch_success|credits_remaining_usd|usage_cost_usd_total)'
```

値はポータルの表示と突き合わせる。`/healthz` はプロセスが生きていれば 200（API の失敗では落とさない）。API の失敗は `neuralwatt_exporter_fetch_success` で見る（`up` は exporter に届くかどうかだけ）。

### 公開と版の更新の手順

`neuralwatt-exporter/deployment.yaml` の `image` は `<tag>@sha256:<digest>` で固定する（tag だけの参照は `ci/check-manifests.sh` が拒否する）。

1. コードを変えた PR をマージする（`exporters/neuralwatt/**` の変更は `neuralwatt-exporter-tests.yml` が検査する）。
2. `main` のコミットにタグ `neuralwatt-exporter-<任意の版>` を付けて push するか、Actions の **Publish Neuralwatt exporter** を `main` から手動実行する（`workflow_dispatch`）。テストとイメージのビルド・起動確認の後、`ghcr.io/lepinoid/neuralwatt-exporter:<yy.mm.dd>-<コミット SHA5>` を push し、実行ログに `Published: <ref>@<digest>` を出す。
3. 別の PR で、`deployment.yaml` の `image` をその `<tag>@sha256:<digest>` に書き換える（publish の実行 URL を PR に書く）。
4. マージ後、`kubectl -n lepinoid get deploy,pods -l app.kubernetes.io/name=neuralwatt-exporter` で 1/1 Ready・再起動 0 を確かめ、上の「確認方法」で `neuralwatt_exporter_fetch_success 1` を確認する。

初回の公開（2026-11-10）: `ghcr.io/lepinoid/neuralwatt-exporter:26.11.10-39702@sha256:85683c794be3d0d0d329cdf79029bc65bf52a22522a95c4fff4671a9df449558`（[run 42830419610](https://github.com/Lepinoid/infra/actions/runs/42830419610)）。**新しい GHCR パッケージの初回 publish は `requested access to the resource is denied` になることがある**（パッケージとリポジトリの関連付け・可視性は人の作業）。パッケージは作成直後は private で、リポジトリ `Lepinoid/infra` に関連付けられている。Pod が `ImagePullBackOff` で `unauthorized`／`denied` になるときは、[`ghcr-pull-secret` がパッケージを読めるか](#ghcr-pull-secret-がパッケージを読めるか)の切り分けに従い、`ghcr-pull-secret` の token の所有ユーザーにパッケージ `lepinoid/neuralwatt-exporter` の Read を付けるか、パッケージを公開にする（イメージに秘密はなく、リポジトリも公開）。

### 取得間隔（15 分）の影響

この配備は `NEURALWATT_POLL_INTERVAL=15m` で、Prometheus の scrape（60 秒）とは独立に、15 分に 1 回だけ `/v1/quota` を取得する。値（残高・費用）は 15 分ごとにしか更新されない（`neuralwatt_quota_snapshot_timestamp_seconds` と `neuralwatt_exporter_last_success_timestamp_seconds` が、最後の成功の時刻を示す）。

- **失敗の検知が遅れる**: 取得が失敗すると、`neuralwatt_exporter_fetch_success` は次の取得（15 分後）まで 0 のまま。1 回の失敗では約 15 分、2 回連続で約 30 分 0 のままになる。**`NeuralwattQuotaFetchFailing` は `for: 40m`**（turtton/infra#199）で、1 回・2 回の失敗では発火せず、**3 回連続の失敗（約 45 分。最小で約 42 分 50 秒）で発火する**。障害が起きてから発火するまでは、障害のあとの最初の取得までの待ち（最大 15 分）を含めて**およそ 40〜55 分に、scrape・評価の遅延を足した時間**。
  - 旧設定の説明（`for: 15m` のとき）: 一度だけの一時的な失敗（`network_error` は 22 時間に 4 回あった）でも、`fetch_success` が 15 分間 0 のままなので `for: 15m` に届いて発火しえた。#199 で `for` を取得間隔より長くして、単発の失敗では発火しないようにした。
- **費用の急増の検知が遅れる**: `NeuralwattCostBurnRateHigh`（1 時間に 2 ドル超）は、費用の値が 15 分ごとの階段状にしか更新されないので、実際の急増から最大で 15 分遅れて検知する。アラートの式は `max_over_time(increase(…[1h])[15m:1m]) > 2`（1 時間の増加の**直近 15 分間の最大**。1 時間の窓に入る更新は通常 4 回だが、15 分に約 1 分だけ 3 回になって増加が 3/4 に落ちるため、その谷で解消・再発火を繰り返さないようにしている）。**ダッシュボードの「1 時間あたりの費用」は生の `increase(…[1h])` なので、アラートの判定値と一時的に一致しないことがある**（アラートは直近 15 分の最大を使う）。**アラートの解消も最大で約 15 分遅れる**。残高・予算の消化率・キーの上限のアラートも同様に、検知が最大 15 分遅れる。
- **`402`（`payment_required`）の検知**: `NeuralwattKeyBlocked` の窓は `increase(neuralwatt_exporter_fetches_total{result="payment_required"}[35m]) > 0`（取得間隔の 2 倍 + 5 分。turtton/infra#199）。`402` は取得（15 分）のたびに 1 ずつ増えるので、`402` が続く間は途切れず発火し、間の取得が 1 回別のエラーで抜けても解消しない。
  - 旧設定の説明（窓が `[10m]` のとき）: 窓が取得間隔（15 分）より短く、`402` が続く間でも、15 分ごとに「10 分発火 → 5 分解消」を繰り返しえた。#199 で窓を取得間隔より長くした。
- **`429` のバックオフ**: `Retry-After` があればそれに従う（15 分より短ければ 15 分、長ければその時間）。なければ指数バックオフ（上限 10 分）だが、**取得間隔（15 分）を下回らない**ので、実質 15 分ごと（間隔を延ばさない）。1 秒 1 リクエストの制限には、15 分間隔では触れない。
- **影響しないもの**: `NeuralwattQuotaStale`（最後の成功から 6 時間）は、15 分間隔でも 6 時間より十分短いので、影響しない（一度も成功していない場合の起動時刻からの 6 時間の猶予も同じ）。`NeuralwattExporterDown`・`NeuralwattExporterAbsent` は scrape の `up` なので、取得間隔に依存しない。
- **ダッシュボード**: 「最後の成功からの経過」は正常でも 0〜15 分になるので、色の閾値を橙 30 分・赤 60 分にした。
- **キーの回転**は再起動不要のまま。ただし、新しいキーでの取得は次の取得（最大 15 分後）になる。すぐに確かめたいときは Pod を再起動する（起動時に 1 回取得する）。

### キーの回転・`402` が出たとき

- **キーの回転は exporter の再起動が要らない。** exporter は取得のたびにキーのファイルを読み直す。`dsh-agent/secrets.sops.yaml` を更新して Flux が適用すると、kubelet が Secret のマウントを更新する（反映まで数分かかることがある）。dsh-agent 側は環境変数のスナップショットなので、従来どおり `kubectl -n lepinoid rollout restart statefulset/dsh-agent` が要る。キーが無効になった間は `neuralwatt_exporter_fetches_total{result="unauthorized"}` が増え、`fetch_success` が 0 になる。
- **`402`（`payment_required`）**: Neuralwatt はキーが上限（ポータルで設定するキーごとの allowance）に達すると、そのキーのリクエストを `402` で止める。dsh-agent と**同じキー**を使うので、**`/v1/quota` も `402` で取れなくなる可能性がある**。このとき exporter は `fetch_success 0`・`result="payment_required"` で**検知するだけ**で、直前の成功の値（残高・費用）を `/metrics` に残す（古さは `neuralwatt_exporter_last_success_timestamp_seconds` で分かる）。復旧は、ポータルでキーの上限を引き上げるか、上限の期間がリセットされる（`neuralwatt_key_allowance_reset_timestamp_seconds`）のを待つ。別キーへの切り替えはこの作業の範囲外（`402` が実測で出たら別の issue で扱う）。
- `credential_error` が続くとき: キーのファイルが読めない・空。`kubectl -n lepinoid describe pod -l app.kubernetes.io/name=neuralwatt-exporter` で Secret のマウントのイベントを見る。

### テスト・CI

- `ci/check-neuralwatt-exporter.sh`: docker の `golang` で `gofmt -l`・`go test -race -shuffle=on -count=1 ./...`・`go vet ./...`（`neuralwatt-exporter-tests.yml`）。テストは `httptest` の偽サーバーで、公式ドキュメントの `/v1/quota` の例と `null` の組み合わせ（`testdata/`）を固定し、`/metrics` の出力を golden と比べ、`testutil` の lint を通す。Neuralwatt の API は呼ばない。
- ダッシュボード（`grafana/dashboard-neuralwatt.yaml`）の全 `expr` の `neuralwatt_*` の名前が、exporter の指標の名前（`testdata/metrics.golden`）に含まれることも、同じテストが検査する。
- publish workflow は `neuralwatt-exporter-image.yml`（タグ `neuralwatt-exporter-*` または `workflow_dispatch`）。テストの後、イメージをビルドして起動確認（`ci/smoke-neuralwatt-exporter-image.sh`）をしてから push する。

## ワールド配布 (lepinoid-dev)

Lepinoid/Worlds の `lepinoid/world-sync-config.yaml` で有効にした terrestrial と lv1 を、Minecraft 1.21.8 用の runtime importer で変換して OCI bundle (`ghcr.io/lepinoid/runtime-world-bundle:<version>-<runtime SHA7>`) として配布する。元の建築用ワールドは変換元として扱う。

bundle の digest と pool fingerprint は `lepinoid/world-versions.yaml` に記録する。`lepinoid/deployment.yaml` の annotation `lepinoid.dev/world-bundle-digest` の変更で runtime Pod が rollout され、world-sync initContainer が `/worlds/<world>` へ展開する。Velocity は同じ fingerprint の runtime に接続させるため、ワールド更新時は proxy も再作成される。

同じ Worlds／runtime 版の bundle が既にある場合は検証して再利用する。import は Anvil 内部の保存時刻も更新するため、公開済みタグへ変換し直したバイト列を上書きしない。

### ワークフローの認証設定

`world-bundle.yml` が runtime importer のソースを取得できるよう、`Lepinoid/lepinoid` のみにアクセスできる読み取り専用の deploy key を作成し、公開鍵を同リポジトリの Deploy keys に書き込み権限なしで登録する。秘密鍵はこの infra リポジトリの Actions secret `LEPINOID_SOURCE_DEPLOY_KEY` に登録する。`Lepinoid/Worlds` の取得には、引き続き同リポジトリの `contents:read` 権限を持つ Actions secret `PAT_TOKEN` を使用する。

### 配布対象ワールドの追加

1. `lepinoid/world-sync-config.yaml` の `config.json` を変更する。
2. runtime に対応ワールドの読み込み設定・機能があることを確認する。
3. `world-bundle.yml` を実行し、新しい digest と fingerprint を配備する。

### ロールバック

`update-world-versions.yml` で過去の runtime bundle を指定するか、Git で以前の digest・fingerprint を揃えて戻す。制作 seed と DB、各台の world-edits PVC は保持する。ワールド構造の異なる版への復元は編集内容との整合性を確認して行う。

### 失敗時の挙動

runtime 用の bundle 検証・展開に失敗した場合は initContainer が失敗し、ゲーム本体を起動しない。digest と import 済みワールドを修正して再配備する。

詳細なセットアップ手順は [BOOTSTRAP.md](BOOTSTRAP.md) を参照。
