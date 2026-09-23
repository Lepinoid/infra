# Lease と Flux の所有権

`plugin-updater` Lease は Flux が事前作成し、取得・更新・解放は updater が担当する。Lease manifest の `kustomize.toolkit.fluxcd.io/ssa: IfNotPresent` は必須。Flux による既存 Lease の再適用は行わない。Lease の時間定数は `internal/lease/lease.go`（duration 60 秒、renew deadline 30 秒、heartbeat 5 秒）が実行時の正本となる。

updater が変更する Kubernetes フィールドには `--field-manager=plugin-updater` を明示する。Lease の runtime fields だけでなく、Deployment の `lepinoid.dev/restart-transaction` / `lepinoid.dev/restart-requested-at` annotation にも適用する。これらの annotation は Git の Deployment manifest に固定値として追加しない。

Lease 更新は `metadata.resourceVersion` を含む JSON merge patch を使う。取得時に読んだ resourceVersion と競合したら更新は失敗し、別 holder を上書きしない。部分的な Lease オブジェクトを `kubectl replace` すると Flux の label/annotation などまで消してしまうため使用しない。

## infra#36 の原因

Flux kustomize-controller v1.9.5 は既定で `kubectl` prefix の field manager を cleanup し、Git の望ましい状態を再適用する。従来の updater は `kubectl-replace` で holderIdentity/renewTime を更新していた。10 分ごとの reconcile により両フィールドが消え、直前の renew が成功していても次の heartbeat が holder 不一致を検出して停止した。

2026-09-22 UTC の実測:

| Flux Lease 再適用 | updater 停止 |
| --- | --- |
| 16:32:41.844 | 16:32:43.363 |
| 16:42:54.656 | 16:42:57.883 |
| 16:53:07.538 | 16:53:08.193 |

ジョブ非実行時の Lease watch と明示 reconcile でも、`holderIdentity` / `renewTime` が削除されることを確認した。Lease 喪失時に停止する処理は正しいため、deadline 延長や holder 不一致の無視では対処しない。

- [Flux の apply policy](https://fluxcd.io/flux/components/kustomize/kustomizations/#controlling-the-apply-behavior-of-resources)
- [稼働 controller の cleanup 設定](https://github.com/fluxcd/kustomize-controller/blob/v1.9.5/internal/controller/kustomization_controller.go)
- [SSA v0.76.2 の manager prefix matching](https://github.com/fluxcd/pkg/blob/ssa/v0.76.2/ssa/patch.go)

## 障害時の確認

heartbeat のエラーは API の read/update 失敗、holder 不一致、record 期限切れ、local renewal deadline 超過を区別し、観測 holder / resourceVersion / 最終成功からの経過をログに残す。transaction の cancellation cause に Lease 喪失理由を保持する。SIGTERM/SIGINT または正常終了に伴う heartbeat 停止は Lease 喪失として扱わない。終了時は heartbeat を停止・join してから、時間制限付きで Lease を解放する。

`context canceled` を調べる際は Job の開始・終了時刻、Lease の managedFields、Flux reconcile 時刻、Pod の termination reason を突き合わせる。`main` は SIGTERM/SIGINT でも context をキャンセルするため、文字列だけから heartbeat が原因と断定しない。
