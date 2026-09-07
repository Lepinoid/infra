# updater core — incomplete, do not enable

このディレクトリは issue #4 の部分実装です。**CronJob を有効化したり、本イメージを本番 sidecar/initContainer に配置したりしないでください。**

`run` は固定名 Lease を取得できない場合だけ正常終了します。取得できた場合は Lease を解放し、`transaction runner is not yet integrated; refusing all PVC mutations` で非ゼロ終了します。自動更新はまだ実行しません。

## 実装済みの部品

- 管理 CLI: JSON 原子的書き込み、file/dir fsync、checksum、jar owner/mode、rename
- Lease 取得・更新・heartbeat 部品、Pod UID/generation/recovery/blocking fencing 判定
- journal モデルと厳密 JSON decoder、deploymentIdentity、FMI 理由列挙
- player count、checkpoint 応答、gate freshness/identity、起動状態の判定
- archive tar のメンバー検査、compatibility/checksum 検証
- init recovery、readiness、閉鎖解除、jar 切替の部品と局所テスト
- desired ConfigMap、artifact 解決 workflow、管理イメージ公開 workflow

## 未完了・要修正

- `run` の A1〜B11 制御、heartbeat の起動・停止接続、リモート sidecar ファイル操作、desired YAML 読み取り、supersede、drift repair、checkpoint 完了待機、rollout/watch、rollback、終端確定と archive。
- phase 別 recovery の完全な契約実装。現行 init recovery は commitCandidate 未設定時に SOURCE を選択するため、INSTALL_COMPLETE 後の正常 TARGET 再起動との調停が未実装。
- init recovery は `server.properties` CAS の競合防止ロック、全エラー経路の recovery 記録、厳密な recovery chain 検証、gate backup の正式配置契約が未完成。
- 永続 JSON Schema と infra 固有の journal/current/recovery/metadata canonical fixture。decoder は構造を検査するが、すべての意味制約・列挙値を網羅していない。
- GC 実行との接続、archive/recovery 関連単位・保持期間による削除、重複 digest の扱い。
- Docker ビルドと同梱バイナリ実行の検証。busybox は `/bin/busybox` のみで、kubectl cp が必要とする `tar` エントリポイントは未配置。
- workflow の実行検証、GHCR 側での immutable policy の保証。現行公開 workflow は直列化と既存タグ拒否のみで、別 publisher との競合を registry 側で防ぐものではない。
- Deployment/CronJob/Lease/RBAC/NetworkPolicy/kustomization の組み込みはこの部分実装では変更していない。
- 実 Paper/RCON fixture 採取、実クラスタ障害注入、実クライアントの全ログイン拒否検証は未実施。RCON fixture は依頼に示された Paper 1.21.1 出力例に基づく。

管理イメージを手動 workflow で初回公開した後、**digest を確定して別 PR で追従する必要があります**。その前に上記の安全性欠落を解消し、active journal 不在の専用保守手順を整備してください。イメージ公開 workflow は Deployment を更新しません。

## ローカル検証

```sh
go test -race -shuffle=on -count=1 ./...
go vet ./...
gofmt -l .
CGO_ENABLED=0 go build -o bin/updater .
```

外部 Go 依存はありません。共有 fixture は `build-server/contract-fixtures/shared` に配置し、LepinoidTools `17a4cf6` の fixture と同一内容です。
