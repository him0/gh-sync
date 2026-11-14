<p align="center"><img src="assets/banner.svg" alt="gh sync: すべてのローカルブランチをコマンド 1 つで最新に" width="100%"></p>

# gh-sync

[English](README.md) | 日本語

[![CI](https://github.com/him0/gh-sync/actions/workflows/ci.yml/badge.svg)](https://github.com/him0/gh-sync/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/him0/gh-sync)](https://github.com/him0/gh-sync/releases/latest)
[![License: MIT](https://img.shields.io/github/license/him0/gh-sync)](LICENSE)

すべてのローカルブランチをコマンド 1 つでリモートの最新状態に追従させ、リモートでマージ・削除済みのブランチを片付ける [GitHub CLI](https://cli.github.com/) 拡張です。

[`hub sync`](https://hub.github.com/hub-sync.1.html) を単体の拡張として移植したもので、hub から gh に移行した人に向いています。

```console
$ gh sync
Updated branch main (was 3f2a1bc).
Updated branch feature/login (was 9d04e7a).
Deleted branch fix/typo (was c81b2d5).
warning: 'wip/experiment' seems to contain unpushed commits
```

## インストール

```bash
gh extension install him0/gh-sync
```

macOS・Linux・Windows・FreeBSD 向けにビルド済みバイナリを配布しているため、Go は不要です。

```bash
# 最新リリースにアップグレードする
gh extension upgrade sync

# バージョンを指定してインストールする
gh extension install him0/gh-sync --pin v0.5.0
```

## 使い方

git リポジトリの中で実行します。

```bash
gh sync
```

### 動作

1. **main remote を決めます。** `--remote` で指定した remote、無ければ `upstream`、`github`、`origin` のうち最初に見つかったもの、どれも無ければ最初の remote を使います。fork に `upstream` を追加している場合は、親リポジトリと同期します。
2. `git fetch --prune` で **その remote を fetch します。** ローカルブランチが追跡しているほかの remote(fork の `origin` など)も一緒に fetch します。fetch できなかった remote を追跡するブランチは、警告を出してスキップします。
3. [除外](#ブランチの除外)したものを除く **すべてのローカルブランチを** upstream と、upstream が無ければ main remote 上の同名のブランチと比較します。マージ済みかどうかは、常に main remote のデフォルトブランチに対して判定します。

| ローカルブランチ | 動作 |
| --- | --- |
| 遅れている | fast-forward する: `Updated branch <name> (was <sha>).` |
| 先行または分岐している | 残す: `warning: '<name>' seems to contain unpushed commits` |
| upstream が削除済みで、全コミットがデフォルトブランチに含まれる | 削除する: `Deleted branch <name> (was <sha>).` |
| upstream が削除済みで、先頭がマージ済み pull request の head と一致する(squash や rebase でのマージ) | 削除する: `Deleted branch <name> (was <sha>, merged via #<number>).` |
| upstream が削除済みで、それ以外 | 残す: `warning: '<name>' was deleted on <remote>, but appears not merged into '<default>'` |
| 最新、または remote に対応するブランチが無い | 何もしない |

### 安全性

- merge、rebase、強制更新、push は一切しません。ローカルにしか無いコミットを持つブランチは必ず残ります。
- 削除されうるのは、upstream を git が prune した(`[gone]`)ブランチだけです。upstream を一度も設定していないブランチや、upstream が fetch refspec の対象外のブランチ(single-branch クローン、`gh pr checkout` による `refs/pull/<N>/head`)は削除しません。
- マージ済み pull request は `gh api graphql` で 50 本ごとに 1 リクエストで確認します。確認できない場合(`gh` が未認証、GitHub 以外の remote、API エラー、30 秒のタイムアウト)はブランチを残し、理由は `--verbose` で表示します。`~/.ssh/config` のホストエイリアスや `ssh.github.com` は `ssh -G` で解決します。
- 削除するブランチをチェックアウト中の場合は、先にデフォルトブランチへ切り替えます(`Switched to branch ...`)。ローカルに無ければ main remote から作成します。
- 別の worktree でチェックアウトされているブランチは、警告を出してスキップします。

gh-sync が更新するのは**ローカル**のブランチだけです。fork を `upstream` と同期したあと、fork も更新するには push してください(例: `git push origin main`)。

## オプション

| オプション | 説明 |
| --- | --- |
| `--verbose` | 変更や検索を行う git・`gh` コマンドと、スキップしたブランチの note を stderr に表示する |
| `--dry-run` | ブランチを変更せずに `Would update branch ...` / `Would delete branch ...` を表示する。fetch は実行する |
| `--color=WHEN` | `auto`(デフォルト)、`always`、`never` |
| `--remote=NAME` | `NAME` を main remote として使う |
| `--exclude=GLOB` | `GLOB` に一致するブランチには触れない。複数回指定可。[ブランチの除外](#ブランチの除外)を参照 |
| `--version` | バージョンを表示する |
| `--help` | 使い方を表示する |

gh-sync は位置引数を取りません。`gh sync dry-run`(`--` の付け忘れ)は同期せずにエラーになります。

### ブランチの除外

除外したブランチは fast-forward も削除もされません。パターンは `--exclude` と git config の `gh-sync.exclude` から読み込み、すべてを適用します。

```sh
git config --add gh-sync.exclude 'wip/*'   # --global ですべてのリポジトリに適用
gh sync --exclude='wip/*' --exclude=tmp
```

パターンはブランチ名全体に対して Go の [`path.Match`](https://pkg.go.dev/path#Match) で照合するため、`*` は `/` に一致しません。`wip/*` は `wip/foo` に一致しますが、`wip/foo/bar` には一致しません。

### 色と出力

更新したブランチは緑、削除したブランチは赤、警告は黄、エラーは太字の赤で表示します。`--color=auto` では stdout と stderr をそれぞれ端末のときだけ色付けし、空でない [`NO_COLOR`](https://no-color.org) があれば色を付けません。

ブランチの更新・削除・切り替えは stdout に、警告、エラー、`--verbose` の出力、fetch の進捗は stderr に出力します。終了ステータスは、実行できなかったときや更新・削除・fetch に失敗したときは `1`、それ以外(警告を出してスキップした場合を含む)は `0` です。

## トラブルシューティング

**`could not determine default branch`**
main remote に使える `HEAD`、`main`、`master` がありません。`git remote set-head <remote> --auto` で設定してください。

**`warning: couldn't fast-forward '<name>'`**
チェックアウト中のブランチのローカルの変更が上書きされてしまいます。commit か stash をしてから、もう一度 `gh sync` を実行してください。

**`warning: '<name>' upstream config is broken; skipping`**
`branch.<name>.merge` が無いか、不正です。`git branch --set-upstream-to=<remote>/<branch> <name>` で直してください。

バグを報告するときは、`gh sync --version` と `gh sync --verbose` の出力を添えてください。

## 必要なもの

- [GitHub CLI](https://cli.github.com/) 2.0 以降
- Git 2.23 以降

## 開発

ツールのバージョンとタスクは [mise](https://mise.jdx.dev/) で管理しています。

```bash
mise run build   # ./gh-sync をビルドする
mise run test    # go test ./...
mise run lint    # go vet、gofmt のチェック、golangci-lint
```

`v*` タグを push するとリリースが公開されます。開発上の決まりは [CLAUDE.md](CLAUDE.md) を参照してください。

## 謝辞

[hub](https://github.com/mislav/hub) の `sync` コマンドに着想を得て、GitHub CLI 向けに独自に再実装したものです。

## ライセンス

[MIT](LICENSE)
