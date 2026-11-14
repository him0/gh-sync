package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMain isolates every git invocation in this package (including the ones
// made by run() itself) from the developer's global and system git config, so
// settings such as init.defaultBranch, commit.gpgsign, pull.rebase or hooks
// cannot change test outcomes. Commit identity is pinned via environment
// variables so commits work even without user.name / user.email configured.
func TestMain(m *testing.M) {
	env := map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@test.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@test.com",
		"GIT_TERMINAL_PROMPT": "0",
	}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "failed to set %s: %v\n", k, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// setupTestRepo creates a temporary git repo and returns its path. The initial
// branch is pinned to "main" so tests are independent of the developer's
// global `init.defaultBranch` config.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		// -c init.defaultBranch is honored by `git init` on git 2.28+; for
		// older versions, the env-style override still works because git
		// resolves config before reading the subcommand.
		{"git", "-c", "init.defaultBranch=main", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup command %v failed: %v\n%s", args, err, out)
		}
	}
	return dir
}

// ---------------------------------------------------------------------------
// Integration tests for run() / processBranch
//
// The helpers below build a small, self-contained "fork" topology:
//
//	upstream (working repo) --push--> remote.git (bare) <--fetch-- local (clone)
//
// `upstream` plays the role of other contributors / the GitHub side: tests
// commit and push there to make the remote move. `local` is the repository
// gh-sync runs in (the test's CWD), cloned from remote.git so it has an
// `origin` remote with refs/remotes/origin/HEAD set.
// ---------------------------------------------------------------------------

// syncFixture is a ready-to-use upstream/remote/local triple for exercising
// run() end to end.
type syncFixture struct {
	t        *testing.T
	upstream string // non-bare repo that pushes to remote
	remote   string // bare repo; the "origin" of local
	local    string // repo gh-sync operates on (also the test's CWD)
}

// newSyncFixture creates the fixture and chdirs into the local clone, so a
// subsequent f.sync() targets it. The default branch is "main".
func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	upstream := setupTestRepo(t)
	base := t.TempDir()
	f := &syncFixture{
		t:        t,
		upstream: upstream,
		remote:   filepath.Join(base, "remote.git"),
		local:    filepath.Join(base, "local"),
	}
	f.gitIn(base, "clone", "--quiet", "--bare", upstream, f.remote)
	f.gitIn(upstream, "remote", "add", "origin", f.remote)
	f.gitIn(upstream, "fetch", "--quiet", "origin")
	f.gitIn(upstream, "branch", "--quiet", "--set-upstream-to=origin/main", "main")
	f.gitIn(base, "clone", "--quiet", f.remote, f.local)
	f.gitIn(f.local, "config", "user.email", "test@test.com")
	f.gitIn(f.local, "config", "user.name", "Test")
	f.gitIn(f.local, "config", "commit.gpgsign", "false")
	t.Chdir(f.local)
	return f
}

// gitIn runs git in dir and returns trimmed stdout, failing the test on error.
func (f *syncFixture) gitIn(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("git %v (in %s) failed: %v\n%s", args, dir, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// upstreamGit / localGit run git in the respective repository.
func (f *syncFixture) upstreamGit(args ...string) string {
	f.t.Helper()
	return f.gitIn(f.upstream, args...)
}

func (f *syncFixture) localGit(args ...string) string {
	f.t.Helper()
	return f.gitIn(f.local, args...)
}

// commit creates an empty commit with msg on the checked-out branch of dir.
func (f *syncFixture) commit(dir, msg string) {
	f.t.Helper()
	f.gitIn(dir, "commit", "--quiet", "--allow-empty", "-m", msg)
}

// pushUpstreamBranch creates branch on upstream from its current main with
// the given commits, pushes it to the remote, and switches upstream back to
// main.
func (f *syncFixture) pushUpstreamBranch(branch string, commits ...string) {
	f.t.Helper()
	f.upstreamGit("checkout", "--quiet", "-b", branch, "main")
	for _, msg := range commits {
		f.commit(f.upstream, msg)
	}
	f.upstreamGit("push", "--quiet", "-u", "origin", branch)
	f.upstreamGit("checkout", "--quiet", "main")
}

// advanceUpstream checks out branch on upstream, adds commits, and pushes.
func (f *syncFixture) advanceUpstream(branch string, commits ...string) {
	f.t.Helper()
	f.upstreamGit("checkout", "--quiet", branch)
	for _, msg := range commits {
		f.commit(f.upstream, msg)
	}
	f.upstreamGit("push", "--quiet", "origin", branch)
	f.upstreamGit("checkout", "--quiet", "main")
}

// deleteRemoteBranch removes branch from the remote (as GitHub does after a
// PR is merged with "delete branch").
func (f *syncFixture) deleteRemoteBranch(branch string) {
	f.t.Helper()
	f.upstreamGit("push", "--quiet", "origin", "--delete", branch)
}

// trackRemoteBranch fetches and creates a local branch tracking
// origin/<branch>, as `git checkout <branch>` would after a fresh fetch.
func (f *syncFixture) trackRemoteBranch(branch string) {
	f.t.Helper()
	f.localGit("fetch", "--quiet", "origin")
	f.localGit("branch", "--quiet", "--track", branch, "origin/"+branch)
}

// sha returns the full SHA of ref in dir.
func (f *syncFixture) sha(dir, ref string) string {
	f.t.Helper()
	return f.gitIn(dir, "rev-parse", ref)
}

// localBranchExists reports whether refs/heads/<branch> exists in local.
func (f *syncFixture) localBranchExists(branch string) bool {
	f.t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = f.local
	return cmd.Run() == nil
}

// currentBranch returns the branch checked out in local.
func (f *syncFixture) currentBranch() string {
	f.t.Helper()
	return f.localGit("symbolic-ref", "--short", "HEAD")
}

// syncResult captures what a single run() invocation printed and returned.
type syncResult struct {
	stdout string
	stderr string
	err    error
}

// sync invokes run() in the local repo with --color=never (plus any extra
// args) and captures stdout/stderr. run() writes directly to os.Stdout and
// os.Stderr, so they are temporarily swapped for temp files; tests using this
// must not call t.Parallel (t.Chdir already forbids it).
func (f *syncFixture) sync(extraArgs ...string) syncResult {
	f.t.Helper()
	args := append([]string{"--color=never"}, extraArgs...)

	outFile, err := os.CreateTemp(f.t.TempDir(), "stdout")
	if err != nil {
		f.t.Fatal(err)
	}
	errFile, err := os.CreateTemp(f.t.TempDir(), "stderr")
	if err != nil {
		f.t.Fatal(err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outFile, errFile
	runErr := run(args)
	os.Stdout, os.Stderr = origOut, origErr

	read := func(file *os.File) string {
		f.t.Helper()
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			f.t.Fatal(err)
		}
		b, err := io.ReadAll(file)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			f.t.Fatal(err)
		}
		return string(b)
	}
	res := syncResult{stdout: read(outFile), stderr: read(errFile), err: runErr}
	f.t.Logf("run(%v) err=%v\nstdout:\n%s\nstderr:\n%s", args, res.err, res.stdout, res.stderr)
	return res
}

func assertContains(t *testing.T, label, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s does not contain %q:\n%s", label, want, got)
	}
}

func assertNotContains(t *testing.T, label, got, unwanted string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Errorf("%s unexpectedly contains %q:\n%s", label, unwanted, got)
	}
}

func TestRunFastForwardsCurrentBranch(t *testing.T) {
	f := newSyncFixture(t)
	before := f.sha(f.local, "main")
	f.advanceUpstream("main", "upstream-1", "upstream-2")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch main (was %s).", before[:7]))
	if got, want := f.sha(f.local, "main"), f.sha(f.upstream, "main"); got != want {
		t.Errorf("local main = %s, want %s", got, want)
	}
	// The checked-out branch is updated via merge --ff-only, so HEAD and the
	// working tree follow along.
	if got, want := f.sha(f.local, "HEAD"), f.sha(f.upstream, "main"); got != want {
		t.Errorf("local HEAD = %s, want %s", got, want)
	}
	if f.currentBranch() != "main" {
		t.Errorf("current branch = %q, want main", f.currentBranch())
	}
}

func TestRunFastForwardsNonCurrentBranch(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	before := f.sha(f.local, "feature")
	f.advanceUpstream("feature", "feature-2")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch feature (was %s).", before[:7]))
	if got, want := f.sha(f.local, "feature"), f.sha(f.upstream, "feature"); got != want {
		t.Errorf("local feature = %s, want %s", got, want)
	}
	if f.currentBranch() != "main" {
		t.Errorf("current branch = %q, want main", f.currentBranch())
	}
}

// TestRunFastForwardsBranchSharingNameWithTag covers a local branch that has
// the same name as a tag. `%(refname:short)` would list it as "heads/rel",
// which made gh-sync skip it silently (#57).
func TestRunFastForwardsBranchSharingNameWithTag(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("rel", "rel-1")
	f.trackRemoteBranch("rel")
	f.localGit("tag", "rel", "rel")
	before := f.sha(f.local, "refs/heads/rel")
	f.advanceUpstream("rel", "rel-2")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch rel (was %s).", before[:7]))
	if got, want := f.sha(f.local, "refs/heads/rel"), f.sha(f.upstream, "rel"); got != want {
		t.Errorf("local rel = %s, want %s", got, want)
	}
	if got := f.sha(f.local, "refs/tags/rel"); got != before {
		t.Errorf("tag rel moved to %s, want it left at %s", got, before)
	}
}

// TestRunFastForwardsCheckedOutBranchSharingNameWithTag is the checked-out
// variant: `symbolic-ref --short HEAD` would also print "heads/rel", so the
// current branch would not match the listed name and `branch -f` would be
// used on the checked-out branch, which git refuses.
func TestRunFastForwardsCheckedOutBranchSharingNameWithTag(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("rel", "rel-1")
	f.trackRemoteBranch("rel")
	f.localGit("tag", "rel", "rel")
	f.localGit("switch", "--quiet", "rel")
	before := f.sha(f.local, "refs/heads/rel")
	f.advanceUpstream("rel", "rel-2")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch rel (was %s).", before[:7]))
	want := f.sha(f.upstream, "rel")
	if got := f.sha(f.local, "refs/heads/rel"); got != want {
		t.Errorf("local rel = %s, want %s", got, want)
	}
	// Updated via merge --ff-only, so HEAD follows the branch.
	if got := f.sha(f.local, "HEAD"); got != want {
		t.Errorf("local HEAD = %s, want %s", got, want)
	}
	if got := f.sha(f.local, "refs/tags/rel"); got != before {
		t.Errorf("tag rel moved to %s, want it left at %s", got, before)
	}
}

// TestGetLocalBranchesDetachedHead checks that the "(HEAD detached at ...)"
// line printed by `git branch` is not returned as a branch name (#57).
func TestGetLocalBranchesDetachedHead(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	f.localGit("checkout", "--quiet", "--detach", "main")

	got, err := getLocalBranches()
	if err != nil {
		t.Fatalf("getLocalBranches() error: %v", err)
	}
	want := []string{"feature", "main"}
	if !slices.Equal(got, want) {
		t.Errorf("getLocalBranches() = %q, want %q", got, want)
	}
}

func TestRunDeletesMergedGoneBranch(t *testing.T) {
	for _, checkedOut := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkedOut=%v", checkedOut), func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("feature", "feature-1")
			f.trackRemoteBranch("feature")
			featureSHA := f.sha(f.local, "feature")
			if checkedOut {
				f.localGit("checkout", "--quiet", "feature")
			}

			// Merge feature into main upstream (fast-forward), then delete
			// the remote branch as a merged PR would.
			f.upstreamGit("merge", "--quiet", "--ff-only", "feature")
			f.upstreamGit("push", "--quiet", "origin", "main")
			f.deleteRemoteBranch("feature")

			res := f.sync()
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			deleted := fmt.Sprintf("Deleted branch feature (was %s).", featureSHA[:7])
			if checkedOut {
				// The checkout of the default branch is announced right
				// before the deletion.
				assertContains(t, "stdout", res.stdout, "Switched to branch main.\n"+deleted)
			} else {
				assertContains(t, "stdout", res.stdout, deleted)
				assertNotContains(t, "stdout", res.stdout, "Switched to branch")
			}
			if f.localBranchExists("feature") {
				t.Error("local branch feature still exists")
			}
			if f.currentBranch() != "main" {
				t.Errorf("current branch = %q, want main", f.currentBranch())
			}
		})
	}
}

func TestRunKeepsUnmergedGoneBranch(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	featureSHA := f.sha(f.local, "feature")
	f.deleteRemoteBranch("feature")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stderr", res.stderr, "warning: 'feature' was deleted on origin, but appears not merged into 'main'")
	assertNotContains(t, "stdout", res.stdout, "Deleted branch feature")
	if !f.localBranchExists("feature") {
		t.Fatal("unmerged branch feature was deleted")
	}
	if got := f.sha(f.local, "feature"); got != featureSHA {
		t.Errorf("local feature = %s, want unchanged %s", got, featureSHA)
	}
}

// TestRunDryRun checks that --dry-run reports every update and deletion a real
// sync would make (fast-forwarding non-current branches, deleting the
// checked-out merged branch) while leaving all branches and HEAD untouched.
func TestRunDryRun(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	f.pushUpstreamBranch("merged", "merged-1")
	f.trackRemoteBranch("merged")
	f.localGit("checkout", "--quiet", "merged")

	mainBefore := f.sha(f.local, "main")
	featureBefore := f.sha(f.local, "feature")
	mergedBefore := f.sha(f.local, "merged")

	f.advanceUpstream("feature", "feature-2")
	f.upstreamGit("checkout", "--quiet", "main")
	f.upstreamGit("merge", "--quiet", "--ff-only", "merged")
	f.upstreamGit("push", "--quiet", "origin", "main")
	f.deleteRemoteBranch("merged")

	res := f.sync("--dry-run")
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Would update branch main (was %s).", mainBefore[:7]))
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Would update branch feature (was %s).", featureBefore[:7]))
	// merged is checked out, so the preview must announce the checkout of the
	// default branch immediately before the deletion.
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Would switch to branch main.\nWould delete branch merged (was %s).", mergedBefore[:7]))
	assertNotContains(t, "stdout", res.stdout, "Updated branch")
	assertNotContains(t, "stdout", res.stdout, "Deleted branch")

	for branch, want := range map[string]string{"main": mainBefore, "feature": featureBefore, "merged": mergedBefore} {
		if !f.localBranchExists(branch) {
			t.Errorf("local branch %s was deleted", branch)
			continue
		}
		if got := f.sha(f.local, branch); got != want {
			t.Errorf("local %s = %s, want unchanged %s", branch, got, want)
		}
	}
	if f.currentBranch() != "merged" {
		t.Errorf("current branch = %q, want merged", f.currentBranch())
	}
}

func TestRunWarnsOnUnpushedCommits(t *testing.T) {
	tests := []struct {
		name            string
		upstreamAdvance bool // true: diverged (ahead and behind); false: ahead only
	}{
		{"ahead", false},
		{"diverged", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.commit(f.local, "local-only")
			if tt.upstreamAdvance {
				f.advanceUpstream("main", "upstream-1")
			}
			before := f.sha(f.local, "main")

			res := f.sync()
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			assertContains(t, "stderr", res.stderr, "warning: 'main' seems to contain unpushed commits")
			assertNotContains(t, "stdout", res.stdout, "Updated branch main")
			if got := f.sha(f.local, "main"); got != before {
				t.Errorf("local main = %s, want unchanged %s", got, before)
			}
		})
	}
}

func TestRunUpToDateIsNoop(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	mainBefore := f.sha(f.local, "main")
	featureBefore := f.sha(f.local, "feature")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty", res.stderr)
	}
	if got := f.sha(f.local, "main"); got != mainBefore {
		t.Errorf("local main = %s, want unchanged %s", got, mainBefore)
	}
	if got := f.sha(f.local, "feature"); got != featureBefore {
		t.Errorf("local feature = %s, want unchanged %s", got, featureBefore)
	}
}

// TestRunRejectsPositionalArguments checks that a positional argument is an
// error and no branch is touched (issue #60). Go's flag package stops at the
// first non-flag argument, so without this check "gh sync dry-run" (missing
// "--") and "gh sync foo --dry-run" (flag after a positional argument) would
// both run a real sync.
func TestRunRejectsPositionalArguments(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantArg string
	}{
		{"missing dashes", []string{"dry-run"}, "dry-run"},
		{"flag after positional", []string{"foo", "--dry-run"}, "foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("merged", "merged-1")
			f.trackRemoteBranch("merged")
			mainBefore := f.sha(f.local, "main")
			mergedBefore := f.sha(f.local, "merged")

			// Merging "merged" moves main forward, so a real sync would both
			// fast-forward main and delete merged.
			f.upstreamGit("merge", "--quiet", "--ff-only", "merged")
			f.upstreamGit("push", "--quiet", "origin", "main")
			f.deleteRemoteBranch("merged")

			res := f.sync(tt.args...)
			if res.err == nil {
				t.Fatal("run() error = nil, want an error")
			}
			assertContains(t, "stderr", res.stderr, fmt.Sprintf("fatal: unexpected argument: %q", tt.wantArg))
			assertContains(t, "stderr", res.stderr, "Usage: gh sync [options]")
			if res.stdout != "" {
				t.Errorf("stdout = %q, want empty", res.stdout)
			}
			if got := f.sha(f.local, "main"); got != mainBefore {
				t.Errorf("local main = %s, want unchanged %s", got, mainBefore)
			}
			if !f.localBranchExists("merged") {
				t.Fatal("local branch merged was deleted")
			}
			if got := f.sha(f.local, "merged"); got != mergedBefore {
				t.Errorf("local merged = %s, want unchanged %s", got, mergedBefore)
			}
		})
	}
}

// TestRunHelp checks that --help prints the usage to stdout, so
// `gh sync --help | less` works, and does not sync (issue #63).
func TestRunHelp(t *testing.T) {
	f := newSyncFixture(t)
	before := f.sha(f.local, "main")
	f.advanceUpstream("main", "upstream-1")

	res := f.sync("--help")
	if !errors.Is(res.err, flag.ErrHelp) {
		t.Fatalf("run() error = %v, want flag.ErrHelp", res.err)
	}
	if !strings.HasPrefix(res.stdout, "Usage: gh sync [options]\n") {
		t.Errorf("stdout does not start with the usage line:\n%s", res.stdout)
	}
	assertContains(t, "stdout", res.stdout, "--dry-run")
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty", res.stderr)
	}
	if got := f.sha(f.local, "main"); got != before {
		t.Errorf("local main moved to %s, want it left at %s", got, before)
	}
}

// TestRunVerbosePrintsGitErrorOnce checks that a failing git command under
// --verbose shows git's error only inside the warning, not a second time
// right after the command line (issue #63).
func TestRunVerbosePrintsGitErrorOnce(t *testing.T) {
	f := newSyncFixture(t)
	if err := os.WriteFile(filepath.Join(f.upstream, "f"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.upstreamGit("add", "f")
	f.commit(f.upstream, "add f")
	f.upstreamGit("push", "--quiet", "origin", "main")
	// An untracked f in local makes `git merge --ff-only` refuse to run.
	if err := os.WriteFile(filepath.Join(f.local, "f"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := f.sync("--verbose")
	if res.err == nil {
		t.Fatal("run() error = nil, want an error")
	}
	assertContains(t, "stderr", res.stderr, "$ git merge --ff-only --quiet refs/remotes/origin/main\nwarning: couldn't fast-forward 'main'")
	if n := strings.Count(res.stderr, "would be overwritten by merge"); n != 1 {
		t.Errorf("git's error appears %d times in stderr, want 1", n)
	}
}

// TestRunBeforeFirstFetch checks that a remote that was added but never
// fetched (no remote-tracking refs yet) can be synced: the default branch is
// determined after the fetch that creates those refs.
func TestRunBeforeFirstFetch(t *testing.T) {
	f := newSyncFixture(t)
	// Re-adding origin drops refs/remotes/origin/* (HEAD included) and the
	// upstream config of main, as in `git init && git remote add origin <url>`.
	f.localGit("remote", "remove", "origin")
	f.localGit("remote", "add", "origin", f.remote)
	before := f.sha(f.local, "main")
	f.advanceUpstream("main", "upstream-1")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertNotContains(t, "stderr", res.stderr, "could not determine default branch")
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch main (was %s).", before[:7]))
	if got, want := f.sha(f.local, "main"), f.sha(f.upstream, "main"); got != want {
		t.Errorf("local main = %s, want %s", got, want)
	}
}

// TestRunIgnoresDanglingRemoteHead checks that when refs/remotes/origin/HEAD
// points at a branch that was pruned (the remote's default branch was renamed
// from master to main), gh-sync falls back to main and still deletes merged
// branches that are gone on the remote.
func TestRunIgnoresDanglingRemoteHead(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("master")
	f.localGit("fetch", "--quiet", "origin")
	f.localGit("remote", "set-head", "origin", "master")
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	featureSHA := f.sha(f.local, "feature")

	f.upstreamGit("merge", "--quiet", "--ff-only", "feature")
	f.upstreamGit("push", "--quiet", "origin", "main")
	f.deleteRemoteBranch("feature")
	f.deleteRemoteBranch("master")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertNotContains(t, "stderr", res.stderr, "couldn't compare")
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Deleted branch feature (was %s).", featureSHA[:7]))
	if f.localBranchExists("feature") {
		t.Error("local branch feature still exists")
	}
}

// makePromisorRemote marks origin in the local clone as a promisor remote, as
// `git clone --filter=blob:none` does. `git remote -v` then prints the filter
// after "(fetch)" (e.g. "origin\t<url> (fetch) [blob:none]").
func (f *syncFixture) makePromisorRemote() {
	f.t.Helper()
	f.localGit("config", "remote.origin.promisor", "true")
	f.localGit("config", "remote.origin.partialclonefilter", "blob:none")
}

// TestRunPartialClone checks that a sync completes in a partial clone (#56).
func TestRunPartialClone(t *testing.T) {
	f := newSyncFixture(t)
	f.makePromisorRemote()
	before := f.sha(f.local, "main")
	f.advanceUpstream("main", "upstream-1")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch main (was %s).", before[:7]))
	if got, want := f.sha(f.local, "main"), f.sha(f.upstream, "main"); got != want {
		t.Errorf("local main = %s, want %s", got, want)
	}
}

func TestParseFetchRemoteLine(t *testing.T) {
	tests := []struct {
		line     string
		wantName string
		wantURL  string
		wantOK   bool
	}{
		{"origin\thttps://github.com/org/repo.git (fetch)", "origin", "https://github.com/org/repo.git", true},
		{"origin\thttps://github.com/org/repo.git (push)", "", "", false},
		{"origin\thttps://github.com/org/repo.git (fetch) [blob:none]", "origin", "https://github.com/org/repo.git", true},
		{"origin\thttps://github.com/org/repo.git (fetch) [tree:0]", "origin", "https://github.com/org/repo.git", true},
		{"local\t/path/with space/repo.git (fetch)", "local", "/path/with space/repo.git", true},
		{"", "", "", false},
	}
	for _, tt := range tests {
		name, url, ok := parseFetchRemoteLine(tt.line)
		if name != tt.wantName || url != tt.wantURL || ok != tt.wantOK {
			t.Errorf("parseFetchRemoteLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.line, name, url, ok, tt.wantName, tt.wantURL, tt.wantOK)
		}
	}
}
