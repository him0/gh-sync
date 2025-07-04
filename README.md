# gh-sync

GitHub CLI Extension to sync your fork with its upstream repository.

## Features

- Automatically detects the parent repository
- Adds upstream remote if not present
- Fetches and merges the latest changes from upstream
- Pushes the changes to your fork
- Colored output for better visibility

## Installation

```bash
gh extension install him0/gh-sync
```

## Usage

```bash
gh sync
```

This will:
1. Check if the current directory is a git repository
2. Get the parent repository information
3. Add upstream remote if not present
4. Fetch from upstream
5. Merge upstream's default branch (main/master) into your current branch
6. Push to origin

### Example

```bash
$ cd my-forked-repo
$ gh sync
Parent repository: original-owner/original-repo
Upstream already configured: https://github.com/original-owner/original-repo.git
Fetching from upstream...
Default branch: main
Merging upstream/main...
Pushing to origin...
✓ Successfully synced with upstream
```

## Requirements

- [GitHub CLI](https://cli.github.com/) (gh) must be installed and authenticated
- Git must be installed
- The current directory must be a git repository that is a fork

## Notes

- The extension will warn you if you're not on the main/master branch
- If merge conflicts occur, you'll need to resolve them manually
- The extension uses the default branch of the upstream repository (main or master)

## Acknowledgments

This project is inspired by [hub](https://github.com/mislav/hub)'s `sync` command. While hub provides a comprehensive set of GitHub CLI features, gh-sync focuses solely on the fork synchronization functionality as a standalone GitHub CLI extension.

## License

MIT