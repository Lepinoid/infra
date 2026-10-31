# チェックポイントの再開

Updater は `lepinoid updater-checkpoint` の受付と、ワールド保存・push の完了を別々に確認する。`operationId=<uuid>` は受付の応答であり、完了を意味しない。

1 回の Job の待機上限は 5 分。保存がそれより長い場合は `checkpoint-timeout` で保留し、次の Job が同じ操作の確認から再開する。待機上限を超えても、サーバー側の保存は継続する。

## 永続する記録

`/data/plugins/.lepinoid/checkpoint/<transactionId>` に、要求と受付済みの操作を原子的に記録する。既存の journal の JSON 形式は変更しない。このディレクトリは旧版の sidecar / initContainer が行う journal inventory の対象外なので、操作IDの記録によって既存 journal の読み取り互換性は変わらない。

- RCON 要求前に要求の intent を保存する。
- 受付を確認した操作IDを、最初の完了確認より前に保存する。
- タイムアウト、Job の中断、status の読み取り障害では記録を保持する。
- 同じ Pod の既知の操作については、新しい保存を要求せず状態を確認する。
- 成功を確認した結果も保存し、次の phase を記録する前に Job が止まっても再利用する。
- 明確に失敗した操作は、その Job を `checkpoint-failed` で保留し、失敗の記録に基づいて次の Job で再試行する。
- Pod が変わった場合は、閉鎖状態を再検証してから新しい Pod で保存を要求する。
- トランザクション終端で、記録を journal とともに archive する。

既存の journal に記録がない場合も、要求前の intent から記録を開始する。旧版が行った過去の要求の操作IDを、journal だけから復元することはできない。

## 確認できない場合

完了確認には操作ID、Pod UID、`UPDATER_CHECKPOINT` trigger、結果、時刻、push 済みコミット SHA を使う。RCON 応答を失った場合は、要求の intent に対応すると確認できる status からだけ受付を引き継ぐ。確認できない場合は保留する。

LepinoidTools の `updater-status.json` は最新の操作 1 件を保持し、通常の定期保存でも上書きされる。既知の操作IDと異なる status は、新しい保存を要求してよい根拠にはならない。長時間の停止中に結果が上書きされた場合、自動復旧できず `checkpoint-timeout` が続くことがある。これは重複要求を防ぐための保留であり、保存成功とは扱わない。

診断時には、同じトランザクションの journal、checkpoint 記録、現在の Pod UID、`updater-status.json`、Job ログを照合する。記録を削除して再要求する前に、サーバー側の保存がまだ進行していないかと、ワールドの保存・push の結果を確認する必要がある。

## イメージへの反映

Go コードのマージだけでは稼働中イメージは変わらない。`Publish plugin updater` workflow でイメージを公開し、検証済み digest を使う別の保守変更が必要になる。イメージ更新には既存の active journal 不在の保守条件を適用する。この修正自体は Deployment / CronJob の digest や実クラスタの状態を変更しない。
