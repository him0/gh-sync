package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

	// findMergedPRs is passed to run(). It defaults to a finder that always
	// fails, so tests never call gh; tests of the pull request path replace it.
	findMergedPRs mergedPRFinder
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
		findMergedPRs: func(runConfig, Remote, string, []string) (map[string][]mergedPR, error) {
			return nil, errors.New("pull request lookup is not stubbed in this test")
		},
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
	runErr := run(args, f.findMergedPRs)
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
func TestReadRefsDetachedHead(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	f.localGit("checkout", "--quiet", "--detach", "main")

	refs, err := readRefs("origin")
	if err != nil {
		t.Fatalf("readRefs() error: %v", err)
	}
	var got []string
	for _, b := range refs.branches {
		got = append(got, b.name)
	}
	want := []string{"feature", "main"}
	if !slices.Equal(got, want) {
		t.Errorf("readRefs() branches = %q, want %q", got, want)
	}
}

// TestReadRefs checks the tip, upstream and ahead/behind counts readRefs
// reports for branches that are in sync, ahead, behind, diverged, gone and
// without upstream, and that it lists the main remote's refs.
func TestReadRefs(t *testing.T) {
	f := newSyncFixture(t)
	for _, name := range []string{"same", "ahead", "behind", "diverged", "gone"} {
		f.pushUpstreamBranch(name, name+"-1")
		f.trackRemoteBranch(name)
	}
	f.localGit("branch", "--no-track", "untracked", "main")
	f.localGit("checkout", "--quiet", "ahead")
	f.commit(f.local, "ahead-2")
	f.localGit("checkout", "--quiet", "diverged")
	f.commit(f.local, "diverged-local")
	f.localGit("checkout", "--quiet", "main")
	f.advanceUpstream("behind", "behind-2", "behind-3")
	f.advanceUpstream("diverged", "diverged-remote")
	f.deleteRemoteBranch("gone")
	f.localGit("fetch", "--quiet", "--prune", "origin")

	refs, err := readRefs("origin")
	if err != nil {
		t.Fatalf("readRefs() error: %v", err)
	}
	got := make(map[string]localBranch)
	for _, b := range refs.branches {
		got[b.name] = b
	}
	want := map[string]localBranch{
		"main":      {name: "main", upstream: "refs/remotes/origin/main"},
		"same":      {name: "same", upstream: "refs/remotes/origin/same"},
		"ahead":     {name: "ahead", upstream: "refs/remotes/origin/ahead", ahead: 1},
		"behind":    {name: "behind", upstream: "refs/remotes/origin/behind", behind: 2},
		"diverged":  {name: "diverged", upstream: "refs/remotes/origin/diverged", ahead: 1, behind: 1},
		"gone":      {name: "gone", upstream: "refs/remotes/origin/gone", gone: true},
		"untracked": {name: "untracked"},
	}
	if len(got) != len(want) {
		t.Errorf("readRefs() returned %d branches, want %d: %+v", len(got), len(want), refs.branches)
	}
	for name, w := range want {
		w.tip = f.sha(f.local, "refs/heads/"+name)
		if got[name] != w {
			t.Errorf("branch %s = %+v, want %+v", name, got[name], w)
		}
	}
	for _, ref := range []string{"refs/remotes/origin/main", "refs/remotes/origin/behind"} {
		if !refs.remoteRefs[ref] {
			t.Errorf("remoteRefs lacks %s: %v", ref, refs.remoteRefs)
		}
	}
	if refs.remoteRefs["refs/remotes/origin/gone"] {
		t.Errorf("remoteRefs has pruned refs/remotes/origin/gone")
	}
}

func TestParseTrack(t *testing.T) {
	tests := []struct {
		track         string
		ahead, behind int
		gone, wantErr bool
	}{
		{track: ""},
		{track: "[gone]", gone: true},
		{track: "[ahead 3]", ahead: 3},
		{track: "[behind 2]", behind: 2},
		{track: "[ahead 1, behind 12]", ahead: 1, behind: 12},
		{track: "ahead 1", wantErr: true},
		{track: "[ahead x]", wantErr: true},
		{track: "[voraus 1]", wantErr: true},
		{track: "[]", wantErr: true},
	}
	for _, tt := range tests {
		ahead, behind, gone, err := parseTrack(tt.track)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseTrack(%q) error = %v, wantErr %v", tt.track, err, tt.wantErr)
			continue
		}
		if ahead != tt.ahead || behind != tt.behind || gone != tt.gone {
			t.Errorf("parseTrack(%q) = %d, %d, %v, want %d, %d, %v", tt.track, ahead, behind, gone, tt.ahead, tt.behind, tt.gone)
		}
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

// TestRunDeletesCheckedOutBranchWithoutLocalDefault covers a clone without a
// local default branch where two remotes have one: a plain `git checkout main`
// is ambiguous there, so the new local main must track the main remote.
func TestRunDeletesCheckedOutBranchWithoutLocalDefault(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	featureSHA := f.sha(f.local, "feature")
	f.localGit("checkout", "--quiet", "feature")
	f.localGit("branch", "--quiet", "-D", "main")
	// A second remote that also has main. Its name ranks below origin, so
	// origin stays the main remote.
	f.localGit("remote", "add", "mirror", f.remote)
	f.localGit("fetch", "--quiet", "mirror")

	f.upstreamGit("merge", "--quiet", "--ff-only", "feature")
	f.upstreamGit("push", "--quiet", "origin", "main")
	f.deleteRemoteBranch("feature")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Switched to branch main.\nDeleted branch feature (was %s).", featureSHA[:7]))
	if f.localBranchExists("feature") {
		t.Error("local branch feature still exists")
	}
	if f.currentBranch() != "main" {
		t.Errorf("current branch = %q, want main", f.currentBranch())
	}
	if got := f.localGit("rev-parse", "--abbrev-ref", "main@{upstream}"); got != "origin/main" {
		t.Errorf("main tracks %q, want origin/main", got)
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

// squashMergeAndDelete simulates GitHub's "Squash and merge" followed by
// deleting the branch: a new commit lands on main, so the branch's own commits
// never become reachable from the default branch.
func (f *syncFixture) squashMergeAndDelete(branch string) {
	f.t.Helper()
	f.upstreamGit("checkout", "--quiet", "main")
	f.commit(f.upstream, branch+" (squashed)")
	f.upstreamGit("push", "--quiet", "origin", "main")
	f.deleteRemoteBranch(branch)
}

// prLookup records the arguments a mergedPRFinder was called with.
type prLookup struct {
	remote string
	base   string
	heads  []string
}

// stubMergedPRs returns a finder that answers from prs (keyed by remote
// branch name) and records every call in calls.
func stubMergedPRs(prs map[string][]mergedPR, calls *[]prLookup) mergedPRFinder {
	return func(_ runConfig, remote Remote, base string, heads []string) (map[string][]mergedPR, error) {
		*calls = append(*calls, prLookup{remote: remote.Name, base: base, heads: slices.Clone(heads)})
		found := make(map[string][]mergedPR)
		for _, h := range heads {
			found[h] = prs[h]
		}
		return found, nil
	}
}

func TestRunDeletesSquashMergedBranch(t *testing.T) {
	tests := []struct {
		name       string
		local      string // local branch name; it always tracks origin/feature
		checkedOut bool
		dryRun     bool
	}{
		{"not checked out", "feature", false, false},
		{"checked out", "feature", true, false},
		// The pull request must be looked up by the remote branch name.
		{"local name differs", "topic", false, false},
		{"dry run", "feature", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("feature", "feature-1")
			f.localGit("fetch", "--quiet", "origin")
			f.localGit("branch", "--quiet", "--track", tt.local, "origin/feature")
			tip := f.sha(f.local, tt.local)
			if tt.checkedOut {
				f.localGit("checkout", "--quiet", tt.local)
			}
			f.squashMergeAndDelete("feature")

			var calls []prLookup
			// An older merged pull request from the same branch name does
			// not match the tip and must be skipped.
			f.findMergedPRs = stubMergedPRs(map[string][]mergedPR{
				"feature": {{Number: 99, HeadRefOid: strings.Repeat("0", 40)}, {Number: 123, HeadRefOid: tip}},
			}, &calls)
			var args []string
			if tt.dryRun {
				args = append(args, "--dry-run")
			}
			res := f.sync(args...)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}

			want := prLookup{remote: "origin", base: "main", heads: []string{"feature"}}
			if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
				t.Errorf("findMergedPRs calls = %+v, want one call %+v", calls, want)
			}
			verb := "Deleted"
			if tt.dryRun {
				verb = "Would delete"
			}
			deleted := fmt.Sprintf("%s branch %s (was %s, merged via #123).", verb, tt.local, tip[:7])
			switch {
			case tt.dryRun:
				assertContains(t, "stdout", res.stdout, "Would switch to branch main.\n"+deleted)
			case tt.checkedOut:
				assertContains(t, "stdout", res.stdout, "Switched to branch main.\n"+deleted)
			default:
				assertContains(t, "stdout", res.stdout, deleted)
			}
			assertNotContains(t, "stderr", res.stderr, "appears not merged")

			if tt.dryRun {
				f.assertBranchAt(tt.local, tip)
				if f.currentBranch() != tt.local {
					t.Errorf("current branch = %q, want %s", f.currentBranch(), tt.local)
				}
				return
			}
			if f.localBranchExists(tt.local) {
				t.Errorf("local branch %s still exists", tt.local)
			}
			if f.currentBranch() != "main" {
				t.Errorf("current branch = %q, want main", f.currentBranch())
			}
		})
	}
}

// TestRunLooksUpSquashMergedBranchesTogether checks that all candidate
// branches are looked up in a single call, with each remote branch name once,
// and that only branches whose tip matches are deleted.
func TestRunLooksUpSquashMergedBranchesTogether(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature-a", "a-1")
	f.pushUpstreamBranch("feature-b", "b-1")
	f.pushUpstreamBranch("feature-c", "c-1")
	f.trackRemoteBranch("feature-a")
	f.trackRemoteBranch("feature-b")
	f.trackRemoteBranch("feature-c")
	f.localGit("branch", "--quiet", "--track", "copy-of-a", "origin/feature-a")
	tipA := f.sha(f.local, "feature-a")
	tipB := f.sha(f.local, "feature-b")
	for _, b := range []string{"feature-a", "feature-b", "feature-c"} {
		f.squashMergeAndDelete(b)
	}

	var calls []prLookup
	f.findMergedPRs = stubMergedPRs(map[string][]mergedPR{
		"feature-a": {{Number: 1, HeadRefOid: tipA}},
		"feature-b": {{Number: 2, HeadRefOid: tipB}},
	}, &calls)
	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}

	want := prLookup{remote: "origin", base: "main", heads: []string{"feature-a", "feature-b", "feature-c"}}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Errorf("findMergedPRs calls = %+v, want one call %+v", calls, want)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Deleted branch copy-of-a (was %s, merged via #1).", tipA[:7]))
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Deleted branch feature-a (was %s, merged via #1).", tipA[:7]))
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Deleted branch feature-b (was %s, merged via #2).", tipB[:7]))
	assertContains(t, "stderr", res.stderr, "warning: 'feature-c' was deleted on origin, but appears not merged into 'main'")
	// feature-c has no merged pull request and must be kept.
	f.assertBranchAt("feature-c", f.sha(f.upstream, "feature-c"))
}

// TestRunKeepsSquashMergedBranchWithoutPR checks the fallback: when no merged
// pull request matches the tip, or the lookup fails, the branch is kept with
// the usual warning. The failure reason is shown only with --verbose.
func TestRunKeepsSquashMergedBranchWithoutPR(t *testing.T) {
	lookupErr := errors.New("gh: not logged in")
	tests := []struct {
		name     string
		prs      []mergedPR // merged pull requests for "feature"
		err      error
		verbose  bool
		wantNote bool
	}{
		{"no merged pull request", nil, nil, false, false},
		// Commits were added after the pull request was merged.
		{"head differs from tip", []mergedPR{{Number: 7, HeadRefOid: strings.Repeat("0", 40)}}, nil, false, false},
		{"lookup failed", nil, lookupErr, false, false},
		{"lookup failed, verbose", nil, lookupErr, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("feature", "feature-1")
			f.trackRemoteBranch("feature")
			tip := f.sha(f.local, "feature")
			f.squashMergeAndDelete("feature")
			f.findMergedPRs = func(runConfig, Remote, string, []string) (map[string][]mergedPR, error) {
				if tt.err != nil {
					return nil, tt.err
				}
				return map[string][]mergedPR{"feature": tt.prs}, nil
			}
			var args []string
			if tt.verbose {
				args = append(args, "--verbose")
			}

			res := f.sync(args...)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			assertContains(t, "stderr", res.stderr, "warning: 'feature' was deleted on origin, but appears not merged into 'main'")
			assertNotContains(t, "stdout", res.stdout, "Deleted branch feature")
			if tt.wantNote {
				assertContains(t, "stderr", res.stderr, "note: couldn't look up merged pull requests: gh: not logged in")
			} else {
				assertNotContains(t, "stderr", res.stderr, "note: couldn't look up")
			}
			f.assertBranchAt("feature", tip)
		})
	}
}

func TestBuildMergedPRQuery(t *testing.T) {
	// Characters that would break or alter the query if interpolated.
	heads := []string{"feature", `evil") { x } #`}
	query, vars := buildMergedPRQuery(gitHubRepo{host: "github.com", owner: "him0", name: "gh-sync"}, "main", heads)

	wantVars := []queryVar{
		{"owner", "him0"}, {"name", "gh-sync"}, {"base", "main"},
		{"h0", "feature"}, {"h1", `evil") { x } #`},
	}
	if !reflect.DeepEqual(vars, wantVars) {
		t.Errorf("vars = %+v, want %+v", vars, wantVars)
	}
	for _, want := range []string{
		"query($owner: String!, $name: String!, $base: String!, $h0: String!, $h1: String!) {",
		"repository(owner: $owner, name: $name) {",
		"b0: pullRequests(headRefName: $h0, baseRefName: $base, states: MERGED, first: 20, orderBy: {field: UPDATED_AT, direction: DESC}) {",
		"b1: pullRequests(headRefName: $h1, baseRefName: $base, states: MERGED, first: 20, orderBy: {field: UPDATED_AT, direction: DESC}) {",
		"nodes { number headRefOid }",
	} {
		assertContains(t, "query", query, want)
	}
	for _, leaked := range []string{"him0", "gh-sync", "evil", "feature"} {
		assertNotContains(t, "query", query, leaked)
	}
}

func TestParseMergedPRResponse(t *testing.T) {
	heads := []string{"feature", "fix"}
	got, err := parseMergedPRResponse([]byte(`{"data":{"repository":{
		"b0":{"nodes":[{"number":12,"headRefOid":"aaaa"},{"number":3,"headRefOid":"bbbb"}]},
		"b1":{"nodes":[]}}}}`), heads)
	if err != nil {
		t.Fatalf("parseMergedPRResponse() error: %v", err)
	}
	want := map[string][]mergedPR{
		"feature": {{Number: 12, HeadRefOid: "aaaa"}, {Number: 3, HeadRefOid: "bbbb"}},
		"fix":     {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseMergedPRResponse() = %+v, want %+v", got, want)
	}

	for name, output := range map[string]string{
		"repository not found": `{"data":{"repository":null}}`,
		"missing alias":        `{"data":{"repository":{"b0":{"nodes":[]}}}}`,
		"GraphQL error":        `{"data":null,"errors":[{"message":"Something went wrong"}]}`,
		"not JSON":             `oops`,
	} {
		if _, err := parseMergedPRResponse([]byte(output), heads); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestParseGitHubURL(t *testing.T) {
	tests := []struct {
		url     string
		want    gitHubRepo
		wantErr bool
	}{
		{url: "https://github.com/him0/gh-sync.git", want: gitHubRepo{"github.com", "him0", "gh-sync"}},
		{url: "https://github.com/him0/gh-sync", want: gitHubRepo{"github.com", "him0", "gh-sync"}},
		{url: "https://github.com/him0/gh-sync/", want: gitHubRepo{"github.com", "him0", "gh-sync"}},
		{url: "https://user@ghe.example.com/org/repo.git", want: gitHubRepo{"ghe.example.com", "org", "repo"}},
		{url: "git@github.com:him0/gh-sync.git", want: gitHubRepo{"github.com", "him0", "gh-sync"}},
		{url: "github.com:him0/gh-sync", want: gitHubRepo{"github.com", "him0", "gh-sync"}},
		{url: "ssh://git@github.com/him0/gh-sync.git", want: gitHubRepo{"github.com", "him0", "gh-sync"}},
		{url: "ssh://git@ghe.example.com:2222/org/repo.git", want: gitHubRepo{"ghe.example.com", "org", "repo"}},
		{url: "/home/me/repos/gh-sync.git", wantErr: true},
		{url: `C:\repos\gh-sync.git`, wantErr: true},
		{url: "C:/repos/gh-sync.git", wantErr: true},
		{url: "file:///home/me/repos/gh-sync.git", wantErr: true},
		{url: "https://gitlab.example.com/group/sub/repo.git", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, err := parseGitHubURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGitHubURL(%q) = %+v, want error", tt.url, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubURL(%q) error: %v", tt.url, err)
			}
			if got != tt.want {
				t.Errorf("parseGitHubURL(%q) = %+v, want %+v", tt.url, got, tt.want)
			}
		})
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

// TestRunColorsWarnings checks that warnings are yellow when stderr is
// colorized and plain otherwise, like errors are light red.
func TestRunColorsWarnings(t *testing.T) {
	tests := []struct {
		color string
		want  string
	}{
		{"always", "\033[33mwarning: 'main' seems to contain unpushed commits\033[0m\n"},
		{"never", "warning: 'main' seems to contain unpushed commits\n"},
	}
	for _, tt := range tests {
		t.Run(tt.color, func(t *testing.T) {
			f := newSyncFixture(t)
			f.commit(f.local, "local-only")

			res := f.sync("--color=" + tt.color)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			assertContains(t, "stderr", res.stderr, tt.want)
			if tt.color == "never" {
				assertNotContains(t, "stderr", res.stderr, "\033[")
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

func TestRunRemoteFlag(t *testing.T) {
	f := newSyncFixture(t)
	// A second remote "fork" (a copy of remote.git) where feature moves on.
	// It ranks below origin, so only --remote=fork selects it.
	fork := filepath.Join(t.TempDir(), "fork.git")
	f.gitIn(f.upstream, "clone", "--quiet", "--bare", f.remote, fork)
	f.upstreamGit("remote", "add", "fork", fork)
	f.pushUpstreamBranch("feature", "feature-1")
	f.upstreamGit("push", "--quiet", "fork", "main", "feature")
	f.localGit("remote", "add", "fork", fork)
	f.localGit("fetch", "--quiet", "fork")
	f.localGit("branch", "--quiet", "--track", "feature", "fork/feature")
	before := f.sha(f.local, "feature")
	f.upstreamGit("checkout", "--quiet", "feature")
	f.commit(f.upstream, "feature-2")
	f.upstreamGit("push", "--quiet", "fork", "feature")
	f.upstreamGit("checkout", "--quiet", "main")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	f.assertBranchAt("feature", before)

	res = f.sync("--remote=fork")
	if res.err != nil {
		t.Fatalf("run(--remote=fork) error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch feature (was %s).", before[:7]))
	f.assertBranchAt("feature", f.sha(f.upstream, "feature"))
}

func TestRunRemoteFlagUnknownRemote(t *testing.T) {
	f := newSyncFixture(t)
	before := f.sha(f.local, "main")
	f.advanceUpstream("main", "upstream-1")

	res := f.sync("--remote=nope")
	if res.err == nil {
		t.Fatal("run() succeeded, want an error for an unknown remote")
	}
	assertContains(t, "stderr", res.stderr, "fatal: no such remote 'nope'")
	f.assertBranchAt("main", before)
}

// TestRunExclude checks that branches matching an exclude pattern, from
// --exclude or gh-sync.exclude, are neither updated nor deleted, while other
// branches still are.
func TestRunExclude(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config []string
		flags  []string
	}{
		{name: "flag", flags: []string{"--exclude=wip/*", "--exclude=keep"}},
		{name: "config", config: []string{"wip/*", "keep"}},
		{name: "config and flag", config: []string{"wip/*"}, flags: []string{"--exclude=keep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSyncFixture(t)
			// wip/a would be fast-forwarded, keep would be deleted (merged
			// and gone), and other would be fast-forwarded.
			for _, b := range []string{"wip/a", "keep", "other"} {
				f.pushUpstreamBranch(b, b+"-1")
				f.trackRemoteBranch(b)
			}
			wipBefore := f.sha(f.local, "wip/a")
			keepBefore := f.sha(f.local, "keep")
			otherBefore := f.sha(f.local, "other")
			f.advanceUpstream("wip/a", "wip-2")
			f.advanceUpstream("other", "other-2")
			f.upstreamGit("merge", "--quiet", "--no-edit", "keep")
			f.upstreamGit("push", "--quiet", "origin", "main")
			f.deleteRemoteBranch("keep")
			for _, p := range tc.config {
				f.localGit("config", "--add", "gh-sync.exclude", p)
			}

			res := f.sync(append([]string{"--verbose"}, tc.flags...)...)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			f.assertBranchAt("wip/a", wipBefore)
			f.assertBranchAt("keep", keepBefore)
			assertContains(t, "stderr", res.stderr, "note: skipping 'wip/a' (excluded by 'wip/*')")
			assertContains(t, "stderr", res.stderr, "note: skipping 'keep' (excluded by 'keep')")
			assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch other (was %s).", otherBefore[:7]))
		})
	}
}

func TestRunExcludeInvalidPattern(t *testing.T) {
	f := newSyncFixture(t)
	before := f.sha(f.local, "main")
	f.advanceUpstream("main", "upstream-1")

	res := f.sync("--exclude=[")
	if res.err == nil {
		t.Fatal("run() succeeded, want an error for an invalid pattern")
	}
	assertContains(t, "stderr", res.stderr, `fatal: invalid exclude pattern "["`)
	f.assertBranchAt("main", before)
}

func TestMatchExclude(t *testing.T) {
	patterns := []string{"wip/*", "tmp-?"}
	for _, tc := range []struct {
		branch string
		want   string
	}{
		{"wip/foo", "wip/*"},
		{"wip/foo/bar", ""},
		{"wip", ""},
		{"tmp-1", "tmp-?"},
		{"tmp-12", ""},
		{"main", ""},
	} {
		got, ok := matchExclude(patterns, tc.branch)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("matchExclude(%q) = %q, %v; want %q", tc.branch, got, ok, tc.want)
		}
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

// ---------------------------------------------------------------------------
// Tests for issue #65: behavior promised by the README that was not pinned
// down by a test.
// ---------------------------------------------------------------------------

// assertBranchAt fails the test unless local branch exists and points at want.
func (f *syncFixture) assertBranchAt(branch, want string) {
	f.t.Helper()
	if !f.localBranchExists(branch) {
		f.t.Errorf("local branch %s was deleted", branch)
		return
	}
	if got := f.sha(f.local, branch); got != want {
		f.t.Errorf("local %s = %s, want unchanged %s", branch, got, want)
	}
}

// mergeIntoUpstreamMain fast-forwards main to branch on upstream and pushes
// it, as merging a pull request does.
func (f *syncFixture) mergeIntoUpstreamMain(branch string) {
	f.t.Helper()
	f.upstreamGit("merge", "--quiet", "--ff-only", branch)
	f.upstreamGit("push", "--quiet", "origin", "main")
}

// TestRunIgnoresBranchWithoutRemoteCounterpart checks the Safety promise that
// branches that never had an upstream are never deleted: a local-only branch
// (no upstream config, no same-named branch on the main remote) is left
// untouched and silent, even when it is fully merged into the default branch.
func TestRunIgnoresBranchWithoutRemoteCounterpart(t *testing.T) {
	f := newSyncFixture(t)
	// local-only points at main, so every commit is in origin/main and the
	// deletion path would consider it merged.
	f.localGit("branch", "--no-track", "local-only", "main")
	before := f.sha(f.local, "local-only")

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
	f.assertBranchAt("local-only", before)
}

// TestRunIgnoresBranchTrackingOtherRemote checks that a branch tracking a
// remote other than the main remote, with no same-named branch on the main
// remote, is neither updated nor deleted, even though its own upstream has
// moved and it is fully merged into the main remote's default branch.
func TestRunIgnoresBranchTrackingOtherRemote(t *testing.T) {
	f := newSyncFixture(t)
	// A second remote "fork" with a branch "topic" that exists only there.
	fork := filepath.Join(t.TempDir(), "fork.git")
	f.gitIn(f.upstream, "clone", "--quiet", "--bare", f.remote, fork)
	f.upstreamGit("remote", "add", "fork", fork)
	f.upstreamGit("push", "--quiet", "fork", "main:topic")
	f.localGit("remote", "add", "fork", fork)
	f.localGit("fetch", "--quiet", "fork")
	f.localGit("branch", "--quiet", "--track", "topic", "fork/topic")
	before := f.sha(f.local, "topic")
	// Move fork/topic forward, so a sync against the fork would fast-forward.
	f.upstreamGit("checkout", "--quiet", "-b", "topic")
	f.commit(f.upstream, "topic-1")
	f.upstreamGit("push", "--quiet", "fork", "topic")
	f.upstreamGit("checkout", "--quiet", "main")
	// gh-sync fetches only the main remote; fetch the fork here so the moved
	// fork/topic is visible locally.
	f.localGit("fetch", "--quiet", "fork")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertNotContains(t, "stdout", res.stdout, "topic")
	assertNotContains(t, "stderr", res.stderr, "topic")
	f.assertBranchAt("topic", before)
}

// TestRunSkipsBrokenUpstreamConfig checks that a branch whose
// branch.<name>.remote is the main remote but whose branch.<name>.merge is
// missing or invalid is skipped with a warning while the same-named remote
// branch still exists. It must neither be fast-forwarded (the remote branch
// moved) nor deleted (it is fully merged into the default branch).
func TestRunSkipsBrokenUpstreamConfig(t *testing.T) {
	tests := []struct {
		name        string
		breakConfig func(f *syncFixture)
	}{
		{"merge missing", func(f *syncFixture) {
			f.localGit("config", "--unset", "branch.feature.merge")
		}},
		{"merge invalid", func(f *syncFixture) {
			f.localGit("config", "branch.feature.merge", "not-a-ref")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("feature", "feature-1")
			f.trackRemoteBranch("feature")
			before := f.sha(f.local, "feature")
			// feature is merged into main, then moves on remotely: a working
			// config would fast-forward it, a "gone" verdict would delete it.
			f.mergeIntoUpstreamMain("feature")
			f.advanceUpstream("feature", "feature-2")
			tt.breakConfig(f)

			res := f.sync()
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			assertContains(t, "stderr", res.stderr, "warning: 'feature' upstream config is broken; skipping")
			assertNotContains(t, "stdout", res.stdout, "branch feature")
			f.assertBranchAt("feature", before)
		})
	}
}

// TestRunFastForwardsByNameWithoutUpstreamConfig checks that a branch with no
// upstream config is fast-forwarded to the same-named branch on the main
// remote.
func TestRunFastForwardsByNameWithoutUpstreamConfig(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.localGit("fetch", "--quiet", "origin")
	f.localGit("branch", "--no-track", "feature", "origin/feature")
	before := f.sha(f.local, "feature")
	f.advanceUpstream("feature", "feature-2")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch feature (was %s).", shortSHA(before)))
	if got, want := f.sha(f.local, "feature"), f.sha(f.upstream, "feature"); got != want {
		t.Errorf("local feature = %s, want %s", got, want)
	}
}

// TestRunDeleteFailures checks that when deleting a merged, gone branch
// fails, a warning is printed, run() returns an error and the branch is kept.
func TestRunDeleteFailures(t *testing.T) {
	// setup creates a merged, gone branch "feature" whose commit adds file f.
	setup := func(t *testing.T) (*syncFixture, string) {
		f := newSyncFixture(t)
		f.upstreamGit("checkout", "--quiet", "-b", "feature")
		if err := os.WriteFile(filepath.Join(f.upstream, "f"), []byte("feature\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.upstreamGit("add", "f")
		f.commit(f.upstream, "add f")
		f.upstreamGit("push", "--quiet", "-u", "origin", "feature")
		f.upstreamGit("checkout", "--quiet", "main")
		f.trackRemoteBranch("feature")
		f.mergeIntoUpstreamMain("feature")
		f.deleteRemoteBranch("feature")
		return f, f.sha(f.local, "feature")
	}

	// The checked-out branch is deleted only after switching to the default
	// branch. A local change to f (which local main does not have yet) makes
	// that checkout fail, so the branch must not be deleted.
	t.Run("checkout fails", func(t *testing.T) {
		f, before := setup(t)
		f.localGit("checkout", "--quiet", "feature")
		if err := os.WriteFile(filepath.Join(f.local, "f"), []byte("local change\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		res := f.sync()
		if res.err == nil {
			t.Fatal("run() error = nil, want an error")
		}
		assertContains(t, "stderr", res.stderr, "warning: couldn't checkout 'main':")
		assertContains(t, "stderr", res.stderr, "would be overwritten by checkout")
		assertNotContains(t, "stdout", res.stdout, "Switched to branch")
		assertNotContains(t, "stdout", res.stdout, "Deleted branch feature")
		f.assertBranchAt("feature", before)
		if f.currentBranch() != "feature" {
			t.Errorf("current branch = %q, want feature", f.currentBranch())
		}
	})

	// git refuses to delete a branch checked out in another worktree.
	t.Run("branch -D fails", func(t *testing.T) {
		f, before := setup(t)
		f.localGit("worktree", "add", "--quiet", filepath.Join(t.TempDir(), "wt"), "feature")

		res := f.sync()
		if res.err == nil {
			t.Fatal("run() error = nil, want an error")
		}
		assertContains(t, "stderr", res.stderr, "warning: couldn't delete 'feature':")
		assertNotContains(t, "stdout", res.stdout, "Deleted branch feature")
		f.assertBranchAt("feature", before)
	})
}

// TestRunSkipsUpstreamOutsideFetchRefspec checks that a branch whose upstream
// git has no remote-tracking ref for, because the upstream is not covered by
// remote.origin.fetch, is not treated as deleted on the remote: it is neither
// deleted nor warned about, with or without commits missing from the default
// branch.
func TestRunSkipsUpstreamOutsideFetchRefspec(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *syncFixture, branch string)
	}{
		// A single-branch (or shallow) clone fetches only main, so pushing
		// with -u sets branch.<name>.merge without creating
		// refs/remotes/origin/<name>.
		{"single-branch refspec", func(f *syncFixture, branch string) {
			f.localGit("config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
			f.localGit("push", "--quiet", "-u", "origin", branch)
			f.localGit("update-ref", "-d", "refs/remotes/origin/"+branch)
		}},
		// `gh pr checkout` of a pull request from a fork can track
		// refs/pull/<N>/head, which the default refspec doesn't fetch.
		{"pull request ref", func(f *syncFixture, branch string) {
			f.localGit("config", "branch."+branch+".remote", "origin")
			f.localGit("config", "branch."+branch+".merge", "refs/pull/1/head")
		}},
	}
	for _, tt := range tests {
		for _, withCommit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/withCommit=%v", tt.name, withCommit), func(t *testing.T) {
				f := newSyncFixture(t)
				f.localGit("checkout", "--quiet", "-b", "topic")
				if withCommit {
					f.commit(f.local, "wip")
				}
				before := f.sha(f.local, "topic")
				tt.setup(f, "topic")

				res := f.sync("--verbose")
				if res.err != nil {
					t.Fatalf("run() error: %v", res.err)
				}
				assertNotContains(t, "stderr", res.stderr, "warning:")
				assertContains(t, "stderr", res.stderr, "note: 'topic' has no remote-tracking ref for its upstream; skipping")
				assertNotContains(t, "stdout", res.stdout, "topic")
				assertNotContains(t, "stdout", res.stdout, "Switched to branch")
				f.assertBranchAt("topic", before)
				if f.currentBranch() != "topic" {
					t.Errorf("current branch = %q, want topic", f.currentBranch())
				}
			})
		}
	}
}
