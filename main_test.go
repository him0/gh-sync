package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestColorizeOutput(t *testing.T) {
	tests := []struct {
		name string
		flag string
		want bool
	}{
		{"always", "always", true},
		{"never", "never", false},
		// "auto" depends on whether stdout is a terminal; in tests it is not
		{"auto_in_test", "auto", false},
		// unknown values fall through to auto behavior
		{"unknown_in_test", "typo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := colorizeOutput(tt.flag)
			if got != tt.want {
				t.Errorf("colorizeOutput(%q) = %v, want %v", tt.flag, got, tt.want)
			}
		})
	}
}

func TestNewColorConfig(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		c := newColorConfig(true)
		if c.green == "" || c.lightGreen == "" || c.red == "" || c.lightRed == "" || c.magenta == "" || c.reset == "" {
			t.Errorf("newColorConfig(true) returned empty fields: %+v", c)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		c := newColorConfig(false)
		if c.green != "" || c.lightGreen != "" || c.red != "" || c.lightRed != "" || c.magenta != "" || c.reset != "" {
			t.Errorf("newColorConfig(false) returned non-empty fields: %+v", c)
		}
	})
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

func TestGetCommitDifference(t *testing.T) {
	dir := setupTestRepo(t)
	// t.Chdir restores the previous CWD on cleanup and serializes against
	// other Chdir-using tests, so callers don't need to coordinate manually.
	t.Chdir(dir)

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}

	defaultBranch := "refs/heads/main"

	run("git", "checkout", "-b", "feature")
	run("git", "commit", "--allow-empty", "-m", "feature-1")
	run("git", "commit", "--allow-empty", "-m", "feature-2")

	featureBranch := "refs/heads/feature"

	ahead, behind, err := getCommitDifference(featureBranch, defaultBranch)
	if err != nil {
		t.Fatalf("getCommitDifference() error: %v", err)
	}
	if ahead != 2 {
		t.Errorf("expected ahead=2, got %d", ahead)
	}
	if behind != 0 {
		t.Errorf("expected behind=0, got %d", behind)
	}

	ahead, behind, err = getCommitDifference(defaultBranch, featureBranch)
	if err != nil {
		t.Fatalf("getCommitDifference() error: %v", err)
	}
	if ahead != 0 {
		t.Errorf("expected ahead=0, got %d", ahead)
	}
	if behind != 2 {
		t.Errorf("expected behind=2, got %d", behind)
	}
}

func TestGetDefaultBranch(t *testing.T) {
	dir := setupTestRepo(t)
	t.Chdir(dir)

	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "clone", "--bare", dir, remoteDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone --bare failed: %v\n%s", err, out)
	}

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}

	run("git", "remote", "add", "testremote", remoteDir)
	run("git", "fetch", "testremote")
	// `git fetch` does not create refs/remotes/<remote>/HEAD; without
	// `set-head` the test would only exercise the main/master fallback paths.
	run("git", "remote", "set-head", "testremote", "--auto")

	remote := &Remote{Name: "testremote", URL: remoteDir}

	branch, err := getDefaultBranch(remote)
	if err != nil {
		t.Fatalf("getDefaultBranch() error: %v", err)
	}
	if branch != "main" {
		t.Errorf("getDefaultBranch() = %q, want %q", branch, "main")
	}
}

func TestGetDefaultBranchMissingRemote(t *testing.T) {
	dir := setupTestRepo(t)
	t.Chdir(dir)

	// Remote configured but with no fetched refs — neither symbolic-ref nor
	// show-ref of main/master will succeed.
	remote := &Remote{Name: "ghost", URL: ""}
	if _, err := getDefaultBranch(remote); err == nil {
		t.Error("getDefaultBranch() with no remote refs: want error, got nil")
	}
}

func TestShortSHA(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "unknown"},
		{"abc", "abc"},
		{"abcdef0", "abcdef0"},
		{"abcdef01234567", "abcdef0"},
	}
	for _, tt := range tests {
		if got := shortSHA(tt.in); got != tt.want {
			t.Errorf("shortSHA(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
