# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Kubernetes tenant workload repository for the `lepinoid` tenant, deployed via Flux CD onto the `turtton/infra` cluster. Uses Kustomize for resource composition and SOPS + Age for secret encryption.

## Development Environment

direnv + Nix flake で開発環境が自動構築される（`kubectl`, `sops`, `age`, `opentofu` と、静的チェック用の `kustomize`, `kubeconform`, `yq`, `pinact`, `zizmor` が利用可能）。

```bash
# Nix flakeのフォーマット
nix fmt

# Secret の暗号化（新規・変更時）
sops --encrypt --in-place <file>.sops.yaml

# Secret の復号（閲覧・編集時）
sops --decrypt <file>.sops.yaml
sops <file>.sops.yaml  # エディタで復号→編集→保存時に再暗号化

# マニフェスト検証（CI の Static checks / Manifests と同じ）
ci/check-manifests.sh

# workflow の検証（CI の Static checks / GitHub Actions workflows と同じ）
GITHUB_TOKEN=$(gh auth token) pinact run --check --verify
GH_TOKEN=$(gh auth token) zizmor .
```

SOPS の Age 秘密鍵は `SOPS_AGE_KEY_CMD` 経由で Bitwarden CLI (`rbw get lepinoid-infra-age-key`) から取得される。ローカルファイルとしては保持しない。

## Manifest Rules

- **`metadata.namespace` は設定しない** — Flux の `targetNamespace: lepinoid` で自動注入される
- **全コンテナ（initContainers を含む）に `resources.requests` / `resources.limits` を設定する（`cpu` と `memory` の両方必須）** — ResourceQuota により `requests.cpu`, `requests.memory`, `limits.cpu`, `limits.memory` のいずれかが欠けた Pod は作成が拒否される（CI で検査）
- **`nodeSelector` は原則設定しない** — 例外は `build-server/`。`longhorn-gameserver`（strict-local）のボリュームを使うため、Deployment・CronJob・rescue Pod を `mainworker-1` に固定している（[build-server/README.md](build-server/README.md)）
- **クラスタスコープリソース（ClusterRole, CRD, PV 等）は作成不可**
- Secret ファイルは `*.sops.yaml` の命名規則に従う（`.sops.yaml` の `path_regex` に一致させる）。暗号化済みであること、それ以外のファイルに値を持つ Secret を置かないことを CI で検査する
- `ghcr.io/lepinoid/plugin-updater` は `<tag>@sha256:<digest>` で固定する。digest で固定した同じ image は、同じトップレベルディレクトリ内ですべて同じ参照にそろえる（CI で検査）
- `ghcr.io/lepinoid/dsh-agent` と `ghcr.io/lepinoid/neuralwatt-exporter` も `<tag>@sha256:<digest>` で固定する（CI で検査。`MANIFEST_DIGEST_REQUIRED_IMAGES`）。版は publish workflow（`neuralwatt-exporter-image.yml`）の実行ログの `Published: <ref>@<digest>` を `neuralwatt-exporter/deployment.yaml` に貼る別 PR で上げる（README の「neuralwatt-exporter」節）。公開前の雛形に置くプレースホルダ digest（0 を 64 個）は、`kustomization.yaml` から参照されるマニフェストに入れない（CI で検査）
- `kustomization.yaml` に含めない手動適用のマニフェスト（例: `build-server/rescue-pod.yaml`）も、同じ CI 検査の対象になる

## GitHub Actions Rules

- `uses:` は commit SHA で固定し、版をコメントで併記する（`pinact run` で付与。CI は `pinact run --check --verify` で SHA とコメントの一致まで検査）
- workflow 先頭は `permissions: {}` とし、必要な権限は job ごとに最小限で付与する
- `actions/checkout` は `persist-credentials: false` にする。push が必要なら、その step だけ token を `http.https://github.com/.extraheader` で渡す
- `run:` や `script:` に `${{ ... }}` を直接埋め込まず、`env:` 経由で渡す（zizmor の template-injection 対策）
- zizmor の指摘を抑制するときは、該当行に `# zizmor: ignore[<audit>]` を付け、その理由をコメントで残す
- CI で使うツールの版と sha256 は `ci/install-tools.sh` だけで管理する（workflow ごとにダウンロード処理を重複させない）

## Resource Quota

クラスタ側（turtton/infra）で管理される。ARC導入用のクラスタ変更で以下へ拡張する（適用状態は `kubectl -n lepinoid get resourcequota` で確認できる）:

| リソース | 上限 |
|----------|------|
| requests.cpu | 25 |
| requests.memory | 50Gi |
| limits.cpu | 34 |
| limits.memory | 66Gi |
| PVC 数 | 24 |

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
├── grafana/                ← スタンドアロン Grafana (grafana:11.5.2)
│   └── Cloudflare Access auth proxy (Cf-Access-Authenticated-User-Email ヘッダー)
├── lepinoid/               ← dev server (Velocity + Minestom runtime pool, PostgreSQL, NATS, ワールド配布)
│   └── dev 限定の MCP (runtime のポート 8083): token は専用 Secret `lepinoid-dev-mcp`、到達元は CiliumNetworkPolicy の ingressDeny でエージェント Pod (`lepinoid-dev-agent`) 以外を拒否（標準 NetworkPolicy の許可は加算されるため）
│   └── エージェント bridge の口 (同じ 8083 の `/agent/*`): token は Secret `lepinoid-agent-bridge` (`LEPINOID_AGENT_BRIDGE_TOKEN`)、許可する人は `Lepinoid/OperatorInfo` の `operators.yml` が正本で、runtime が既存の operator 認可（ロール）で判定する（許可するロールは環境変数 `LEPINOID_AGENT_ROLES=owner,developer,builder`。許可リストの Secret・mount は持たない）。NetworkPolicy は MCP と共通で変えない。bridge の宛先は pool-0 固定。token の回転は Secret を直して runtime を再起動（README「エージェント bridge の口」）
├── neuralwatt-exporter/    ← Neuralwatt の残高・使用量・キーの上限の Prometheus exporter (ghcr.io/lepinoid/neuralwatt-exporter、コードは exporters/neuralwatt/)。Service なし・Secret `dsh-agent-secrets` の API キー 1 キーだけをファイルで投影。外向きは標準 NetworkPolicy の許可と CiliumNetworkPolicy の egressDeny で絞る。scrape・アラートは turtton/infra 側 (PodMonitor・PrometheusRule)。ダッシュボードは grafana/dashboard-neuralwatt.yaml
├── dsh-agent/              ← dev 調整用の LLM エージェント dsh の専用 Pod (StatefulSet + PVC)。公開しない（port-forward で使う）。外向きは標準 NetworkPolicy の許可と CiliumNetworkPolicy の egressDeny で絞る。Secret `dsh-agent-secrets` はキーの保持者が暗号化する
└── build-server/           ← ビルドサーバー (PaperMC + MySQL) と plugin-updater の CronJob

updater/                    ← plugin-updater (Go)。build-server の LepinoidTools を、プレイヤーの退出を待ってから安全に更新する
exporters/neuralwatt/       ← neuralwatt-exporter (Go)。Neuralwatt の GET /v1/quota を一定間隔で取得して /metrics で返す。テストは ci/check-neuralwatt-exporter.sh
ci/                         ← workflow から呼ぶスクリプトと、そのテスト (ci/lib/*_test.sh)
terraform/                  ← Cloudflare（Tunnel・DNS・Access）の OpenTofu 構成 (PR の /tf-apply で適用)
```

Flux CD がこのリポジトリを GitRepository として監視し、`lepinoid` namespace にリソースを reconcile する。
