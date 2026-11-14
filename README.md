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
warning: 'wip/experiment' seems to contain unpushed commits
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

1. **Picks the main remote.** The first one that exists out of `upstream`, `github`, `origin`; otherwise the first remote listed by git. In an ordinary clone that is `origin`. In a fork where you added an `upstream` remote, your branches are synced with the parent repository instead.
2. **Fetches it** with `git fetch --prune`.
3. **Walks every local branch** and compares it with its counterpart on the main remote: the configured upstream if the branch tracks that remote, otherwise the branch with the same name.

| State of the local branch | Action |
| --- | --- |
| Behind the remote branch | Fast-forwarded: `Updated branch <name> (was <sha>).` |
| Up to date | Nothing |
| Has commits that are not on the remote branch (ahead or diverged) | Left untouched: `warning: '<name>' seems to contain unpushed commits` |
| Upstream branch was deleted, and every commit is in the remote default branch | Deleted: `Deleted branch <name> (was <sha>).` |
| Upstream branch was deleted, but some commits are not in the remote default branch | Left untouched: `warning: '<name>' was deleted on <remote>, but appears not merged into '<default>'` |
| No counterpart on the main remote | Nothing |

### Safety

gh-sync only ever moves branches forward or removes ones that are fully merged:

- It never merges, rebases, force-updates or pushes. A branch with local-only commits is always left as is.
- A branch is deleted only when it tracked a branch on the main remote, that branch is gone, and the local branch has no commits missing from the remote default branch. Branches that never had an upstream are never deleted.
- If the branch being deleted is checked out, gh-sync switches to the default branch first and reports it as `Switched to branch ...`.
- A branch checked out in another worktree is not moved.

Two things worth knowing:

- Pull requests merged with **squash** or **rebase** leave commits that are not in the default branch, so those local branches are reported as "appears not merged" and kept. Delete them yourself with `git branch -D`.
- gh-sync updates your **local** branches only. When you sync a fork against `upstream`, push afterwards to update the fork on GitHub as well (e.g. `git push origin main`).

## Options

| Option | Description |
| --- | --- |
| `--verbose` | Print each git command that changes something, as `$ git ...`, to stderr (like hub's `HUB_VERBOSE=1`) |
| `--dry-run` | Show what would be updated or deleted (`Would update branch ...`, `Would delete branch ...`, preceded by `Would switch to branch ...` when the checked-out branch would be deleted) without changing any local branch. The fetch still runs |
| `--color=WHEN` | Colorize output: `auto` (default), `always` or `never` |
| `--version` | Print the gh-sync version and exit |
| `--help` | Print usage |

### Color

With `--color=auto`, stdout and stderr are decided independently: each stream is colorized only when it is a terminal. `gh sync | grep Updated` therefore emits no escape sequences even though warnings on the terminal stay colored.

Setting the [`NO_COLOR`](https://no-color.org) environment variable disables color in `auto` mode. `--color=always` overrides it.

### Output and exit status

- `Updated branch ...`, `Switched to branch ...` and `Deleted branch ...` (`Would update branch ...`, `Would switch to branch ...` and `Would delete branch ...` with `--dry-run`) go to **stdout**.
- Warnings, errors and `--verbose` logs go to **stderr**.
- The exit status is `0` on success, including when branches were skipped with a warning, and `1` when gh-sync could not run (not a git repository, no remote, fetch failed) or an update or deletion failed.

```console
$ gh sync --verbose
$ git fetch --prune --quiet --progress origin
$ git merge --ff-only --quiet refs/remotes/origin/main
Updated branch main (was 3f2a1bc).
```

## Troubleshooting

**`could not determine default branch`**
The remote has no `HEAD` reference and neither `main` nor `master`. Set it with:

```bash
git remote set-head <remote> --auto
```

**`warning: couldn't fast-forward '<name>'`**
The branch is checked out and local changes would be overwritten, or it is checked out in another worktree. Commit or stash the changes and run `gh sync` again.

**`warning: '<name>' upstream config is broken; skipping`**
`branch.<name>.merge` is missing or invalid. Repair it with:

```bash
git branch --set-upstream-to=<remote>/<branch> <name>
```

**The wrong remote is used**
The priority is fixed: `upstream`, `github`, `origin`. Rename your remotes accordingly (`git remote rename <old> upstream`).

When reporting a bug, please include the output of `gh sync --version` and `gh sync --verbose`.

## Requirements

- [GitHub CLI](https://cli.github.com/) 2.0 or later
- Git

## Development

Tool versions and tasks are managed with [mise](https://mise.jdx.dev/). The Go version comes from `go.mod`, and golangci-lint is pinned to the version CI uses.

```bash
mise run build   # build ./gh-sync
mise run test    # go test ./...
mise run lint    # go vet + gofmt check + golangci-lint

./gh-sync --verbose        # run the local build directly
gh extension install .     # or install the checkout as `gh sync`
```

Pushing a `v*` tag builds the binaries for all platforms and publishes a release. See [CLAUDE.md](CLAUDE.md) for the architecture, CI and release details.

## Acknowledgments

Inspired by the `sync` command of [hub](https://github.com/mislav/hub), which this extension reimplements on its own for the GitHub CLI.

## License

[MIT](LICENSE)
