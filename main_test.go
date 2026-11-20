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

// advanceUpstream checks out branch on upstream, adds commits, and pushes it
// to the remote.
func (f *syncFixture) advanceUpstream(branch string, commits ...string) {
	f.t.Helper()
	f.advanceOn("origin", branch, commits...)
}

// advanceOn is advanceUpstream for another remote of upstream, such as "fork".
func (f *syncFixture) advanceOn(remote, branch string, commits ...string) {
	f.t.Helper()
	f.upstreamGit("checkout", "--quiet", branch)
	for _, msg := range commits {
		f.commit(f.upstream, msg)
	}
	f.upstreamGit("push", "--quiet", remote, branch)
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

// addForkRemote adds a second remote "fork" to local (and upstream, which
// pushes to it), as in a fork setup where the main remote is the parent
// repository. gh-sync keeps using origin as the main remote. The fork starts
// as a copy of the remote.
func (f *syncFixture) addForkRemote() string {
	f.t.Helper()
	fork := filepath.Join(f.t.TempDir(), "fork.git")
	f.gitIn(f.upstream, "clone", "--quiet", "--bare", f.remote, fork)
	f.upstreamGit("remote", "add", "fork", fork)
	f.localGit("remote", "add", "fork", fork)
	return fork
}

// pushForkBranch creates branch on upstream from main with the given
// commits, pushes it to the fork only, and creates a local branch tracking
// fork/<branch>.
func (f *syncFixture) pushForkBranch(branch string, commits ...string) {
	f.t.Helper()
	f.upstreamGit("checkout", "--quiet", "-b", branch, "main")
	for _, msg := range commits {
		f.commit(f.upstream, msg)
	}
	f.upstreamGit("push", "--quiet", "fork", branch)
	f.upstreamGit("checkout", "--quiet", "main")
	f.localGit("fetch", "--quiet", "fork")
	f.localGit("branch", "--quiet", "--track", branch, "fork/"+branch)
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
// the same name as a tag (#57). `%(refname:short)` would list it as
// "heads/rel" and skip it; when it is checked out, `symbolic-ref --short HEAD`
// would also print "heads/rel", so `branch -f` would be used on the
// checked-out branch, which git refuses.
func TestRunFastForwardsBranchSharingNameWithTag(t *testing.T) {
	for _, checkedOut := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkedOut=%v", checkedOut), func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("rel", "rel-1")
			f.trackRemoteBranch("rel")
			f.localGit("tag", "rel", "rel")
			if checkedOut {
				f.localGit("switch", "--quiet", "rel")
			}
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
			if got := f.sha(f.local, "HEAD"); checkedOut && got != want {
				t.Errorf("local HEAD = %s, want %s", got, want)
			}
			if got := f.sha(f.local, "refs/tags/rel"); got != before {
				t.Errorf("tag rel moved to %s, want it left at %s", got, before)
			}
		})
	}
}

// TestReadRefsDetachedHead checks that a detached HEAD is not listed as a
// branch, as `git branch` would with "(HEAD detached at ...)" (#57).
func TestReadRefsDetachedHead(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.trackRemoteBranch("feature")
	f.localGit("checkout", "--quiet", "--detach", "main")

	refs, err := readRefs([]string{"origin"})
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

	refs, err := readRefs([]string{"origin"})
	if err != nil {
		t.Fatalf("readRefs() error: %v", err)
	}
	got := make(map[string]localBranch)
	for _, b := range refs.branches {
		got[b.name] = b
	}
	want := map[string]localBranch{
		"main":      {name: "main", upstream: "refs/remotes/origin/main", worktreePath: f.localGit("rev-parse", "--show-toplevel")},
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
			f.mergeIntoUpstreamMain("feature")
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

	f.mergeIntoUpstreamMain("feature")
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
			assertNotContains(t, "stderr", res.stderr, "was deleted on origin")

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
	assertContains(t, "stderr", res.stderr, "warning: 'feature-c' was deleted on origin, but 1 commit is not in 'main' and no merged pull request matches it; not deleted")
	// feature-c has no merged pull request and must be kept.
	f.assertBranchAt("feature-c", f.sha(f.upstream, "feature-c"))
}

// TestRunDeletesBranchBehindMergedPR checks that a branch whose tip is one of
// a merged pull request's commits, but not its head, is deleted: commits were
// pushed to the pull request from elsewhere before it was merged, and the
// local branch never fetched them.
func TestRunDeletesBranchBehindMergedPR(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dryRun=%v", dryRun), func(t *testing.T) {
			f := newSyncFixture(t)
			base := f.sha(f.upstream, "main")
			f.pushUpstreamBranch("feature", "feature-1")
			f.trackRemoteBranch("feature")
			tip := f.sha(f.local, "feature")
			f.advanceUpstream("feature", "feature-2")
			head := f.sha(f.upstream, "feature")
			f.squashMergeAndDelete("feature")

			var calls []prLookup
			// The head of #4 is an ancestor of the tip, which alone would
			// mean "commits added after the merge"; #5 containing the tip
			// takes precedence.
			f.findMergedPRs = stubMergedPRs(map[string][]mergedPR{
				"feature": {
					{Number: 4, HeadRefOid: base, Commits: []string{base}},
					{Number: 5, HeadRefOid: head, Commits: []string{tip, head}},
				},
			}, &calls)
			var args []string
			if dryRun {
				args = append(args, "--dry-run")
			}
			res := f.sync(args...)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}

			assertNotContains(t, "stderr", res.stderr, "warning:")
			if dryRun {
				assertContains(t, "stdout", res.stdout, fmt.Sprintf("Would delete branch feature (was %s, merged via #5).", tip[:7]))
				f.assertBranchAt("feature", tip)
				return
			}
			assertContains(t, "stdout", res.stdout, fmt.Sprintf("Deleted branch feature (was %s, merged via #5).", tip[:7]))
			if f.localBranchExists("feature") {
				t.Error("local branch feature still exists")
			}
		})
	}
}

// TestRunKeepsBranchWithCommitsAddedAfterMerge checks that a branch with
// commits on top of a merged pull request's head is kept, and that the
// warning names the pull request and counts the added commits.
func TestRunKeepsBranchWithCommitsAddedAfterMerge(t *testing.T) {
	tests := []struct {
		name  string
		added []string // commits made locally after the merge
		want  string
	}{
		{"one commit", []string{"local-1"}, "1 commit was added after it was merged via #7"},
		{"two commits", []string{"local-1", "local-2"}, "2 commits were added after it was merged via #7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("feature", "feature-1")
			f.trackRemoteBranch("feature")
			head := f.sha(f.local, "feature")
			f.squashMergeAndDelete("feature")
			f.localGit("checkout", "--quiet", "feature")
			for _, msg := range tt.added {
				f.commit(f.local, msg)
			}
			tip := f.sha(f.local, "feature")

			var calls []prLookup
			f.findMergedPRs = stubMergedPRs(map[string][]mergedPR{
				"feature": {{Number: 7, HeadRefOid: head, Commits: []string{head}}},
			}, &calls)
			res := f.sync()
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}

			assertContains(t, "stderr", res.stderr, "warning: 'feature' was deleted on origin, but "+tt.want+"; not deleted\n")
			assertNotContains(t, "stdout", res.stdout, "branch feature")
			f.assertBranchAt("feature", tip)
			if f.currentBranch() != "feature" {
				t.Errorf("current branch = %q, want feature", f.currentBranch())
			}
		})
	}
}

// TestRunKeepsBranchWithoutMatchingPR checks that a branch is kept when no
// merged pull request contains its tip or has a head it descends from, and
// that the warning counts the commits missing from the default branch.
func TestRunKeepsBranchWithoutMatchingPR(t *testing.T) {
	// A commit that does not exist locally, so it can't be an ancestor.
	unknown := strings.Repeat("0", 40)
	tests := []struct {
		name    string
		commits []string   // commits on "feature"
		prs     []mergedPR // merged pull requests for "feature"
		want    string
	}{
		{"no merged pull request", []string{"feature-1"}, nil, "1 commit is not in 'main'"},
		{"several commits", []string{"feature-1", "feature-2", "feature-3"}, nil, "3 commits are not in 'main'"},
		{"unrelated pull request", []string{"feature-1"}, []mergedPR{{Number: 7, HeadRefOid: unknown, Commits: []string{unknown}}}, "1 commit is not in 'main'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("feature", tt.commits...)
			f.trackRemoteBranch("feature")
			tip := f.sha(f.local, "feature")
			f.squashMergeAndDelete("feature")
			var calls []prLookup
			f.findMergedPRs = stubMergedPRs(map[string][]mergedPR{"feature": tt.prs}, &calls)

			res := f.sync()
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			assertContains(t, "stderr", res.stderr, "warning: 'feature' was deleted on origin, but "+tt.want+" and no merged pull request matches it; not deleted\n")
			assertNotContains(t, "stderr", res.stderr, "couldn't look up")
			assertNotContains(t, "stdout", res.stdout, "branch feature")
			f.assertBranchAt("feature", tip)
		})
	}
}

// TestRunKeepsBranchWhenPRLookupFails checks that a failed lookup keeps the
// branches with a warning each, and that the reason is printed as a warning
// before them: once, even when the branches span several requests that fail
// the same way.
func TestRunKeepsBranchWhenPRLookupFails(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	tip := f.sha(f.upstream, "feature")
	// One more remote branch than fits in a single request.
	var branches, pushSpecs, deleteSpecs []string
	for i := range mergedPRChunkSize + 1 {
		branch := fmt.Sprintf("topic-%02d", i)
		branches = append(branches, branch)
		pushSpecs = append(pushSpecs, "feature:refs/heads/"+branch)
		deleteSpecs = append(deleteSpecs, ":refs/heads/"+branch)
	}
	f.upstreamGit(append([]string{"push", "--quiet", "origin"}, pushSpecs...)...)
	f.localGit("fetch", "--quiet", "origin")
	for _, branch := range branches {
		f.localGit("branch", "--quiet", "--track", branch, "origin/"+branch)
	}
	f.upstreamGit(append([]string{"push", "--quiet", "origin"}, deleteSpecs...)...)

	calls := 0
	f.findMergedPRs = func(runConfig, Remote, string, []string) (map[string][]mergedPR, error) {
		calls++
		return nil, errors.New("gh: not logged in")
	}
	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}

	if calls != 2 {
		t.Errorf("findMergedPRs calls = %d, want 2", calls)
	}
	reason := "warning: couldn't look up merged pull requests: gh: not logged in\n"
	if got := strings.Count(res.stderr, reason); got != 1 {
		t.Errorf("stderr contains the lookup failure reason %d times, want 1:\n%s", got, res.stderr)
	}
	for _, branch := range branches {
		kept := fmt.Sprintf("warning: '%s' was deleted on origin, but 1 commit is not in 'main' and pull requests couldn't be looked up; not deleted\n", branch)
		assertContains(t, "stderr", res.stderr, kept)
		if strings.Index(res.stderr, reason) > strings.Index(res.stderr, kept) {
			t.Errorf("the lookup failure reason is printed after the warning for %s", branch)
		}
		f.assertBranchAt(branch, tip)
	}
	assertNotContains(t, "stdout", res.stdout, "branch topic-")
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
		"nodes { number headRefOid commits(last: 100) { nodes { commit { oid } } } }",
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
		"b0":{"nodes":[
			{"number":12,"headRefOid":"aaaa","commits":{"nodes":[{"commit":{"oid":"cccc"}},{"commit":{"oid":"aaaa"}}]}},
			{"number":3,"headRefOid":"bbbb","commits":{"nodes":[]}}]},
		"b1":{"nodes":[]}}}}`), heads)
	if err != nil {
		t.Fatalf("parseMergedPRResponse() error: %v", err)
	}
	want := map[string][]mergedPR{
		"feature": {{Number: 12, HeadRefOid: "aaaa", Commits: []string{"cccc", "aaaa"}}, {Number: 3, HeadRefOid: "bbbb"}},
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

func TestSSHResolvedHost(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		output string
		err    error
		want   string
	}{
		{"alias", "github-work", "user git\nhostname github.com\nport 22\n", nil, "github.com"},
		{"enterprise alias", "ghe", "hostname ghe.example.com\n", nil, "ghe.example.com"},
		{"port 443", "ssh.github.com", "hostname ssh.github.com\nport 443\n", nil, "github.com"},
		{"alias to port 443", "gh443", "hostname ssh.github.com\n", nil, "github.com"},
		{"ssh failed", "github-work", "", errors.New("exec: \"ssh\": executable file not found in $PATH"), "github-work"},
		{"ssh failed for ssh.github.com", "ssh.github.com", "", errors.New("exit status 255"), "github.com"},
		{"no hostname line", "github.com", "user git\n", nil, "github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshResolvedHost(tt.host, []byte(tt.output), tt.err); got != tt.want {
				t.Errorf("sshResolvedHost(%q, %q, %v) = %q, want %q", tt.host, tt.output, tt.err, got, tt.want)
			}
		})
	}
}

func TestResolveGitHubHostWithoutSSH(t *testing.T) {
	// An empty PATH makes any ssh call fail: a non-SSH remote keeps its host,
	// a cached host is reused, and a failed lookup keeps the URL's host.
	t.Setenv("PATH", "")
	cfg := runConfig{sshHosts: map[string]string{"github-work": "github.com"}}
	tests := []struct {
		url, host, want string
	}{
		{"https://ssh.github.com/org/repo.git", "ssh.github.com", "ssh.github.com"},
		{"github-work:org/repo.git", "github-work", "github.com"},                     // cached
		{"ssh://git@ssh.github.com:443/org/repo.git", "ssh.github.com", "github.com"}, // ssh fails, mapped
		{"-oProxyCommand=x:org/repo.git", "-oProxyCommand=x", "-oProxyCommand=x"},
	}
	for _, tt := range tests {
		if got := resolveGitHubHost(cfg, tt.url, tt.host); got != tt.want {
			t.Errorf("resolveGitHubHost(%q, %q) = %q, want %q", tt.url, tt.host, got, tt.want)
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
	f.mergeIntoUpstreamMain("merged")
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

// TestRunWarnsOnUnpushedCommits checks that a branch with commits its
// upstream doesn't have is kept, with a warning that names the upstream and
// tells ahead from diverged.
func TestRunWarnsOnUnpushedCommits(t *testing.T) {
	tests := []struct {
		name     string
		local    []string // commits added locally
		upstream []string // commits added on the remote
		want     string
	}{
		{"ahead by one", []string{"local-1"}, nil, "warning: 'main' has 1 commit not in 'origin/main'; not updated\n"},
		{"ahead by two", []string{"local-1", "local-2"}, nil, "warning: 'main' has 2 commits not in 'origin/main'; not updated\n"},
		{"diverged", []string{"local-1", "local-2"}, []string{"upstream-1"}, "warning: 'main' has diverged from 'origin/main' (2 ahead, 1 behind); not updated\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncFixture(t)
			for _, msg := range tt.local {
				f.commit(f.local, msg)
			}
			if len(tt.upstream) > 0 {
				f.advanceUpstream("main", tt.upstream...)
			}
			before := f.sha(f.local, "main")

			res := f.sync()
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			if res.stderr != tt.want {
				t.Errorf("stderr = %q, want %q", res.stderr, tt.want)
			}
			assertNotContains(t, "stdout", res.stdout, "Updated branch main")
			if got := f.sha(f.local, "main"); got != before {
				t.Errorf("local main = %s, want unchanged %s", got, before)
			}
		})
	}
}

// TestRunWarnsOnUnpushedCommitsMatchedByName checks that for a branch without
// upstream config the warning names the same-named branch on the main remote,
// which is what it was compared against.
func TestRunWarnsOnUnpushedCommitsMatchedByName(t *testing.T) {
	f := newSyncFixture(t)
	f.pushUpstreamBranch("feature", "feature-1")
	f.localGit("fetch", "--quiet", "origin")
	f.localGit("checkout", "--quiet", "--no-track", "-b", "feature", "origin/feature")
	f.commit(f.local, "local-1")
	f.localGit("checkout", "--quiet", "main")
	f.advanceUpstream("feature", "feature-2", "feature-3")
	before := f.sha(f.local, "feature")

	res := f.sync()
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	want := "warning: 'feature' has diverged from 'origin/feature' (1 ahead, 2 behind); not updated\n"
	if res.stderr != want {
		t.Errorf("stderr = %q, want %q", res.stderr, want)
	}
	f.assertBranchAt("feature", before)
}

// TestRunColorsWarnings checks that warnings are yellow when stderr is
// colorized and plain otherwise, like errors are light red.
func TestRunColorsWarnings(t *testing.T) {
	tests := []struct {
		color string
		want  string
	}{
		{"always", "\033[33mwarning: 'main' has 1 commit not in 'origin/main'; not updated\033[0m\n"},
		{"never", "warning: 'main' has 1 commit not in 'origin/main'; not updated\n"},
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

// TestRunUpToDateIsNoop checks that up-to-date branches, and a local-only
// branch (no upstream, no same-named remote branch) that is fully merged, are
// left alone silently: branches that never had an upstream are never deleted.
// "Already up to date." is shown only when stderr is a terminal, which it is
// not here.
func TestRunUpToDateIsNoop(t *testing.T) {
	for _, args := range [][]string{nil, {"--dry-run"}} {
		t.Run(strings.Join(append([]string{"args"}, args...), " "), func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushUpstreamBranch("feature", "feature-1")
			f.trackRemoteBranch("feature")
			f.localGit("branch", "--no-track", "local-only", "main")
			before := make(map[string]string)
			for _, b := range []string{"main", "feature", "local-only"} {
				before[b] = f.sha(f.local, b)
			}

			res := f.sync(args...)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want empty", res.stdout)
			}
			if res.stderr != "" {
				t.Errorf("stderr = %q, want empty", res.stderr)
			}
			for b, sha := range before {
				f.assertBranchAt(b, sha)
			}
		})
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
			f.mergeIntoUpstreamMain("merged")
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
	// feature moves on in fork, which ranks below origin, so only
	// --remote=fork selects it.
	f.addForkRemote()
	f.pushUpstreamBranch("feature", "feature-1")
	f.upstreamGit("push", "--quiet", "fork", "feature")
	f.localGit("fetch", "--quiet", "fork")
	// Without upstream config, feature is matched by name against the main
	// remote. (A branch tracking fork would be synced against fork/feature
	// whichever remote is the main one.)
	f.localGit("branch", "--quiet", "--no-track", "feature", "fork/feature")
	before := f.sha(f.local, "feature")
	f.advanceOn("fork", "feature", "feature-2")

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

// TestRunRejectsInvalidOptions checks that an invalid option value stops
// gh-sync before anything is synced.
func TestRunRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		arg     string
		wantErr string
	}{
		{"--remote=nope", "fatal: no such remote 'nope'"},
		{"--exclude=[", `fatal: invalid exclude pattern "["`},
		{"--color=sometimes", `fatal: invalid --color value: "sometimes"`},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			f := newSyncFixture(t)
			before := f.sha(f.local, "main")
			f.advanceUpstream("main", "upstream-1")

			res := f.sync(tt.arg)
			if res.err == nil {
				t.Fatal("run() error = nil, want an error")
			}
			assertContains(t, "stderr", res.stderr, tt.wantErr)
			f.assertBranchAt("main", before)
		})
	}
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
	assertContains(t, "stdout", res.stdout, "--remove-worktrees")
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

	f.mergeIntoUpstreamMain("feature")
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

// TestRunPartialClone checks that a sync completes in a partial clone (#56).
func TestRunPartialClone(t *testing.T) {
	f := newSyncFixture(t)
	// Mark origin as a promisor remote, as `git clone --filter=blob:none`
	// does: `git remote -v` then prints "origin\t<url> (fetch) [blob:none]".
	f.localGit("config", "remote.origin.promisor", "true")
	f.localGit("config", "remote.origin.partialclonefilter", "blob:none")
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

// TestRunSyncsBranchTrackingOtherRemote checks that a branch tracking a
// remote other than the main remote (a fork's origin when the main remote is
// upstream) is synced against its own upstream.
func TestRunSyncsBranchTrackingOtherRemote(t *testing.T) {
	t.Run("fast-forward", func(t *testing.T) {
		f := newSyncFixture(t)
		f.addForkRemote()
		f.pushForkBranch("topic", "topic-1")
		before := f.sha(f.local, "topic")
		// The fork's topic moves on; local has not fetched it yet.
		f.advanceOn("fork", "topic", "topic-2")

		res := f.sync()
		if res.err != nil {
			t.Fatalf("run() error: %v", res.err)
		}
		assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch topic (was %s).", shortSHA(before)))
		if got, want := f.sha(f.local, "topic"), f.sha(f.upstream, "topic"); got != want {
			t.Errorf("local topic = %s, want %s", got, want)
		}
	})

	t.Run("merged and deleted on the fork", func(t *testing.T) {
		f := newSyncFixture(t)
		f.addForkRemote()
		f.pushForkBranch("topic", "topic-1")
		before := f.sha(f.local, "topic")
		// Merged into the main remote's default branch, then deleted on the
		// fork, as after a merged pull request from the fork.
		f.mergeIntoUpstreamMain("topic")
		f.upstreamGit("push", "--quiet", "fork", "--delete", "topic")

		res := f.sync()
		if res.err != nil {
			t.Fatalf("run() error: %v", res.err)
		}
		assertContains(t, "stdout", res.stdout, fmt.Sprintf("Deleted branch topic (was %s).", shortSHA(before)))
		if f.localBranchExists("topic") {
			t.Error("local branch topic still exists")
		}
	})

	t.Run("unmerged and deleted on the fork", func(t *testing.T) {
		f := newSyncFixture(t)
		f.addForkRemote()
		f.pushForkBranch("topic", "topic-1")
		before := f.sha(f.local, "topic")
		f.upstreamGit("push", "--quiet", "fork", "--delete", "topic")

		res := f.sync()
		if res.err != nil {
			t.Fatalf("run() error: %v", res.err)
		}
		assertContains(t, "stderr", res.stderr, "warning: 'topic' was deleted on fork, but 1 commit is not in 'main' and pull requests couldn't be looked up; not deleted")
		f.assertBranchAt("topic", before)
	})

	// A same-named branch on the main remote is not used for a branch that
	// tracks another remote: its own upstream decides.
	t.Run("same-named branch on the main remote", func(t *testing.T) {
		f := newSyncFixture(t)
		f.addForkRemote()
		f.pushForkBranch("topic", "topic-1")
		before := f.sha(f.local, "topic")
		f.pushUpstreamBranch("other", "other-1")
		f.upstreamGit("push", "--quiet", "origin", "other:topic")

		res := f.sync()
		if res.err != nil {
			t.Fatalf("run() error: %v", res.err)
		}
		assertNotContains(t, "stdout", res.stdout, "topic")
		assertNotContains(t, "stderr", res.stderr, "topic")
		f.assertBranchAt("topic", before)
	})

	// When the other remote can't be fetched, its branches are skipped with
	// a warning and the exit status is 1; the main remote's branches are
	// still synced.
	t.Run("fetch fails", func(t *testing.T) {
		f := newSyncFixture(t)
		fork := f.addForkRemote()
		f.pushForkBranch("topic", "topic-1")
		before := f.sha(f.local, "topic")
		// Deleted on the fork and merged: a successful fetch would delete it.
		f.mergeIntoUpstreamMain("topic")
		if err := os.RemoveAll(fork); err != nil {
			t.Fatal(err)
		}
		f.pushUpstreamBranch("feature", "feature-1")
		f.trackRemoteBranch("feature")
		featureBefore := f.sha(f.local, "feature")
		f.advanceUpstream("feature", "feature-2")

		res := f.sync()
		if res.err == nil {
			t.Fatal("run() error = nil, want an error")
		}
		assertContains(t, "stderr", res.stderr, "warning: couldn't fetch fork; skipping the branches that track it:")
		assertContains(t, "stdout", res.stdout, fmt.Sprintf("Updated branch feature (was %s).", shortSHA(featureBefore)))
		assertNotContains(t, "stdout", res.stdout, "topic")
		f.assertBranchAt("topic", before)
	})
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

// TestRunKeepsBranchWhenCheckoutFails checks that a checked-out merged, gone
// branch is kept, with a warning and an error exit, when switching to the
// default branch first fails: here a local change to f, which local main
// does not have yet, blocks the checkout.
func TestRunKeepsBranchWhenCheckoutFails(t *testing.T) {
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
	before := f.sha(f.local, "feature")
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
}

// TestRunSkipsBranchesInOtherWorktree checks that a behind branch, a merged
// gone branch and a squash-merged gone branch, each checked out in another
// worktree, are kept, are not counted as failures, and are reported the same
// way with --dry-run: one warning listing the branches that were not deleted
// with their worktrees, and one note counting those that were not updated.
// The worktrees are outside the main worktree, so their paths are absolute.
func TestRunSkipsBranchesInOtherWorktree(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dryRun=%v", dryRun), func(t *testing.T) {
			f := newSyncFixture(t)
			// behind: the remote branch moves on.
			f.pushUpstreamBranch("behind", "behind-1")
			f.trackRemoteBranch("behind")
			f.advanceUpstream("behind", "behind-2")
			// merged: fast-forward merged into main, then deleted.
			f.pushUpstreamBranch("merged", "merged-1")
			f.trackRemoteBranch("merged")
			f.mergeIntoUpstreamMain("merged")
			f.deleteRemoteBranch("merged")
			// squashed: squash-merged via pull request #7, then deleted.
			f.pushUpstreamBranch("squashed", "squashed-1")
			f.trackRemoteBranch("squashed")
			squashedSHA := f.sha(f.local, "squashed")
			f.squashMergeAndDelete("squashed")
			var calls []prLookup
			f.findMergedPRs = stubMergedPRs(map[string][]mergedPR{
				"squashed": {{Number: 7, HeadRefOid: squashedSHA}},
			}, &calls)

			before := make(map[string]string)
			paths := make(map[string]string)
			for _, branch := range []string{"behind", "merged", "squashed"} {
				before[branch] = f.sha(f.local, branch)
				paths[branch] = filepath.Join(t.TempDir(), branch)
				f.localGit("worktree", "add", "--quiet", paths[branch], branch)
				// git reports the resolved path (e.g. /private/var on macOS).
				paths[branch] = f.gitIn(paths[branch], "rev-parse", "--show-toplevel")
			}

			var args []string
			if dryRun {
				args = append(args, "--dry-run")
			}
			res := f.sync(args...)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			want := "warning: 2 merged branches are still checked out in other worktrees; not deleted:\n" +
				"  merged     " + paths["merged"] + "\n" +
				"  squashed   " + paths["squashed"] + "\n" +
				worktreeHint +
				"note: 1 branch checked out in another worktree is behind its upstream; not updated\n"
			if res.stderr != want {
				t.Errorf("stderr = %q, want %q", res.stderr, want)
			}
			for branch, sha := range before {
				assertNotContains(t, "stdout", res.stdout, "branch "+branch)
				f.assertBranchAt(branch, sha)
			}
		})
	}
}

const worktreeHint = "hint: remove a worktree with `git worktree remove <path>`, or run `gh sync --remove-worktrees` to remove the clean ones\n"

// addWorktree checks branch out in a new worktree at path and returns the
// path as git reports it (resolved, e.g. /private/var on macOS).
func (f *syncFixture) addWorktree(path, branch string) string {
	f.t.Helper()
	f.localGit("worktree", "add", "--quiet", path, branch)
	return f.gitIn(path, "rev-parse", "--show-toplevel")
}

// pushMergedGoneBranch makes branch a local branch that was merged into main
// and deleted on the remote.
func (f *syncFixture) pushMergedGoneBranch(branch string) {
	f.t.Helper()
	f.pushUpstreamBranch(branch, branch+"-1")
	f.trackRemoteBranch(branch)
	f.mergeIntoUpstreamMain(branch)
	f.deleteRemoteBranch(branch)
}

// pushBehindBranch makes branch a local branch that is one commit behind its
// upstream.
func (f *syncFixture) pushBehindBranch(branch string) {
	f.t.Helper()
	f.pushUpstreamBranch(branch, branch+"-1")
	f.trackRemoteBranch(branch)
	f.advanceUpstream(branch, branch+"-2")
}

// TestRunWorktreeSummaryWording checks the other grammatical number of both
// worktree summaries (one branch not deleted, several not updated), that a
// worktree inside the main worktree is shown relative to it while one outside
// keeps its absolute path, and that the branches behind are only listed with
// --verbose.
func TestRunWorktreeSummaryWording(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		t.Run(fmt.Sprintf("verbose=%v", verbose), func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushMergedGoneBranch("merged")
			f.pushBehindBranch("behind")
			f.pushBehindBranch("behind-longer")
			f.addWorktree(filepath.Join(f.local, ".worktrees", "merged"), "merged")
			f.addWorktree(filepath.Join(f.local, ".worktrees", "behind"), "behind")
			outside := f.addWorktree(filepath.Join(t.TempDir(), "outside"), "behind-longer")

			var args []string
			if verbose {
				args = append(args, "--verbose")
			}
			res := f.sync(args...)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			warning := "warning: 1 merged branch is still checked out in another worktree; not deleted:\n" +
				"  merged   " + filepath.Join(".worktrees", "merged") + "\n" +
				worktreeHint
			note := "note: 2 branches checked out in other worktrees are behind their upstream; not updated\n"
			list := "  behind          " + filepath.Join(".worktrees", "behind") + "\n" +
				"  behind-longer   " + outside + "\n"
			if verbose {
				assertContains(t, "stderr", res.stderr, warning+note+list)
			} else if res.stderr != warning+note {
				t.Errorf("stderr = %q, want %q", res.stderr, warning+note)
			}
			// Only main, which received the merge, is updated.
			for _, branch := range []string{"merged", "behind", "behind-longer"} {
				assertNotContains(t, "stdout", res.stdout, "branch "+branch)
			}
		})
	}
}

// TestRunWorktreeSummaryFromLinkedWorktree checks that paths are relative to
// the main worktree, not to the worktree gh-sync runs in, and that a branch
// checked out in the main worktree itself is shown with its absolute path.
func TestRunWorktreeSummaryFromLinkedWorktree(t *testing.T) {
	f := newSyncFixture(t)
	f.pushBehindBranch("behind")
	f.advanceUpstream("main", "main-2")
	f.addWorktree(filepath.Join(f.local, ".worktrees", "behind"), "behind")
	f.localGit("branch", "--quiet", "other", "main")
	linked := f.addWorktree(filepath.Join(t.TempDir(), "linked"), "other")
	mainWorktree := f.localGit("rev-parse", "--show-toplevel")
	t.Chdir(linked)

	res := f.sync("--verbose")
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	assertContains(t, "stderr", res.stderr, "note: 2 branches checked out in other worktrees are behind their upstream; not updated\n"+
		"  behind   "+filepath.Join(".worktrees", "behind")+"\n"+
		"  main     "+mainWorktree+"\n")
}

// TestRunRemoveWorktrees checks that --remove-worktrees removes the clean
// worktrees of a merged branch and of a squash-merged branch and then deletes
// the branches, printing each removal right before its deletion, with the
// path relative to the main worktree when the worktree is inside it.
func TestRunRemoveWorktrees(t *testing.T) {
	f := newSyncFixture(t)
	mainBefore := f.sha(f.local, "main")
	f.pushMergedGoneBranch("merged")
	f.pushUpstreamBranch("squashed", "squashed-1")
	f.trackRemoteBranch("squashed")
	squashedSHA := f.sha(f.local, "squashed")
	f.squashMergeAndDelete("squashed")
	var calls []prLookup
	f.findMergedPRs = stubMergedPRs(map[string][]mergedPR{
		"squashed": {{Number: 7, HeadRefOid: squashedSHA}},
	}, &calls)
	mergedSHA := f.sha(f.local, "merged")
	inside := f.addWorktree(filepath.Join(f.local, ".worktrees", "merged"), "merged")
	outside := f.addWorktree(filepath.Join(t.TempDir(), "squashed"), "squashed")

	res := f.sync("--remove-worktrees")
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	want := fmt.Sprintf("Updated branch main (was %s).\n", mainBefore[:7]) +
		"Removed worktree " + filepath.Join(".worktrees", "merged") + ".\n" +
		fmt.Sprintf("Deleted branch merged (was %s).\n", mergedSHA[:7]) +
		"Removed worktree " + outside + ".\n" +
		fmt.Sprintf("Deleted branch squashed (was %s, merged via #7).\n", squashedSHA[:7])
	if res.stdout != want {
		t.Errorf("stdout = %q, want %q", res.stdout, want)
	}
	if res.stderr != "" {
		t.Errorf("stderr = %q, want empty", res.stderr)
	}
	for _, branch := range []string{"merged", "squashed"} {
		if f.localBranchExists(branch) {
			t.Errorf("local branch %s still exists", branch)
		}
	}
	for _, path := range []string{inside, outside} {
		if dirExists(path) {
			t.Errorf("worktree %s still exists", path)
		}
	}
}

// dirExists reports whether path exists.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeFile creates the file name in dir, containing the line "content".
func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunRemoveWorktreesByState checks what --remove-worktrees does with a
// worktree in each state, and that --dry-run predicts the same outcome
// without changing anything. A worktree git refuses to remove is kept with
// its branch and one warning; that is a skip, so the exit status stays 0 and
// the summary of branches not deleted is not printed as well.
//
// The "ignored file" case documents the risk of the option: git removes a
// worktree whose only extra files are ignored ones, and they are lost.
func TestRunRemoveWorktreesByState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(f *syncFixture, path string)
		// refusal is the reason in the warning, or "" when the worktree is
		// removed.
		refusal string
	}{
		{name: "clean", prepare: func(*syncFixture, string) {}},
		{
			name:    "untracked file",
			prepare: func(f *syncFixture, path string) { writeFile(f.t, path, "notes.txt") },
			refusal: "has modified or untracked files",
		},
		{
			name: "staged file",
			prepare: func(f *syncFixture, path string) {
				writeFile(f.t, path, "notes.txt")
				f.gitIn(path, "add", "notes.txt")
			},
			refusal: "has modified or untracked files",
		},
		{
			name:    "locked",
			prepare: func(f *syncFixture, path string) { f.localGit("worktree", "lock", "--reason", "in use", path) },
			refusal: "is locked",
		},
		{
			name: "ignored file",
			prepare: func(f *syncFixture, path string) {
				// The excludes file ignores a file named "content", which is
				// what writeFile writes into every file.
				f.localGit("config", "core.excludesFile", filepath.Join(f.local, ".git", "ignore"))
				writeFile(f.t, filepath.Join(f.local, ".git"), "ignore")
				writeFile(f.t, path, "content")
			},
		},
	} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dryRun=%v", tc.name, dryRun), func(t *testing.T) {
				f := newSyncFixture(t)
				f.pushMergedGoneBranch("merged")
				// main is brought up to date first, so only the worktree and
				// its branch are left to report.
				f.localGit("pull", "--quiet", "--ff-only")
				sha := f.sha(f.local, "merged")
				path := f.addWorktree(filepath.Join(f.local, ".worktrees", "merged"), "merged")
				shown := filepath.Join(".worktrees", "merged")
				tc.prepare(f, path)

				args := []string{"--remove-worktrees"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				res := f.sync(args...)
				if res.err != nil {
					t.Fatalf("run() error: %v", res.err)
				}

				wantStdout, wantStderr := "", ""
				switch {
				case tc.refusal != "":
					wantStderr = fmt.Sprintf("warning: 'merged' is checked out in %s, which %s; not deleted\n", shown, tc.refusal)
				case dryRun:
					wantStdout = fmt.Sprintf("Would remove worktree %s.\nWould delete branch merged (was %s).\n", shown, sha[:7])
				default:
					wantStdout = fmt.Sprintf("Removed worktree %s.\nDeleted branch merged (was %s).\n", shown, sha[:7])
				}
				if res.stdout != wantStdout {
					t.Errorf("stdout = %q, want %q", res.stdout, wantStdout)
				}
				if res.stderr != wantStderr {
					t.Errorf("stderr = %q, want %q", res.stderr, wantStderr)
				}

				removed := tc.refusal == "" && !dryRun
				if dirExists(path) == removed {
					t.Errorf("worktree exists = %v, want %v", !removed, removed)
				}
				if removed {
					if f.localBranchExists("merged") {
						t.Error("local branch merged still exists")
					}
				} else {
					f.assertBranchAt("merged", sha)
				}
			})
		}
	}
}

// TestRemoveRefusal checks how git's refusals are worded in the warning: the
// two expected ones in gh-sync's words, anything else as the first line of
// git's message, and never with git's advice to use --force.
func TestRemoveRefusal(t *testing.T) {
	exit := errors.New("exit status 128")
	for _, tc := range []struct {
		name   string
		stderr string
		want   string
	}{
		{"dirty", "fatal: '/wt' contains modified or untracked files, use --force to delete it\n", "has modified or untracked files"},
		{"locked", "fatal: cannot remove a locked working tree;\nuse 'remove -f -f' to override or unlock first\n", "is locked"},
		{"locked with a reason", "fatal: cannot remove a locked working tree, lock reason: in use;\nuse 'remove -f -f' to override or unlock first\n", "is locked"},
		{"submodules", "fatal: working trees containing submodules cannot be moved or removed\n", "couldn't be removed: working trees containing submodules cannot be moved or removed"},
		{"other advice to force", "fatal: something is in the way, use --force to delete it\n", "couldn't be removed: something is in the way"},
		{"no message", "", "couldn't be removed: exit status 128"},
	} {
		if got := removeRefusal(tc.stderr, exit); got != tc.want {
			t.Errorf("%s: removeRefusal() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRunRemoveWorktreesSetting checks where the setting comes from: the
// gh-sync.removeWorktrees config, overridden in both directions by the flag
// when it is given, and off by default.
func TestRunRemoveWorktreesSetting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		args   []string
		remove bool
	}{
		{name: "default"},
		{name: "config true", config: "true", remove: true},
		{name: "config yes", config: "yes", remove: true},
		{name: "config false", config: "false"},
		{name: "flag", args: []string{"--remove-worktrees"}, remove: true},
		{name: "flag false overrides config", config: "true", args: []string{"--remove-worktrees=false"}},
		{name: "flag overrides config", config: "false", args: []string{"--remove-worktrees"}, remove: true},
		{name: "flag overrides invalid config", config: "maybe", args: []string{"--remove-worktrees=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSyncFixture(t)
			f.pushMergedGoneBranch("merged")
			sha := f.sha(f.local, "merged")
			path := f.addWorktree(filepath.Join(f.local, ".worktrees", "merged"), "merged")
			if tc.config != "" {
				f.localGit("config", "gh-sync.removeWorktrees", tc.config)
			}

			res := f.sync(tc.args...)
			if res.err != nil {
				t.Fatalf("run() error: %v", res.err)
			}
			if tc.remove {
				assertContains(t, "stdout", res.stdout, "Removed worktree "+filepath.Join(".worktrees", "merged")+".\n")
				if dirExists(path) || f.localBranchExists("merged") {
					t.Error("the worktree or its branch still exists")
				}
				return
			}
			want := "warning: 1 merged branch is still checked out in another worktree; not deleted:\n" +
				"  merged   " + filepath.Join(".worktrees", "merged") + "\n" +
				worktreeHint
			if res.stderr != want {
				t.Errorf("stderr = %q, want %q", res.stderr, want)
			}
			if !dirExists(path) {
				t.Error("the worktree was removed")
			}
			f.assertBranchAt("merged", sha)
		})
	}
}

// TestRunRejectsInvalidRemoveWorktreesConfig checks that a
// gh-sync.removeWorktrees value that is not a boolean stops gh-sync before
// anything is synced.
func TestRunRejectsInvalidRemoveWorktreesConfig(t *testing.T) {
	f := newSyncFixture(t)
	before := f.sha(f.local, "main")
	f.advanceUpstream("main", "upstream-1")
	f.localGit("config", "gh-sync.removeWorktrees", "maybe")

	res := f.sync()
	if res.err == nil {
		t.Fatal("run() error = nil, want an error")
	}
	want := "fatal: couldn't read gh-sync.removeWorktrees: bad boolean config value 'maybe' for 'gh-sync.removeworktrees'\n"
	if res.stderr != want {
		t.Errorf("stderr = %q, want %q", res.stderr, want)
	}
	f.assertBranchAt("main", before)
}

// TestRunRemoveWorktreesKeepsMainWorktree checks that, run from a linked
// worktree, --remove-worktrees does not try to remove the main worktree: a
// merged branch checked out there is reported in the summary, with a hint
// that fits.
func TestRunRemoveWorktreesKeepsMainWorktree(t *testing.T) {
	f := newSyncFixture(t)
	f.pushMergedGoneBranch("merged")
	sha := f.sha(f.local, "merged")
	f.localGit("checkout", "--quiet", "merged")
	linked := f.addWorktree(filepath.Join(t.TempDir(), "linked"), "main")
	mainWorktree := f.localGit("rev-parse", "--show-toplevel")
	t.Chdir(linked)

	for _, dryRun := range []bool{false, true} {
		args := []string{"--remove-worktrees"}
		if dryRun {
			args = append(args, "--dry-run")
		}
		res := f.sync(args...)
		if res.err != nil {
			t.Fatalf("run() error: %v", res.err)
		}
		want := "warning: 1 merged branch is still checked out in another worktree; not deleted:\n" +
			"  merged   " + mainWorktree + "\n" +
			"hint: the main worktree can't be removed; check out another branch in it, then run gh sync again\n"
		if res.stderr != want {
			t.Errorf("dryRun=%v: stderr = %q, want %q", dryRun, res.stderr, want)
		}
		assertNotContains(t, "stdout", res.stdout, "worktree")
		f.assertBranchAt("merged", sha)
	}
}

// TestRunColorsWorktreeSummaries checks that the whole warning block is
// yellow, while the hint and the note are not colored.
func TestRunColorsWorktreeSummaries(t *testing.T) {
	f := newSyncFixture(t)
	f.pushMergedGoneBranch("merged")
	f.pushBehindBranch("behind")
	f.addWorktree(filepath.Join(f.local, ".worktrees", "merged"), "merged")
	f.addWorktree(filepath.Join(f.local, ".worktrees", "behind"), "behind")

	res := f.sync("--color=always")
	if res.err != nil {
		t.Fatalf("run() error: %v", res.err)
	}
	want := "\033[33mwarning: 1 merged branch is still checked out in another worktree; not deleted:\n" +
		"  merged   " + filepath.Join(".worktrees", "merged") + "\033[0m\n" +
		worktreeHint +
		"note: 1 branch checked out in another worktree is behind its upstream; not updated\n"
	if res.stderr != want {
		t.Errorf("stderr = %q, want %q", res.stderr, want)
	}
}

func TestDisplayWorktreePath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	inside := filepath.Join(repo, ".claude", "worktrees", "foo")
	sibling := filepath.Join(base, "repo-wt")
	for _, dir := range []string{inside, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// link is another name for repo, as /tmp is for /private/tmp on macOS.
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	missing := filepath.Join(repo, "missing")

	tests := []struct {
		name         string
		path         string
		mainWorktree string
		want         string
	}{
		{"inside", inside, repo, filepath.Join(".claude", "worktrees", "foo")},
		{"inside, main worktree given through a symlink", inside, link, filepath.Join(".claude", "worktrees", "foo")},
		{"inside, path given through a symlink", filepath.Join(link, ".claude", "worktrees", "foo"), repo, filepath.Join(".claude", "worktrees", "foo")},
		{"inside but missing, so not resolvable", missing, repo, "missing"},
		{"outside, sharing a name prefix", sibling, repo, sibling},
		{"the main worktree itself", repo, repo, repo},
		{"main worktree unknown", inside, "", inside},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayWorktreePath(tt.path, tt.mainWorktree); got != tt.want {
				t.Errorf("displayWorktreePath(%q, %q) = %q, want %q", tt.path, tt.mainWorktree, got, tt.want)
			}
		})
	}
}

// TestSyncReportUpToDate checks what suppresses "Already up to date.". run()
// only prints it when stderr is a terminal, which the tests can't simulate,
// so the condition is tested here: in particular a branch that was not
// updated because of its worktree, which prints a note and no warning.
func TestSyncReportUpToDate(t *testing.T) {
	skip := []worktreeSkip{{branch: "feature", path: "/somewhere"}}
	tests := []struct {
		name   string
		report syncReport
		want   bool
	}{
		{"nothing happened", syncReport{}, true},
		{"changed", syncReport{changed: true}, false},
		{"warned", syncReport{warned: true}, false},
		{"not deleted in a worktree", syncReport{notDeleted: skip}, false},
		{"not updated in a worktree", syncReport{notUpdated: skip}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.report.upToDate(); got != tt.want {
				t.Errorf("upToDate() = %v, want %v", got, tt.want)
			}
		})
	}
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

func TestIsGitHubCloudHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"GitHub.com", true},
		{"gist.github.com", true},
		{"acme.ghe.com", true},
		{"github.example.com", false},
		{"gitlab.com", false},
		{"evilgithub.com", false},
		{"github.com.evil.example", false},
		{"ghe.com.evil.example", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isGitHubCloudHost(tt.host); got != tt.want {
			t.Errorf("isGitHubCloudHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestWithoutEnvVars(t *testing.T) {
	env := []string{"PATH=/bin", "GH_TOKEN=a", "gh_enterprise_token=b", "GH_TOKEN_OTHER=c", "GITHUB_ENTERPRISE_TOKEN=d=e"}
	got := withoutEnvVars(env, ghTokenEnvVars)
	want := []string{"PATH=/bin", "GH_TOKEN_OTHER=c"}
	if !slices.Equal(got, want) {
		t.Errorf("withoutEnvVars = %q, want %q", got, want)
	}
}

func TestCheckGhHost(t *testing.T) {
	// An empty PATH makes `gh auth token` fail, as it does for a host gh has
	// no stored credentials for: a token in the environment must not make an
	// arbitrary host acceptable.
	t.Setenv("PATH", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "token")
	t.Setenv("GH_HOST", "ghes.example.com")
	tests := []struct {
		host    string
		wantErr bool
	}{
		{"github.com", false},
		{"acme.ghe.com", false},
		{"ghes.example.com", false},
		{"GHES.example.com", false},
		{"gitlab.com", true},
	}
	for _, tt := range tests {
		cfg := runConfig{ghHosts: make(map[string]bool)}
		if err := checkGhHost(cfg, tt.host); (err != nil) != tt.wantErr {
			t.Errorf("checkGhHost(%q) error = %v, wantErr %v", tt.host, err, tt.wantErr)
		}
	}

	// A cached answer is used without asking gh again.
	cfg := runConfig{ghHosts: map[string]bool{"ghes2.example.com": true}}
	if err := checkGhHost(cfg, "ghes2.example.com"); err != nil {
		t.Errorf("checkGhHost with a cached host: %v", err)
	}
}

func TestFindMergedPRsWithGhRejectsOtherHosts(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "token")
	t.Setenv("GH_HOST", "")
	cfg := runConfig{sshHosts: make(map[string]string), ghHosts: make(map[string]bool)}
	_, err := findMergedPRsWithGh(cfg, Remote{Name: "origin", URL: "https://gitlab.com/owner/repo.git"}, "main", []string{"feature"})
	if err == nil || !strings.Contains(err.Error(), "not a GitHub host") {
		t.Errorf("findMergedPRsWithGh error = %v, want a host rejection", err)
	}
}
