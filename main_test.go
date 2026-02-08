package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			oldFlag := colorFlag
			defer func() { colorFlag = oldFlag }()
			colorFlag = tt.flag
			got := colorizeOutput()
			if got != tt.want {
				t.Errorf("colorizeOutput() with flag=%q = %v, want %v", tt.flag, got, tt.want)
			}
		})
	}
}

// setupTestRepo creates a temporary git repo and returns its path and a cleanup function.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init"},
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

	// NOTE: os.Chdir mutates process-global state, so this test must not use t.Parallel().
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	// Create a branch and add commits to diverge
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}

	// Get current branch name (main or master)
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	branchOut, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	defaultBranch := "refs/heads/" + strings.TrimSpace(string(branchOut))

	// Create feature branch and add 2 commits
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

	// Check reverse direction
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

	// NOTE: os.Chdir mutates process-global state, so this test must not use t.Parallel().
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	// Create a bare remote to simulate remote/HEAD
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

	remote := &Remote{Name: "testremote", URL: remoteDir}

	// getDefaultBranch should find a branch (main or master depending on git version)
	branch := getDefaultBranch(remote)
	if branch == "" {
		t.Error("getDefaultBranch() returned empty string")
	}
	if branch != "main" && branch != "master" {
		t.Errorf("getDefaultBranch() = %q, expected 'main' or 'master'", branch)
	}
}
