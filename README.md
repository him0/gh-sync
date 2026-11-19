<p align="center"><img src="assets/banner.svg" alt="gh sync: every local branch up to date, in one command" width="100%"></p>

# gh-sync

English | [日本語](README.ja.md)

[![CI](https://github.com/him0/gh-sync/actions/workflows/ci.yml/badge.svg)](https://github.com/him0/gh-sync/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/him0/gh-sync)](https://github.com/him0/gh-sync/releases/latest)
[![License: MIT](https://img.shields.io/github/license/him0/gh-sync)](LICENSE)

A [GitHub CLI](https://cli.github.com/) extension that brings every local branch up to date with its remote in one command, and cleans up the branches that were merged and deleted there.

It is a standalone port of [`hub sync`](https://hub.github.com/hub-sync.1.html) for people who moved from hub to gh.

```console
$ gh sync
Updated branch main (was 3f2a1bc).
Updated branch feature/login (was 9d04e7a).
Deleted branch fix/typo (was c81b2d5).
warning: 'wip/experiment' has 2 commits not in 'origin/wip/experiment'; not updated
```

## Installation

```bash
gh extension install him0/gh-sync
```

Precompiled binaries are published for macOS, Linux, Windows and FreeBSD, so Go is not required.

```bash
# Upgrade to the latest release
gh extension upgrade sync

# Install a specific version
gh extension install him0/gh-sync --pin v0.5.0
```

## Usage

Run it anywhere inside a git repository:

```bash
gh sync
```

### What it does

1. **Picks the main remote**: the one given with `--remote`, otherwise the first of `upstream`, `github` and `origin` that exists, otherwise the first remote. In a fork with an `upstream` remote, your branches are synced with the parent repository.
2. **Fetches it** with `git fetch --prune`, together with every other remote a local branch tracks (such as your fork's `origin`). If one of those can't be fetched, the branches tracking it are skipped with a warning.
3. **Checks every local branch**, except [excluded](#excluding-branches) ones, against its upstream, or against the same-named branch on the main remote when it has none. Merges are always checked against the main remote's default branch.

| Local branch | Action |
| --- | --- |
| Behind | Fast-forwarded: `Updated branch <name> (was <sha>).` |
| Ahead | Kept: `warning: '<name>' has 2 commits not in '<remote>/<branch>'; not updated` |
| Diverged | Kept: `warning: '<name>' has diverged from '<remote>/<branch>' (2 ahead, 1 behind); not updated` |
| Upstream deleted, all commits in the default branch | Deleted: `Deleted branch <name> (was <sha>).` |
| Upstream deleted, tip is the head or one of the commits of a merged pull request (squash or rebase merge) | Deleted: `Deleted branch <name> (was <sha>, merged via #<number>).` |
| Upstream deleted, commits added on top of a merged pull request's head | Kept: `warning: '<name>' was deleted on <remote>, but 2 commits were added after it was merged via #<number>; not deleted` |
| Upstream deleted, otherwise | Kept: `warning: '<name>' was deleted on <remote>, but 1 commit is not in '<default>' and no merged pull request matches it; not deleted` |
| Up to date, or no counterpart on the remote | Nothing |

When no branch was updated or deleted, no warning was printed and no branch was skipped because of [another worktree](#other-worktrees), gh-sync prints `Already up to date.` to stderr if it is a terminal.

### Safety

- gh-sync never merges, rebases, force-updates or pushes. A branch with local-only commits is always kept.
- Only a branch whose upstream git pruned (`[gone]`) can be deleted. Branches that never had an upstream, or whose upstream is outside the fetch refspec (a single-branch clone, `refs/pull/<N>/head` from `gh pr checkout`), are never deleted.
- A branch that is behind its merged pull request (GitHub's "Update branch", a commit pushed from elsewhere) is deleted too: all its commits are in the pull request. Only the last 100 commits of a pull request are checked, and nothing extra is fetched.
- Merged pull requests are looked up with `gh api graphql`, one request per 50 branches. When that fails (`gh` not authenticated, a remote not on GitHub, an API error, a 30-second timeout), the branch is kept with `... and pull requests couldn't be looked up; not deleted`, and the reason is printed once as `warning: couldn't look up merged pull requests: <reason>`. SSH host aliases from `~/.ssh/config` and `ssh.github.com` are resolved with `ssh -G`.
- If the branch to delete is checked out, gh-sync first switches to the default branch (`Switched to branch ...`), creating it from the main remote if needed.
- A branch checked out in [another worktree](#other-worktrees) is never updated, and is deleted only with `--remove-worktrees`, which removes its worktree first. No worktree is removed without that option, and never with `--force`.

gh-sync updates **local** branches only. After syncing a fork against `upstream`, push to update the fork too (e.g. `git push origin main`).

### Other worktrees

git refuses to move or delete a branch that is checked out in another worktree, so gh-sync skips it and reports the skipped branches together at the end:

```console
warning: 2 merged branches are still checked out in other worktrees; not deleted:
  feature/a      .claude/worktrees/eager-wiggling-moonbeam
  feature/long   /home/me/elsewhere/wt
hint: remove a worktree with `git worktree remove <path>`, or run `gh sync --remove-worktrees` to remove the clean ones
note: 3 branches checked out in other worktrees are behind their upstream; not updated
```

- The warning lists the branches that would have been deleted. A worktree inside the main worktree is shown relative to it, any other with its absolute path.
- The note counts the branches that would have been fast-forwarded; `--verbose` lists them the same way. Update one with `git pull --ff-only` in its worktree.

#### Removing worktrees

With `--remove-worktrees`, or `git config gh-sync.removeWorktrees true`, gh-sync removes the worktree of a branch it is about to delete with `git worktree remove`, then deletes the branch:

```console
Removed worktree .claude/worktrees/eager-wiggling-moonbeam.
Deleted branch feature/a (was 1a2b3c4, merged via #12).
```

A worktree git refuses to remove is kept with its branch, and the exit status stays `0`:

```console
warning: 'feature/b' is checked out in .claude/worktrees/calm-river, which has modified or untracked files; not deleted
warning: 'feature/c' is checked out in /home/me/elsewhere/wt, which is locked; not deleted
```

Any other refusal, such as a worktree with submodules, is reported as `..., which couldn't be removed: <git's message>; not deleted`.

It is off by default because of what it can't protect:

- **Ignored files are deleted without asking.** git only refuses for modified or untracked files, so an ignored `.env` or build output in the worktree is lost.
- **gh-sync can't tell whether a shell or an agent session is still running in the worktree.** Its working directory disappears.

Also:

- The flag overrides the config: `--remove-worktrees=false` turns it off for one run.
- Worktrees of branches that are only behind are never removed, and neither is the main worktree.
- `--dry-run` prints `Would remove worktree <path>.` or the same warnings, from `git status` and the lock. It can't predict every refusal (submodules, for example).

## Options

| Option | Description |
| --- | --- |
| `--verbose` | Print the git and `gh` commands that change or look something up to stderr, plus notes on skipped branches and the list of branches not updated because of another worktree |
| `--dry-run` | Print `Would update branch ...` / `Would delete branch ...` / `Would remove worktree ...` without changing any branch or worktree. The fetch still runs |
| `--remove-worktrees` | Remove the worktree of a branch to delete, unless git refuses, then delete the branch. Deletes ignored files. Also `gh-sync.removeWorktrees` in git config. See [Removing worktrees](#removing-worktrees) |
| `--color=WHEN` | `auto` (default), `always` or `never` |
| `--remote=NAME` | Use `NAME` as the main remote |
| `--exclude=GLOB` | Leave branches matching `GLOB` alone. Repeatable. See [Excluding branches](#excluding-branches) |
| `--version` | Print the version |
| `--help` | Print usage |

gh-sync takes no positional arguments: `gh sync dry-run` (missing `--`) is rejected instead of running a sync.

### Excluding branches

Excluded branches are never fast-forwarded or deleted. Patterns come from `--exclude` and from the `gh-sync.exclude` git config key, and all of them apply:

```sh
git config --add gh-sync.exclude 'wip/*'   # add --global for every repository
gh sync --exclude='wip/*' --exclude=tmp
```

Patterns are matched against the whole branch name with Go's [`path.Match`](https://pkg.go.dev/path#Match), so `*` does not match `/`: `wip/*` matches `wip/foo` but not `wip/foo/bar`.

### Color and output

Updated branches are green, deleted ones red, warnings yellow and errors bold red. With `--color=auto`, stdout and stderr are each colorized only when they are a terminal, and a non-empty [`NO_COLOR`](https://no-color.org) disables color.

Branch updates, deletions and switches and worktree removals go to stdout; warnings, hints, notes, errors, `--verbose` output, fetch progress and `Already up to date.` go to stderr, the last two only when it is a terminal. The exit status is `1` when gh-sync could not run or an update, deletion or fetch failed, and `0` otherwise, including when branches were skipped with a warning or a note.

## Troubleshooting

**`could not determine default branch`**
The main remote has no usable `HEAD`, `main` or `master`. Set it with `git remote set-head <remote> --auto`.

**`warning: couldn't fast-forward '<name>'`**
Local changes in the checked-out branch would be overwritten. Commit or stash them and run `gh sync` again.

**`warning: '<name>' upstream config is broken; skipping`**
`branch.<name>.merge` is missing or invalid. Fix it with `git branch --set-upstream-to=<remote>/<branch> <name>`.

**`warning: couldn't look up merged pull requests: <reason>`**
Branches whose commits are not in the default branch are kept until the lookup works. Check `gh auth status`; a remote that is not on GitHub can't be looked up.

When reporting a bug, please include the output of `gh sync --version` and `gh sync --verbose`.

## Requirements

- [GitHub CLI](https://cli.github.com/) 2.0 or later
- Git 2.23 or later

## Development

Tool versions and tasks are managed with [mise](https://mise.jdx.dev/).

```bash
mise run build   # build ./gh-sync
mise run test    # go test ./...
mise run lint    # go vet + gofmt check + golangci-lint
```

Pushing a `v*` tag publishes a release. See [CLAUDE.md](CLAUDE.md) for the contribution rules.

## Acknowledgments

Inspired by the `sync` command of [hub](https://github.com/mislav/hub), which this extension reimplements on its own for the GitHub CLI.

## License

[MIT](LICENSE)
