# Code Review: gh-sync

## Summary

Overall the code is well-structured for a single-file CLI tool. The main concerns are around error handling in critical git operations and lack of tests.

---

## High Priority

### 1. Return values of `runGitSilent` are ignored in `processBranch` (main.go:259-261, 279)

`git merge --ff-only`, `git update-ref`, and `git branch -D` can fail, but their errors are not checked. The success message is printed regardless.

**Fix:** Check the return value and only print the success message when the operation succeeds.

### 2. `fmt.Sscanf` results are not validated (main.go:301, 309)

In `getCommitDifference`, if `Sscanf` fails to parse an integer, `ahead`/`behind` remain 0. This could lead to unintended fast-forwards or branch deletions.

**Fix:** Use `strconv.Atoi` or check the scan count returned by `Sscanf`.

### 3. No tests exist

A tool that deletes branches and updates refs should have tests. At minimum, unit tests for `getCommitDifference`, `getDefaultBranch`, and `colorizeOutput` would add safety.

---

## Medium Priority

### 4. Non-deterministic remote ordering (main.go:146-149)

`getRemotes` iterates over a map, so the fallback `&remotes[0]` in `getMainRemote` may return different remotes across runs.

**Fix:** Sort remotes by name or use a slice-based approach to preserve insertion order.

### 5. `runGitSilent` discards stderr (main.go:323-327)

When git commands fail, error details are lost. Users cannot diagnose failures.

**Fix:** Capture stderr and log it when `--verbose` is enabled, or include it in error messages.

### 6. `verboseLog` color logic is inconsistent with `--color` flag (main.go:332)

`verboseLog` independently checks `os.Stderr` isatty, ignoring the `--color` flag. `--color=always` does not affect verbose log output when stderr is piped.

**Fix:** Use the same color decision from the `--color` flag for verbose output.

### 7. Global mutable color variables (main.go:19-28)

Color strings are global variables mutated in `main()`. This makes the code harder to test and reason about.

**Fix:** Use a config struct passed to functions that need color information.

---

## Low Priority

### 8. Invalid `--color` values are silently accepted (main.go:347-348)

Typos like `--color=alwyas` silently fall through to `auto` behavior.

**Fix:** Print a warning or error for unrecognized values.

### 9. `getCommitDifference` makes two git calls (main.go:294-311)

`git rev-list --left-right --count branch1...branch2` can get both ahead and behind counts in a single call.

**Fix:** Use the combined form for better performance with many branches.

### 10. `softprops/action-gh-release@v1` is outdated (release.yml:67)

v2 is available. v1 uses Node.js 16 which may trigger GitHub Actions deprecation warnings.

### 11. `exitWithError` calls `os.Exit(1)` directly (main.go:352-354)

This prevents deferred functions from running and makes the function untestable.

**Fix:** Return errors to `main()` and call `os.Exit` only there.
