# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Kubernetes tenant workload repository for the `lepinoid` tenant, deployed via Flux CD onto the `turtton/infra` cluster. Uses Kustomize for resource composition and SOPS + Age for secret encryption.

## Development Environment

direnv + Nix flake で開発環境が自動構築される（`kubectl`, `sops`, `age` が利用可能）。

```bash
# Nix flakeのフォーマット
nix fmt

# Secret の暗号化（新規・変更時）
sops --encrypt --in-place <file>.sops.yaml

# Secret の復号（閲覧・編集時）
sops --decrypt <file>.sops.yaml
sops <file>.sops.yaml  # エディタで復号→編集→保存時に再暗号化

# マニフェスト検証
kustomize build .
```

SOPS の Age 秘密鍵は `SOPS_AGE_KEY_CMD` 経由で Bitwarden CLI (`rbw get lepinoid-infra-age-key`) から取得される。ローカルファイルとしては保持しない。

## Manifest Rules

- **`metadata.namespace` は設定しない** — Flux の `targetNamespace: lepinoid` で自動注入される
- **全コンテナに `resources.requests` / `resources.limits` を設定する** — ResourceQuota により未設定 Pod は拒否される
- **`nodeSelector` は設定しない**
- **クラスタスコープリソース（ClusterRole, CRD, PV 等）は作成不可**
- Secret ファイルは `*.sops.yaml` の命名規則に従う（`.sops.yaml` の `path_regex` に一致させる）

## Resource Quota

| リソース | 上限 |
|----------|------|
| requests.cpu | 1 |
| requests.memory | 2Gi |
| limits.cpu | 2 |
| limits.memory | 4Gi |
| PVC 数 | 5 |

## Architecture

```
kustomization.yaml          ← 全リソースのエントリポイント
├── couchdb/                ← Obsidian LiveSync バックエンド (couchdb:3)
│   ├── setup-users サイドカー: users.conf (username:password:database形式) から
│   │   ユーザー・DB を自動作成
│   └── initContainer: ConfigMap の local.ini を writable emptyDir にコピー
├── livesync-bridge/        ← Git 同期ブリッジ (ghcr.io/turtton/livesync-bridge)
│   └── commit.sh: vault 変更を検知し git commit/push (ローカル優先) / pull (リモート優先)
├── cloudflared/            ← Cloudflare Tunnel (token ベース, remotely-managed)
│   └── NetworkPolicy: base の default-deny に対する egress 許可
└── grafana/                ← スタンドアロン Grafana (grafana:11.5.2)
    └── Cloudflare Access auth proxy (Cf-Access-Authenticated-User-Email ヘッダー)
```

Flux CD がこのリポジトリを GitRepository として監視し、`lepinoid` namespace にリソースを reconcile する。
