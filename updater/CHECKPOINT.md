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

Go コードのマージだけでは稼働中イメージは変わらない。`Publish plugin updater` workflow でイメージを公開し、検証済み digest を使う別の保守変更が必要になる。通常のイメージ更新は active journal がない状態で行い、Deployment の initContainer / sidecar、CronJob、rescue Pod の参照を同じ `<tag>@sha256:<digest>` にそろえる。

### 旧版のループで active journal が残っている場合

既存トランザクションを修正版で完了させる必要がある場合は、Deployment を先に更新せず、次の順序で復旧する。

1. CronJob の元の `spec.suspend` を控えてから一時停止する。進行中の旧 Job は中断せず、終了と `plugin-updater` Lease の解放を確認する。Job が終了してもサーバー側の保存は続き得るため、保存を強制停止しない。Flux に一時停止を消されないよう、patch には専用 field manager を指定する。
2. `kubectl create job <name> --from=cronjob/plugin-updater --dry-run=client -o json` で一時 Job の定義を作り、updater の image だけを公開済みの検証済み digest に置き換える。Pod template の `app=plugin-updater-job`、ServiceAccount、PVC、nodeSelector、`JOB_UID` / `DOCKER_CONFIG`、時間制限を維持していることを確認してから作成する。旧 sidecar / initContainer は固定したままにする。
3. 一時 Job を直列に実行し、同じトランザクションを再開する。5 分で `checkpoint-timeout` になった場合は checkpoint 記録と status を照合し、先の Job が終了して Lease を解放した後に次の一時 Job を実行する。保存の結果が不明な場合は記録を削除したり、journal の phase を書き換えたりして先へ進めない。
4. トランザクションが成功として archive され、active journal と `maintenance.flag` がなく、gate が `INACTIVE`、`current` が desired manifest と一致することを確認する。Job の `Complete` だけでは更新完了と判定しない。
5. active journal 不在を確認し、適用直前にプレイヤーが 0 人で保存が `idle` であることを再確認する。gate が開いてから新規入場があり得る場合は、保守中だけ接続経路を抑止し、復元できるよう元の設定を記録する。その後、全 4 箇所の image 参照をそろえた保守 PR をマージし、Flux の適用と Deployment の rollout 完了を確認する。initContainer / sidecar と CronJob が同じ digest を使い、build-server の全コンテナが Ready であることを確認する。
6. 一時的に抑止した接続経路と CronJob の `spec.suspend` を元に戻す。次の定期 Job が正常終了し、新たな checkpoint の繰り返しや入場閉鎖が発生していないことを確認してから、ログなどの証跡を保管し、不要な一時 Job を削除する。

途中で保存結果・閉鎖状態・Lease の所有権を確認できなくなった場合は、CronJob を停止したまま診断する。旧版イメージへ戻してループを再開させることや、進行中の保存を中断することを復旧手順には含めない。
