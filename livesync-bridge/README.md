# LiveSync Bridge の Git 同期

Obsidian LiveSync で同期した vault を Git リポジトリへ反映する。Git 操作は vault 直下の `commit.md` に記載した次の単独行で指示できる。

```markdown
commit=false
pull=false
```

## Obsidian からの操作

操作前に両方のフラグを `false` にして保存し、LiveSync で同期する。その後、次のどちらか一つだけを変更して保存・同期する。

- **コミットと push**: `commit=true` にする。vault の変更をコミットし、リモートへ push する。新しい変更がない場合も push を試みるため、直前に push だけが失敗した場合の再試行にも使える。
- **リモートの変更の取り込み**: `pull=true` にする。fetch と merge を行い、競合箇所ではリモート側の内容を優先する。

処理開始時に対象フラグが `false` に戻る。`false` に戻ったことだけでは push の成功を意味しないため、`commit.md` のエラー表示と GitHub 上の最新コミットを確認する。再操作する場合も、`commit=false` と `pull=false` の状態を保存・同期してから、一つだけを `commit=true` または `pull=true` にする。

## 失敗した場合

`commit.md` に追記される「Git 同期エラー」を読み、失敗した処理段階と再試行の案内を確認する。通信障害による push の失敗なら、通信が復旧した後に `commit=true` で再試行できる。

認証エラーやマージ・履歴の競合、再試行で解消しない問題は、`commit.md` のエラー内容を管理者へ共有する。管理者はファイル内の案内に沿って Git の状態を確認し、必要な復旧を行う。

## デプロイされる機能

`helmrelease.yaml` は chart の最低バージョンを `1.0.2` にしている。このバージョンから、`commit.md` へのエラー案内、Git 操作の失敗検出、変更がない場合の push 再試行、および checksum に基づく Pod の自動 rollout に対応する。
