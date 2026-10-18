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

## Dev server (lepinoid-dev)

`lepinoid` Service の Tailscale hostname `lepinoid-dev` を入口に、Online 認証の Velocity が Minestom runtime の pool へ振り分ける。runtime は 1 台以上、最大 3 台で、各台の定員は 8 人・描画距離は 8 chunk。この値は小人数向け dev 設定であり、24 人での性能保証を示すものではない。

`lepinoid/runtime-version.yaml` に採用した runtime image の版を記録し、`lepinoid/deployment.yaml` の本体・認証情報選択 initContainer と `lepinoid/velocity.yaml` の proxy image を同じ版に揃える。runtime 更新は StatefulSet の RollingUpdate で反映するため、更新対象の接続中プレイヤーは再接続が必要になる場合がある。自動増減の replicas は Velocity が管理する。

app の main で両 image の公開が完了すると `runtime-image-update` が届く。`update-lepinoid.yml` は公開済みの組を確認し、`world-bundle.yml` に渡す。world の準備が完了した後で、image・world・fingerprintを一つの commit で切り替える。手動更新は `update-lepinoid.yml` の `source_sha` に公開済み app commit の完全な SHA を指定する。

DB は `lepinoid-runtime-db` Service 経由の PostgreSQL 16 を使い、新規 PVC `lepinoid-runtime-db-data` に runtime が schema を作成する。Fabric の旧 DB を移行する手順はない。runtime 各台の看板・採取地点などの変更は `world-edits-lepinoid-runtime-pool-<ordinal>` PVC に保存され、Pod 再作成後も保持する。共通ワールドに反映したい編集は Worlds 側へ取り込んでから bundle を更新する。

転送・forwarding・制作 seed・各台の NATS 認証情報は `lepinoid/runtime-secrets.sops.yaml`、参加 UUID の認証は既存の `lepinoid/auth-secret.sops.yaml` と `Lepinoid/OperatorInfo` の `operators.yml` で管理する。制作 seed は DB と組で保持する。

runtime が内蔵する resource pack を `lepinoid-resourcepack:8082` の `/resourcepack.zip` で配信する。Cloudflare Tunnel の `pack-dev.lepinoid.net` をこの Service へ向け、公開 URL `https://pack-dev.lepinoid.net/resourcepack.zip` をクライアントへ案内する。

## ワールド配布 (lepinoid-dev)

Lepinoid/Worlds の `lepinoid/world-sync-config.yaml` で有効にした terrestrial と lv1 を、Minecraft 1.21.8 用の runtime importer で変換して OCI bundle (`ghcr.io/lepinoid/runtime-world-bundle:<version>-<runtime SHA7>`) として配布する。元の建築用ワールドは変換元として扱う。

bundle の digest と pool fingerprint は `lepinoid/world-versions.yaml` に記録する。`lepinoid/deployment.yaml` の annotation `lepinoid.dev/world-bundle-digest` の変更で runtime Pod が rollout され、world-sync initContainer が `/worlds/<world>` へ展開する。Velocity は同じ fingerprint の runtime に接続させるため、ワールド更新時は proxy も再作成される。

同じ Worlds／runtime 版の bundle が既にある場合は検証して再利用する。import は Anvil 内部の保存時刻も更新するため、公開済みタグへ変換し直したバイト列を上書きしない。

### 配布対象ワールドの追加

1. `lepinoid/world-sync-config.yaml` の `config.json` を変更する。
2. runtime に対応ワールドの読み込み設定・機能があることを確認する。
3. `world-bundle.yml` を実行し、新しい digest と fingerprint を配備する。

### ロールバック

`update-world-versions.yml` で過去の runtime bundle を指定するか、Git で以前の digest・fingerprint を揃えて戻す。制作 seed と DB、各台の world-edits PVC は保持する。ワールド構造の異なる版への復元は編集内容との整合性を確認して行う。

### 失敗時の挙動

runtime 用の bundle 検証・展開に失敗した場合は initContainer が失敗し、ゲーム本体を起動しない。digest と import 済みワールドを修正して再配備する。

詳細なセットアップ手順は [BOOTSTRAP.md](BOOTSTRAP.md) を参照。
