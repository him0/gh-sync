package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mattn/go-isatty"
)

type repoInfo struct {
	Parent struct {
		Owner string `json:"owner"`
		Name  string `json:"name"`
	} `json:"parent"`
}

var (
	green      = "\033[32m"
	lightGreen = "\033[32;1m"
	red        = "\033[31m"
	lightRed   = "\033[31;1m"
	yellow     = "\033[33m"
	resetColor = "\033[0m"
)

func main() {
	// Set up color output
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		green = ""
		lightGreen = ""
		red = ""
		lightRed = ""
		yellow = ""
		resetColor = ""
	}

	// Check if current directory is a git repository
	if err := checkGitRepo(); err != nil {
		exitWithError("Not a git repository")
	}

	// Get parent repository info
	parent, err := getParentRepo()
	if err != nil {
		exitWithError("Failed to get repository info: %v", err)
	}
	if parent.Parent.Owner == "" || parent.Parent.Name == "" {
		exitWithError("This repository is not a fork")
	}

	upstreamURL := fmt.Sprintf("https://github.com/%s/%s.git", parent.Parent.Owner, parent.Parent.Name)
	fmt.Printf("Parent repository: %s%s/%s%s\n", lightGreen, parent.Parent.Owner, parent.Parent.Name, resetColor)

	// Check and setup upstream remote
	if err := setupUpstream(upstreamURL); err != nil {
		exitWithError("Failed to setup upstream: %v", err)
	}

	// Get current branch
	currentBranch, err := getCurrentBranch()
	if err != nil {
		exitWithError("Failed to get current branch: %v", err)
	}
	if currentBranch != "main" && currentBranch != "master" {
		fmt.Printf("%sWarning: You are on branch '%s', not 'main' or 'master'%s\n", yellow, currentBranch, resetColor)
	}

	// Fetch from upstream
	fmt.Printf("Fetching from upstream...\n")
	if err := runGit("fetch", "upstream"); err != nil {
		exitWithError("Failed to fetch from upstream: %v", err)
	}

	// Detect default branch
	defaultBranch := detectDefaultBranch()
	fmt.Printf("Default branch: %s%s%s\n", lightGreen, defaultBranch, resetColor)

	// Merge upstream default branch
	fmt.Printf("Merging upstream/%s...\n", defaultBranch)
	if err := runGit("merge", fmt.Sprintf("upstream/%s", defaultBranch)); err != nil {
		exitWithError("Failed to merge upstream/%s: %v", defaultBranch, err)
	}

	// Push to origin
	fmt.Printf("Pushing to origin...\n")
	if err := runGit("push", "origin", currentBranch); err != nil {
		exitWithError("Failed to push to origin: %v", err)
	}

	fmt.Printf("%s✓ Successfully synced with upstream%s\n", green, resetColor)
}

func checkGitRepo() error {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func getParentRepo() (repoInfo, error) {
	var info repoInfo
	cmd := exec.Command("gh", "repo", "view", "--json", "parent")
	output, err := cmd.Output()
	if err != nil {
		return info, err
	}

	if err := json.Unmarshal(output, &info); err != nil {
		return info, err
	}

	return info, nil
}

func setupUpstream(upstreamURL string) error {
	// Check existing upstream
	cmd := exec.Command("git", "remote", "get-url", "upstream")
	existingURL, err := cmd.Output()
	if err == nil {
		// Upstream already exists
		existing := strings.TrimSpace(string(existingURL))
		if existing != upstreamURL {
			fmt.Printf("Updating upstream URL from %s to %s\n", existing, upstreamURL)
			return runGit("remote", "set-url", "upstream", upstreamURL)
		}
		fmt.Printf("Upstream already configured: %s\n", existing)
		return nil
	}

	// Add upstream if it doesn't exist
	fmt.Printf("Adding upstream remote: %s\n", upstreamURL)
	return runGit("remote", "add", "upstream", upstreamURL)
}

func getCurrentBranch() (string, error) {
	cmd := exec.Command("git", "branch", "--show-current")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func detectDefaultBranch() string {
	// Check upstream/HEAD
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/upstream/HEAD")
	output, err := cmd.Output()
	if err == nil {
		branch := strings.TrimSpace(string(output))
		if strings.HasPrefix(branch, "refs/remotes/upstream/") {
			return strings.TrimPrefix(branch, "refs/remotes/upstream/")
		}
	}

	// Check if main branch exists
	if runGit("show-ref", "--verify", "--quiet", "refs/remotes/upstream/main") == nil {
		return "main"
	}

	// Check if master branch exists
	if runGit("show-ref", "--verify", "--quiet", "refs/remotes/upstream/master") == nil {
		return "master"
	}

	// Default to main
	return "main"
}

func runGit(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func exitWithError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "%sError: %s%s\n", lightRed, fmt.Sprintf(format, args...), resetColor)
	os.Exit(1)
}