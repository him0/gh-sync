# gh-sync

GitHub CLI Extension to sync your fork with its upstream repository.

## Features

- Automatically detects the main remote (upstream, github, origin priority)
- Fetches and syncs local branches with their remote counterparts
- Fast-forwards branches that are behind their remote tracking branches
- Deletes local branches that were deleted on the remote (if safely merged)
- Colored output for better visibility
- Verbose logging support to see git commands being executed

## Installation

```bash
gh extension install him0/gh-sync
```

### Upgrade

```bash
gh extension upgrade gh-sync
```

## Usage

```bash
gh sync [options]
```

### Options

- `--verbose`: Show git commands being executed (similar to hub's HUB_VERBOSE=1)
- `--color[=WHEN]`: Control color output
  - `always`: Always colorize output
  - `never`: Never colorize output  
  - `auto`: Colorize output only when stdout is a terminal (default)

### Examples

```bash
# Basic sync
gh sync

# Sync with verbose output
gh sync --verbose

# Sync with color control
gh sync --color=always
gh sync --color=never

# Combine options
gh sync --verbose --color=never
```

This will:
1. Check if the current directory is a git repository
2. Find the main remote (priority: upstream, github, origin)
3. Fetch from the main remote with --prune
4. Process each local branch:
   - If branch is behind its remote: fast-forward merge
   - If branch has unpushed commits: show warning
   - If remote branch was deleted and local branch is merged: delete local branch

### Example Output

```bash
$ gh sync --verbose
$ git fetch --prune --quiet --progress origin
Updated branch feature-branch (was abc1234).
Deleted branch old-feature (was def5678).
warning: 'work-in-progress' seems to contain unpushed commits
```

## Requirements

- [GitHub CLI](https://cli.github.com/) (gh) must be installed and authenticated
- Git must be installed
- The current directory must be a git repository with configured remotes

## Notes

- The extension processes all local branches and their remote tracking branches
- Branches with unpushed commits are left untouched (with a warning)
- Local branches are only deleted if they appear to be safely merged into the default branch
- The extension uses the default branch of the main remote (main or master)

## Acknowledgments

This project is inspired by [hub](https://github.com/mislav/hub)'s `sync` command. While hub provides a comprehensive set of GitHub CLI features, gh-sync focuses solely on the fork synchronization functionality as a standalone GitHub CLI extension.

## License

MIT