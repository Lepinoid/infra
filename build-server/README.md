# build-server

Lepinoid ビルドサーバー（PaperMC 1.21.8 + Build 60）を LXC から Kubernetes クラスタへ移行するためのマニフェスト。

## 構成

| リソース | 概要 |
|----------|------|
| `build-server` Deployment | `itzg/minecraft-server` で PaperMC 1.21.8（`PAPER_BUILD=60` 固定）を起動。メモリ 12G（Aikar flags 有効） |
| `build-server` Service | Tailscale LoadBalancer（`build-server` ホスト名、`tag:mcserver`）で 25565 番ポートを公開 |
| `build-server-mysql` Deployment / Service | CoreProtect 用 MySQL 8.4。ClusterIP の 3306 番ポート |
| `build-server-mysql-dump` CronJob | 毎日 03:30 JST に `coreprotect` DB を mysqldump してバックアップ PVC に gzip 保存 |
| initContainer `git-credentials` | Secret から PAT を展開し `/data/.git-credentials`・`/data/.gitconfig` を生成。LepinoidTools AutoCommit プラグインの `git push` 用 |

ストレージはすべて `longhorn-gameserver` StorageClass（strict-local、レプリカ 1）。`dataLocality: strict-local` のため、Pod は `nodeSelector` で mainworker-1 にピン留めしている（本リポジトリの通常ルール「nodeSelector 設定禁止」の例外）。

## MySQL パスワードのローテーション

1. 新しいパスワードを生成する（例: `openssl rand -base64 24`）
2. `sops build-server/mysql-credentials.sops.yaml` で `MYSQL_ROOT_PASSWORD` / `MYSQL_PASSWORD` を更新
3. MySQL コンテナ内で `ALTER USER` を実行（root と `coreprotect` ユーザーの両方）:

   ```sql
   ALTER USER 'root'@'localhost' IDENTIFIED BY '<新しいrootパスワード>';
   ALTER USER 'coreprotect'@'%' IDENTIFIED BY '<新しいcoreprotectパスワード>';
   ```

4. commit & push して Flux に反映させ、Pod を再起動

## プラグインについて

### OCI 管理（自動更新基盤で扱うもの）

LepinoidTools (`LepinoidTools.jar`) と Multiverse-Core (`Multiverse-Core.jar`) は **自動更新基盤 (issue #4)** で管理する。`plugin-versions.yaml` の desired manifest を CronJob `plugin-updater` (5 分周期) が検出し、gate プラグインによる login 閉鎖 → jar 置換 → Pod 再起動 → 閉鎖解除を自動で行う。journal は `/data/plugins/.lepinoid/` に atomic に記録され、CronJob 中断時も次回起動で phase に応じて回復する。手動介入が必要な場合は `journal/.blocking/` に blocking record が記録される (rescue 手順は `updater/README.md`)。

plugin-updater Pod の egress は CiliumNetworkPolicy `plugin-updater-egress` で kube-apiserver (443) / GHCR (443) / kube-dns (53) の 3 系統のみに制限している。MC バージョンが desired manifest の `supportedMinecraft` に一致しない場合は updater が SUSPENDED で保留する（詳細は issue #12）。

### PVC 手動維持（有料）

- **Arceon**（現行 0.5.6）
- **HeadDatabase**（現行 4.24.0）

### 無料プラグイン（1.21.8 バンプ時に PVC 配置済み、今後 PLUGINS 移行対象）

- **FastAsyncWorldEdit (FAWE)** 2.15.4 — Modrinth 安定版
- **FastAsyncVoxelSniper (FAVS)** 3.2.5 — Modrinth 安定版（FAWE 必須依存）
- **goBrush** 3.8.0-114 — https://ci.athion.net/job/goBrush-1.13+/ の Jenkins 最新 (Arcaniax)
- **goPaint** 3.1.0-120 — https://ci.athion.net/job/goPaint-1.14+/ の Jenkins 最新 (Arcaniax)
- **BuildersUtilities** 2.1.1-161 — https://ci.athion.net/job/Builders-Utilities/ の Jenkins 最新（**Arcaniax 原版**。旧 TehBrian fork とは別物）
- **CoreProtect** CE 24.0 — Modrinth 版。MySQL を使用する別 Deployment あり
- **spark** — Paper 1.21+ 内蔵のため独立 jar なし

### 廃止

- **MetaBrushes**: 公式更新が停止し 1.21.8 で非対応のため。jar 削除済み
- **LunaChat**: 現行未導入、当面不要

## 更新手順

プラグインの更新は用途に応じて 3 系統ある:

1. **LepinoidTools のバージョン更新** → CI で bundle 発行 → repository_dispatch で desired auto 更新 → updater が自動適用
2. **その他のプラグインの更新** → PVC `/data/plugins` 上で jar を入れ替える（専用保守）。`build-server/plugins-staging/<YYYYMMDD>/` に新しい jar をステージして入れ替える
3. **Paper / Minecraft のバージョン更新** → `deployment.yaml` の `VERSION` / `PAPER_BUILD` を変更して rollout restart

## rescue（障害時の診断・修復 Pod）

`build-server-data` PVC は RWO のため、rescue Pod を起動する前に build-server を必ず 0 replicas へ scale し、同時書き込みによる破損を避ける。rescue が稼働している間は updater CronJob を suspend のまま維持する。

1. updater CronJob を停止し、既に実行中の updater がないことを確認する。

   ```sh
   set -euo pipefail
   ORIGINAL_SUSPEND=$(kubectl get cronjob/plugin-updater -n lepinoid -o jsonpath='{.spec.suspend}')
   ORIGINAL_SUSPEND=${ORIGINAL_SUSPEND:-false}
   kubectl patch cronjob/plugin-updater -n lepinoid --type=merge -p '{"spec":{"suspend":true}}'
   updater_pods=$(kubectl get pod -n lepinoid -l app=plugin-updater-job --field-selector=status.phase!=Succeeded,status.phase!=Failed --no-headers)
   test -z "$updater_pods"
   ```

2. build-server を停止し、Pod が完全に消滅したことを確認する。

   ```sh
   set -euo pipefail
   kubectl -n lepinoid scale deploy/build-server --replicas=0
   build_server_pods=$(kubectl get pod -n lepinoid -l app=build-server --field-selector=status.phase!=Succeeded,status.phase!=Failed --no-headers)
   test -z "$build_server_pods"
   ```

3. Flux 管理外の rescue Pod を手動で作成し、PVC を診断・修復する。

   ```sh
   set -euo pipefail
   kubectl apply -f build-server/rescue-pod.yaml
   kubectl exec -it build-server-rescue -n lepinoid -- /bin/busybox sh
   ```

4. 修復後は rescue Pod を削除して build-server と CronJob を元に戻す。

   ```sh
   set -euo pipefail
   kubectl delete pod build-server-rescue -n lepinoid
   kubectl -n lepinoid scale deploy/build-server --replicas=1
   kubectl patch cronjob/plugin-updater -n lepinoid --type=merge -p "{\"spec\":{\"suspend\":$ORIGINAL_SUSPEND}}"
   ```

## ブートストラップ記録（1.21.8 バンプ時）

初回起動時の設定値:

- Date: 202X-XX-XX（予定）
- MC: 1.21.1 → 1.21.8
- Bundle digest: sha256:4f2877bd8671c5459d9e3bf837f441dafc33c7dd552cf3fa0b0baacce74cdb38（2026.09.08-1.21.8-062df）
- Gate digest: sha256:229145904b3b0c5cc42f6d350396e2a21a3e2209aa27e2a10440a30521a750f6（2026.09.08-062df）
- Multiverse: 5.7.1 → 5.8.1

## 注意

- 現行の ResourceQuota（requests.cpu 1 / requests.memory 2Gi / limits.cpu 2 / limits.memory 4Gi / PVC 数 5）では本構成は拒否されるため、適用前にクラスタ側でクォータ引き上げが必要
