# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

gh-sync is a GitHub CLI extension that brings every local branch up to date with its remote (fast-forwarding branches that are behind) and deletes local branches that were merged and deleted on the remote. The remote is usually `origin`; in a fork with an `upstream` remote it is the parent repository. It's inspired by hub's sync command but implemented as a standalone gh extension.

## Build and Development Commands

Tasks are defined in `mise.toml` (tool versions are managed by mise; Go build/module caches live in `.cache/` inside the repository).

```bash
# Build the binary
mise run build

# Run tests
mise run test

# Run go vet and check gofmt
mise run lint

# Run go mod tidy to update dependencies
go mod tidy

# Test locally (from the gh-sync directory)
./gh-sync

# Test with verbose output
./gh-sync --verbose

# Test with color options
./gh-sync --color=always
./gh-sync --color=never
./gh-sync --color=auto
```

## Command Line Options

- `--verbose`: Show git commands being executed (similar to hub's HUB_VERBOSE=1)
- `--dry-run`: Report `Would update branch ...` / `Would delete branch ...` instead of fast-forwarding, checking out or deleting branches (fetch still runs)
- `--color[=WHEN]`: Control color output
  - `always`: Always colorize output (both stdout and stderr, even if `NO_COLOR` is set)
  - `never`: Never colorize output
  - `auto`: Colorize each stream only when it is a terminal (default). stdout messages (e.g. `Updated branch ...`) follow stdout, errors and verbose logs follow stderr, so `gh sync | grep Updated` emits no escape sequences. Color is disabled when the `NO_COLOR` environment variable is set (`--color=always` still forces it)
- `--version`: Print the version and exit (`dev` unless built by the release workflow)

## Architecture

The entire application is contained in a single `main.go` file with the following key functions:

- `main()`: Entry point that orchestrates the sync workflow and parses command line flags
- `getMainRemote()`: Finds the main remote (priority: upstream, github, origin)
- `getDefaultBranch()`: Determines the default branch (prefers main over master)
- `runGitSilent()`: Wrapper for executing git commands with verbose logging support
- `verboseLog()`: Logs git commands when --verbose flag is enabled
- `colorizeOutput()`: Decides whether one output stream should be colorized from the --color flag, that stream's TTY status and `NO_COLOR` (called separately for stdout and stderr)

## Key Implementation Details

1. **Command Line Parsing**: Uses Go's `flag` package for --verbose, --dry-run and --color options
2. **Verbose Logging**: Shows git commands in `$ git command args` format with magenta color
3. **Color Output**: Uses ANSI escape codes with per-stream isatty detection (`runConfig` holds separate `stdoutColors` / `stderrColors`)
4. **Default Branch Detection**: Checks remote/HEAD first, then tries main, then master, defaulting to main
5. **Error Handling**: All errors exit with status code 1 and colored error messages to stderr

## CI

GitHub Actions workflow (`.github/workflows/ci.yml`) runs on pushes to `main` and on pull requests:
- `go test ./...` on ubuntu, macOS and Windows
- `gofmt -l .` (fails if any file is unformatted), `go vet ./...` and golangci-lint on ubuntu

The Go version comes from `go.mod` (`go-version-file`), so bumping Go only requires updating `go.mod`.

### Workflow lockfile

Actions used by the workflows are pinned to verified commits in `.github/workflows/actions.lock`, and `.github/workflows/verify-lockfile.yml` fails CI when the lockfile is out of sync.

- After adding a workflow or changing any `uses:` line, re-run `gh actions-lock` at the repository root and commit the updated `actions.lock` (requires an authenticated `gh` and `gh extension install github/gh-actions-lock`).
- Never edit `actions.lock` by hand.
- `gh actions-lock` narrows partial versions (`@v7`) to full tags (`@v7.0.1`); keep that form.
- Use `gh actions-lock --relock` to advance a ref that moved upstream (after reviewing the upstream history).
- Dependabot (`.github/dependabot.yml`) bumps actions weekly and regenerates the matching lockfile entries in the same PR.

## Release Process

GitHub Actions workflow (`.github/workflows/release.yml`) uses [`cli/gh-extension-precompile`](https://github.com/cli/gh-extension-precompile) to build cross-platform binaries and publish a release (with generated release notes) when a version tag is pushed:
- Darwin (amd64, arm64)
- FreeBSD (386, amd64, arm64)
- Linux (386, amd64, arm, arm64)
- Windows (386, amd64, arm64)

Assets are named `<os>-<arch>[.exe]`, and tags containing `-` (e.g. `v1.0.0-rc.1`) are published as prereleases.

The tag name is embedded into the binary through `go_build_options` (`-X main.version=<tag>`) and reported by `--version`. The action already passes `-ldflags="-s -w"` and the last `-ldflags` wins, so `go_build_options` must stay a single `-ldflags=...` argument that repeats `-s -w`.

To release a new version:
```bash
git tag v1.0.0
git push origin v1.0.0
```