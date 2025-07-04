# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

gh-sync is a GitHub CLI extension that synchronizes a forked repository with its upstream parent repository. It's inspired by hub's sync command but implemented as a standalone gh extension.

## Build and Development Commands

```bash
# Build the binary
go build -o gh-sync .

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
  - `always`: Always colorize output
  - `never`: Never colorize output  
  - `auto`: Colorize output only when stdout is a terminal (default)

## Architecture

The entire application is contained in a single `main.go` file with the following key functions:

- `main()`: Entry point that orchestrates the sync workflow and parses command line flags
- `getMainRemote()`: Finds the main remote (priority: upstream, github, origin)
- `getDefaultBranch()`: Determines the default branch (prefers main over master)
- `runGitSilent()`: Wrapper for executing git commands with verbose logging support
- `verboseLog()`: Logs git commands when --verbose flag is enabled
- `colorizeOutput()`: Determines if color output should be enabled based on --color flag

## Key Implementation Details

1. **Command Line Parsing**: Uses Go's `flag` package for --verbose and --color options
2. **Verbose Logging**: Shows git commands in `$ git command args` format with magenta color
3. **Color Output**: Uses ANSI escape codes with isatty detection for terminal-aware coloring
4. **Default Branch Detection**: Checks remote/HEAD first, then tries main, then master, defaulting to main
5. **Error Handling**: All errors exit with status code 1 and colored error messages to stderr

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