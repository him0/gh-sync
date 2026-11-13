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

1. **main remote を決めます。** `upstream`、`github`、`origin` の順に探して最初に見つかったものを使い、どれも無ければ git が最初に列挙する remote を使います。通常の clone では `origin` です。fork に `upstream` を追加している場合は、代わりに親リポジトリと同期します。
2. `git fetch --prune` で **その remote を fetch します。**
3. **すべてのローカルブランチを順に調べ**、main remote 上の対応するブランチと比較します。ブランチがその remote を追跡していれば設定済みの upstream と、そうでなければ同名のブランチと比較します。

| ローカルブランチの状態 | 動作 |
| --- | --- |
| リモートブランチより遅れている | fast-forward する: `Updated branch <name> (was <sha>).` |
| 最新 | 何もしない |
| リモートブランチに無いコミットがある(先行または分岐) | 触らない: `warning: '<name>' seems to contain unpushed commits` |
| upstream ブランチが削除済みで、全コミットがリモートのデフォルトブランチに含まれる | 削除する: `Deleted branch <name> (was <sha>).` |
| upstream ブランチが削除済みだが、リモートのデフォルトブランチに無いコミットがある | 触らない: `warning: '<name>' was deleted on <remote>, but appears not merged into '<default>'` |
| main remote に対応するブランチが無い | 何もしない |

### 安全性

gh-sync が行うのは、ブランチを前に進めることと、完全にマージ済みのブランチを削除することだけです。

- merge、rebase、強制更新、push は一切しません。ローカルにしか無いコミットを持つブランチは必ずそのまま残ります。
- ブランチを削除するのは、main remote 上のブランチを追跡していて、そのブランチが消えており、かつリモートのデフォルトブランチに無いコミットを持たない場合だけです。upstream を一度も設定していないブランチは削除しません。
- 削除対象のブランチをチェックアウト中の場合は、先にデフォルトブランチへ切り替えます。
- 別の worktree でチェックアウトされているブランチは動かしません。

知っておくとよい点が 2 つあります。

- **squash** や **rebase** でマージされた pull request は、デフォルトブランチに無いコミットが残るため、ローカルブランチは「appears not merged」と報告されて残ります。`git branch -D` で自分で削除してください。
- gh-sync が更新するのは**ローカル**のブランチだけです。fork を `upstream` と同期した場合、GitHub 上の fork も更新するには、あとで push してください(例: `git push origin main`)。

## オプション

| オプション | 説明 |
| --- | --- |
| `--verbose` | 変更を伴う git コマンドを `$ git ...` の形式で stderr に表示する(hub の `HUB_VERBOSE=1` 相当) |
| `--dry-run` | 更新・削除されるブランチを表示する(`Would update branch ...`、`Would delete branch ...`。チェックアウト中のブランチが削除される場合は、その前に `Would switch to branch ...`)だけで、ローカルブランチは変更しない。fetch は実行する |
| `--color=WHEN` | 出力に色を付ける: `auto`(デフォルト)、`always`、`never` |
| `--version` | gh-sync のバージョンを表示して終了する |
| `--help` | 使い方を表示する |

### 色

`--color=auto` では stdout と stderr を別々に判定し、それぞれ端末に接続されているときだけ色を付けます。そのため `gh sync | grep Updated` ではエスケープシーケンスが出力されず、端末に出る警告には色が付いたままになります。

環境変数 [`NO_COLOR`](https://no-color.org) を設定すると、`auto` では色が無効になります。`--color=always` はこれより優先されます。

### 出力と終了ステータス

- `Updated branch ...` と `Deleted branch ...`(`--dry-run` では `Would update branch ...`、`Would switch to branch ...`、`Would delete branch ...`)は **stdout** に出力されます。
- 警告、エラー、`--verbose` のログは **stderr** に出力されます。
- 終了ステータスは、成功時(警告を出してブランチをスキップした場合を含む)が `0`、実行できなかったとき(git リポジトリではない、remote が無い、fetch に失敗した)や更新・削除に失敗したときが `1` です。

```console
$ gh sync --verbose
$ git fetch --prune --quiet --progress origin
$ git merge --ff-only --quiet refs/remotes/origin/main
Updated branch main (was 3f2a1bc).
```

## トラブルシューティング

**`could not determine default branch`**
remote に `HEAD` の参照が無く、`main` も `master` もありません。次のコマンドで設定してください。

```bash
git remote set-head <remote> --auto
```

**`warning: couldn't fast-forward '<name>'`**
チェックアウト中のブランチでローカルの変更が上書きされてしまうか、別の worktree でチェックアウトされています。変更を commit または stash してから、もう一度 `gh sync` を実行してください。

**`warning: '<name>' upstream config is broken; skipping`**
`branch.<name>.merge` が無いか、不正です。次のコマンドで直してください。

```bash
git branch --set-upstream-to=<remote>/<branch> <name>
```

**意図しない remote が使われる**
優先順位は `upstream`、`github`、`origin` で固定です。これに合わせて remote の名前を変更してください(`git remote rename <old> upstream`)。

バグを報告するときは、`gh sync --version` と `gh sync --verbose` の出力を添えてください。

## 必要なもの

- [GitHub CLI](https://cli.github.com/) 2.0 以降
- Git

## 開発

ツールのバージョンとタスクは [mise](https://mise.jdx.dev/) で管理しています。

```bash
mise run build   # ./gh-sync をビルドする
mise run test    # go test ./...
mise run lint    # go vet と gofmt のチェック

./gh-sync --verbose        # ローカルビルドを直接実行する
gh extension install .     # またはチェックアウトを `gh sync` としてインストールする
```

`v*` タグを push すると、全プラットフォーム向けのバイナリがビルドされ、リリースが公開されます。アーキテクチャ、CI、リリースの詳細は [CLAUDE.md](CLAUDE.md) を参照してください。

## 謝辞

[hub](https://github.com/mislav/hub) の `sync` コマンドに着想を得て、GitHub CLI 向けに独自に再実装したものです。

## ライセンス

[MIT](LICENSE)
