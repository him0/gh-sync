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
```

## Architecture

The entire application is contained in a single `main.go` file with the following key functions:

- `main()`: Entry point that orchestrates the sync workflow
- `getParentRepo()`: Uses `gh repo view --json parent` to get fork parent information
- `setupUpstream()`: Manages the upstream remote (adds or updates as needed)
- `detectDefaultBranch()`: Determines the default branch (prefers main over master)
- `runGit()`: Wrapper for executing git commands with output piped to stdout/stderr

## Key Implementation Details

1. **GitHub CLI Integration**: Uses `gh repo view` to get parent repository info, avoiding the need for GitHub API authentication
2. **Color Output**: Uses ANSI escape codes with isatty detection for terminal-aware coloring
3. **Default Branch Detection**: Checks upstream/HEAD first, then tries main, then master, defaulting to main
4. **Error Handling**: All errors exit with status code 1 and colored error messages to stderr

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