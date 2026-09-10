# Lepinoid Infra

[turtton/infra](https://github.com/turtton/infra) クラスタ上の `lepinoid` テナント用 Kubernetes マニフェストリポジトリ。

Flux CD がこのリポジトリを監視し、`lepinoid` namespace にリソースを自動デプロイする。

## Components

| コンポーネント | 概要 |
|---------------|------|
| **CouchDB** | Obsidian LiveSync バックエンド。ユーザー自動セットアップ用サイドカー付き |
| **LiveSync Bridge** | CouchDB と Git リポジトリ間の vault 同期ブリッジ |
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

## ワールド配布 (lepinoid-dev)

Lepinoid/Worlds リポジトリの `lepinoid/world-sync-config.yaml` で `enabled: true` になっているワールドを、週次（金曜 JST 17:00）および手動トリガーで OCI bundle (`ghcr.io/lepinoid/world-bundle:<version>`) としてビルドし、GitOps 経由で lepinoid-dev サーバーの `/data/<world>` に配布する。

bundle の digest は `lepinoid/world-versions.yaml` と `lepinoid/deployment.yaml` の annotation `lepinoid.dev/world-bundle-digest` で管理する。annotation の変更が検知されると Pod が rollout され、initContainer が新しい bundle を展開する。

### 配布対象ワールドの追加（必ずセットで実施）

1. `lepinoid/world-sync-config.yaml` の `config.json` に `{"name": "<world>", "enabled": true}` を追加して commit/push する
2. lepinoid 本体リポジトリの `WorldType.kt` に同名の `worldName` を追加する（追加しないとワールドファイルは配布されるがサーバーにロードされない）
3. `gh workflow run world-bundle.yml` を実行するか、次の週次実行を待つ

### ロールバック

```bash
gh workflow run update-world-versions.yml -f version=<旧version>
# または
gh workflow run update-world-versions.yml -f digest=sha256:<旧digest>
```

GHCR から canonical な値が解決され、`world-versions.yaml` と annotation が旧版に戻る。

### 注意: バージョン衝突 (collision)

Worlds リポジトリに新しい commit が無い状態で配布対象だけを変更しても、bundle の version が同一の場合は immutable tag 衝突でワークフローが失敗する。その場合は Worlds の次の AutoCommit を待ってから再実行すること。

### 失敗時の挙動

- **切替前の失敗**: 現行ワールドは変更されず、initContainer は exit 0 で起動を継続する
- **切替後の失敗**: 旧バージョンへの復元を best effort で試みる。最悪の場合は壊れたまま起動し、ERROR をログに出力する。復旧は digest を旧値に戻して rollout する運用でカバーする

詳細なセットアップ手順は [BOOTSTRAP.md](BOOTSTRAP.md) を参照。
