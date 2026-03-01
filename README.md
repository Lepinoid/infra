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

## Prerequisites

- [Nix](https://nixos.org/) + [direnv](https://direnv.net/)
- [rbw](https://github.com/doy/rbw) (Bitwarden CLI) — SOPS 秘密鍵の取得に使用

```bash
# ディレクトリに入ると direnv が自動で Nix dev shell を有効化
cd lepinoid-infra/
# → kubectl, sops, age が利用可能
```

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

詳細なセットアップ手順は [BOOTSTRAP.md](BOOTSTRAP.md) を参照。
