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
| `terraform-check.yml` | Terraform・関連 workflow の変更 PR | Secret を使わない `tofu fmt` / `tofu validate` |
| `terraform-apply.yml` | `turtton` が `main` から手動実行 | plan の件数確認後、保存した plan を apply して暗号化 state を更新 |

OpenTofu の本番変更は PR をマージ後、Actions の **OpenTofu Apply** で `main` の
`mode=plan` を実行し、結果を確認してから `mode=apply` を実行する。
PR コメントの `/tf-apply` は使用しない。属性値を含む詳細 plan が必要な場合はローカルで確認する。

`static-checks.yml` はパスで絞らずに動くため、branch protection の必須チェックにできる。

### Static checks / Manifests (`ci/check-manifests.sh`)

1. `kustomize build .` が通り、出力と手動適用のマニフェスト（`kustomization.yaml` に含めないもの）が kubeconform の strict 検証に通る。対象の Kubernetes の版と、検証しない CRD（HelmRelease・HelmRepository・CiliumNetworkPolicy）は `ci/lib/manifest-checks.sh` で指定する
2. Pod を作る全コンテナ（initContainers を含む）に `requests` / `limits` の `cpu` と `memory` がある
3. `ghcr.io/lepinoid/plugin-updater` が `<tag>@sha256:<digest>` で固定され、digest で固定した同じ image が同じトップレベルディレクトリ内で同じ参照になっている
4. `*.sops.yaml` が SOPS で暗号化済み（age の受信者と MAC があり、`data` / `stringData` の値がすべて `ENC[...]`）で、それ以外のファイルに値を持つ Secret がない

### Static checks / GitHub Actions workflows

- [pinact](https://github.com/suzuki-shunsuke/pinact): `pinact run --check --verify` で、action が commit SHA で固定され、併記した版のコメントがその SHA と一致することを確認する。固定やコメントの付与は `pinact run` で行う
- [zizmor](https://github.com/zizmorcore/zizmor): workflow のセキュリティ監査（template injection、過剰な権限、認証情報の残留など）。抑制する場合は該当行に `# zizmor: ignore[<audit>]` と理由を書く

### ローカルでの実行

Nix の devShell に必要なツールが入っている。CI で使う版と sha256 は `ci/install-tools.sh` で固定しており、devShell の版とは異なることがある。

```bash
ci/check-manifests.sh                                    # Manifests
bash ci/lib/manifest-checks_test.sh                      # チェック自体のテスト
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
        image: ghcr.io/lepinoid/dsh-agent:0.2.2-rc.1-f434bcb@sha256:e80b6c1ac184849447198fdffe8b10c50a04e28074d4d6f809a20233c6bc67a2
        command: [sleep, "30"]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, readOnlyRootFilesystem: true}
        resources: {requests: {cpu: 50m, memory: 64Mi}, limits: {cpu: 100m, memory: 128Mi}}
  YAML
  kubectl -n lepinoid wait --for=condition=Ready pod/dsh-agent-pull-check --timeout=120s   # 成功すれば読める
  kubectl -n lepinoid describe pod dsh-agent-pull-check | sed -n '/Events:/,$p'              # 失敗時は unauthorized / denied
  kubectl -n lepinoid delete pod dsh-agent-pull-check
  ```

- `unauthorized`／`denied` のとき（鍵の保持者の作業）: `ghcr-pull-secret` の token にこのパッケージ（`ghcr.io/lepinoid/dsh-agent`）の読み取り権限がない。token の所有ユーザーに `read:packages` があること、パッケージ `lepinoid/dsh-agent` の Package settings で、そのユーザー（またはリポジトリ `Lepinoid/dsh-agent`）に Read が付いていることを確かめて調整する。token を差し替える場合は `lepinoid/ghcr-pull-secret.sops.yaml` を更新する（ほかの非公開イメージも引くため、権限を減らさない）。

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
