# build-server

Lepinoid ビルドサーバー（PaperMC 1.21.1）を LXC から Kubernetes クラスタへ移行するためのマニフェスト。

## 構成

| リソース | 概要 |
|----------|------|
| `build-server` Deployment | `itzg/minecraft-server` で PaperMC 1.21.1 を起動。メモリ 12G（Aikar flags 有効） |
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

プラグイン（LepinoidTools 等）は PVC (`build-server-data`) 上で**手動管理**している。現時点でマニフェストからの自動配置は行わない。データ移行（ワールド・プラグインのコピー）も手動で実施する。

## 注意

- 現行の ResourceQuota（requests.cpu 1 / requests.memory 2Gi / limits.cpu 2 / limits.memory 4Gi / PVC 数 5）では本構成は拒否されるため、適用前にクラスタ側でクォータ引き上げが必要
