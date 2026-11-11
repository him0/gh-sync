# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

gh-sync is a GitHub CLI extension that synchronizes a forked repository with its upstream parent repository. It's inspired by hub's sync command but implemented as a standalone gh extension.

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
- `--color[=WHEN]`: Control color output
  - `always`: Always colorize output (both stdout and stderr, even if `NO_COLOR` is set)
  - `never`: Never colorize output
  - `auto`: Colorize each stream only when it is a terminal (default). stdout messages (e.g. `Updated branch ...`) follow stdout, errors and verbose logs follow stderr, so `gh sync | grep Updated` emits no escape sequences. Color is disabled when the `NO_COLOR` environment variable is set (`--color=always` still forces it)

## Architecture

The entire application is contained in a single `main.go` file with the following key functions:

- `main()`: Entry point that orchestrates the sync workflow and parses command line flags
- `getMainRemote()`: Finds the main remote (priority: upstream, github, origin)
- `getDefaultBranch()`: Determines the default branch (prefers main over master)
- `runGitSilent()`: Wrapper for executing git commands with verbose logging support
- `verboseLog()`: Logs git commands when --verbose flag is enabled
- `colorizeOutput()`: Decides whether one output stream should be colorized from the --color flag, that stream's TTY status and `NO_COLOR` (called separately for stdout and stderr)

## Key Implementation Details

1. **Command Line Parsing**: Uses Go's `flag` package for --verbose and --color options
2. **Verbose Logging**: Shows git commands in `$ git command args` format with magenta color
3. **Color Output**: Uses ANSI escape codes with per-stream isatty detection (`runConfig` holds separate `stdoutColors` / `stderrColors`)
4. **Default Branch Detection**: Checks remote/HEAD first, then tries main, then master, defaulting to main
5. **Error Handling**: All errors exit with status code 1 and colored error messages to stderr

## CI

GitHub Actions workflow (`.github/workflows/ci.yml`) runs on pushes to `main` and on pull requests:
- `go test ./...` on ubuntu, macOS and Windows
- `gofmt -l .` (fails if any file is unformatted), `go vet ./...` and golangci-lint on ubuntu

The Go version comes from `go.mod` (`go-version-file`), so bumping Go only requires updating `go.mod`.

## Release Process

GitHub Actions workflow (`.github/workflows/release.yml`) builds cross-platform binaries when a version tag is pushed:
- Darwin (amd64, arm64)
- Linux (amd64, arm64)  
- Windows (amd64)

To release a new version:
```bash
git tag v1.0.0
git push origin v1.0.0
```