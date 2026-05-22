package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
)

type Remote struct {
	Name string
	URL  string
}

// colorConfig holds ANSI color codes used for terminal output. When colorization
// is disabled, all fields are empty strings, so formatting code emits no escape
// sequences without any branching at call sites.
type colorConfig struct {
	green      string
	lightGreen string
	red        string
	lightRed   string
	magenta    string
	reset      string
}

// newColorConfig returns a colorConfig populated with ANSI codes when enabled
// is true, or a zero-value config (all empty strings) when disabled.
func newColorConfig(enabled bool) colorConfig {
	if !enabled {
		return colorConfig{}
	}
	return colorConfig{
		green:      "\033[32m",
		lightGreen: "\033[32;1m",
		red:        "\033[31m",
		lightRed:   "\033[31;1m",
		magenta:    "\033[35m",
		reset:      "\033[0m",
	}
}

var (
	verbose   bool
	colorFlag string
)

func main() {
	// Parse command line flags
	flag.BoolVar(&verbose, "verbose", false, "verbose output")
	flag.StringVar(&colorFlag, "color", "auto", "colorize output (always, never, auto)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nOptions:\n")
		fmt.Fprintf(os.Stderr, "  --verbose\n        verbose output\n")
		fmt.Fprintf(os.Stderr, "  --color string\n        colorize output (always, never, auto) (default \"auto\")\n")
	}
	flag.Parse()

	// Set up color output
	colors := newColorConfig(colorizeOutput(colorFlag))

	// Check if current directory is a git repository
	if err := checkGitRepo(); err != nil {
		exitWithError(colors, "fatal: Not a git repository")
	}

	// Get main remote (first available in priority order: upstream, github, origin)
	remote, err := getMainRemote()
	if err != nil {
		exitWithError(colors, err.Error())
	}

	// Get default branch for the remote
	defaultBranch := getDefaultBranch(remote)

	// Get current branch
	currentBranch := ""
	if branch, err := getCurrentBranch(); err == nil {
		currentBranch = branch
	}

	// Fetch from remote
	if err := runGitSilent(colors, "fetch", "--prune", "--quiet", "--progress", remote.Name); err != nil {
		exitWithError(colors, "Failed to fetch from %s", remote.Name)
	}

	// Get branch to remote mapping
	branchToRemote := getBranchToRemoteMapping()

	// Get all local branches
	branches, err := getLocalBranches()
	if err != nil {
		exitWithError(colors, "Failed to get local branches")
	}

	// Process each branch
	fullDefaultBranch := fmt.Sprintf("refs/remotes/%s/%s", remote.Name, defaultBranch)

	for _, branch := range branches {
		processBranch(colors, branch, remote, branchToRemote, &currentBranch, defaultBranch, fullDefaultBranch)
	}
}

func checkGitRepo() error {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func getMainRemote() (*Remote, error) {
	// Priority order: upstream, github, origin, others
	priorityOrder := []string{"upstream", "github", "origin"}

	remotes, err := getRemotes()
	if err != nil {
		return nil, err
	}

	if len(remotes) == 0 {
		return nil, fmt.Errorf("no git remotes found")
	}

	// Check priority remotes first
	for _, priority := range priorityOrder {
		for _, remote := range remotes {
			if remote.Name == priority {
				return &remote, nil
			}
		}
	}

	// Return first remote if no priority match
	return &remotes[0], nil
}

func getRemotes() ([]Remote, error) {
	cmd := exec.Command("git", "remote", "-v")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var remotes []Remote
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")

	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) >= 2 && strings.HasSuffix(line, "(fetch)") {
			name := parts[0]
			if !seen[name] {
				seen[name] = true
				remotes = append(remotes, Remote{Name: name, URL: parts[1]})
			}
		}
	}

	return remotes, nil
}

func getDefaultBranch(remote *Remote) string {
	// Try to get symbolic ref for remote HEAD first
	cmd := exec.Command("git", "symbolic-ref", fmt.Sprintf("refs/remotes/%s/HEAD", remote.Name))
	output, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(output))
		prefix := fmt.Sprintf("refs/remotes/%s/", remote.Name)
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}

	// Check if main branch exists on remote
	cmd = exec.Command("git", "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/remotes/%s/main", remote.Name))
	if cmd.Run() == nil {
		return "main"
	}

	// Check if master branch exists on remote
	cmd = exec.Command("git", "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/remotes/%s/master", remote.Name))
	if cmd.Run() == nil {
		return "master"
	}

	// Default to main (modern default)
	return "main"
}

func getCurrentBranch() (string, error) {
	cmd := exec.Command("git", "symbolic-ref", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func getBranchToRemoteMapping() map[string]string {
	branchToRemote := make(map[string]string)

	cmd := exec.Command("git", "config", "--get-regexp", "^branch\\..*\\.remote$")
	output, err := cmd.Output()
	if err != nil {
		return branchToRemote
	}

	configRe := regexp.MustCompile(`^branch\.(.+?)\.remote (.+)`)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")

	for _, line := range lines {
		if matches := configRe.FindStringSubmatch(line); len(matches) > 0 {
			branchToRemote[matches[1]] = matches[2]
		}
	}

	return branchToRemote
}

func getLocalBranches() ([]string, error) {
	cmd := exec.Command("git", "branch", "--format=%(refname:short)")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var branches []string
	for _, line := range lines {
		branch := strings.TrimSpace(line)
		if branch != "" {
			branches = append(branches, branch)
		}
	}

	return branches, nil
}

func processBranch(colors colorConfig, branch string, remote *Remote, branchToRemote map[string]string, currentBranch *string, defaultBranch, fullDefaultBranch string) {
	fullBranch := fmt.Sprintf("refs/heads/%s", branch)
	remoteBranch := fmt.Sprintf("refs/remotes/%s/%s", remote.Name, branch)
	gone := false

	// Check if branch has upstream configuration
	if branchToRemote[branch] == remote.Name {
		cmd := exec.Command("git", "rev-parse", "--symbolic-full-name", fmt.Sprintf("%s@{upstream}", branch))
		output, err := cmd.Output()
		if err == nil {
			remoteBranch = strings.TrimSpace(string(output))
		} else {
			remoteBranch = ""
			gone = true
		}
	} else if !hasRemoteBranch(remoteBranch) {
		remoteBranch = ""
	}

	if remoteBranch != "" {
		// Branch has corresponding remote branch
		if ahead, behind, err := getCommitDifference(fullBranch, remoteBranch); err == nil {
			if ahead == 0 && behind == 0 {
				// Branches are identical, do nothing
				return
			} else if ahead == 0 && behind > 0 {
				// Local branch is behind, can fast-forward
				oldCommit := getCommitSHA(fullBranch)
				var updateErr error
				if branch == *currentBranch {
					updateErr = runGitSilent(colors, "merge", "--ff-only", "--quiet", remoteBranch)
				} else {
					updateErr = runGitSilent(colors, "update-ref", fullBranch, remoteBranch)
				}
				if updateErr != nil {
					fmt.Fprintf(os.Stderr, "warning: couldn't fast-forward '%s'\n", branch)
				} else {
					fmt.Printf("%sUpdated branch %s%s%s (was %s).\n", colors.green, colors.lightGreen, branch, colors.reset, oldCommit[:7])
				}
			} else {
				// Local branch has unpushed commits
				fmt.Fprintf(os.Stderr, "warning: '%s' seems to contain unpushed commits\n", branch)
			}
		}
	} else if gone {
		// Remote branch was deleted
		if ahead, behind, err := getCommitDifference(fullBranch, fullDefaultBranch); err == nil {
			if ahead == 0 && behind >= 0 {
				// Branch is ancestor of default branch, safe to delete
				oldCommit := getCommitSHA(fullBranch)
				if branch == *currentBranch {
					if err := runGitSilent(colors, "checkout", "--quiet", defaultBranch); err != nil {
						fmt.Fprintf(os.Stderr, "warning: couldn't checkout '%s'\n", defaultBranch)
						return
					}
					*currentBranch = defaultBranch
				}
				if err := runGitSilent(colors, "branch", "-D", branch); err != nil {
					fmt.Fprintf(os.Stderr, "warning: couldn't delete '%s'\n", branch)
				} else {
					fmt.Printf("%sDeleted branch %s%s%s (was %s).\n", colors.red, colors.lightRed, branch, colors.reset, oldCommit[:7])
				}
			} else {
				// Branch appears not merged
				fmt.Fprintf(os.Stderr, "warning: '%s' was deleted on %s, but appears not merged into '%s'\n", branch, remote.Name, defaultBranch)
			}
		}
	}
}

func hasRemoteBranch(remoteBranch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", remoteBranch)
	return cmd.Run() == nil
}

func getCommitDifference(branch1, branch2 string) (ahead, behind int, err error) {
	// Get commits ahead
	cmd := exec.Command("git", "rev-list", "--count", fmt.Sprintf("%s..%s", branch2, branch1))
	output, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	ahead, err = strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse ahead count: %w", err)
	}

	// Get commits behind
	cmd = exec.Command("git", "rev-list", "--count", fmt.Sprintf("%s..%s", branch1, branch2))
	output, err = cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	behind, err = strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse behind count: %w", err)
	}

	return ahead, behind, nil
}

func getCommitSHA(ref string) string {
	cmd := exec.Command("git", "rev-parse", ref)
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func runGitSilent(colors colorConfig, args ...string) error {
	verboseLog(colors, "git", args)
	cmd := exec.Command("git", args...)
	return cmd.Run()
}

func verboseLog(colors colorConfig, cmd string, args []string) {
	if verbose {
		msg := fmt.Sprintf("$ %s %s", cmd, strings.Join(args, " "))
		if isatty.IsTerminal(os.Stderr.Fd()) && colors.magenta != "" {
			msg = fmt.Sprintf("%s%s%s", colors.magenta, msg, colors.reset)
		}
		fmt.Fprintln(os.Stderr, msg)
	}
}

func colorizeOutput(flag string) bool {
	switch flag {
	case "always":
		return true
	case "never":
		return false
	case "auto":
		return isatty.IsTerminal(os.Stdout.Fd())
	default:
		return isatty.IsTerminal(os.Stdout.Fd())
	}
}

func exitWithError(colors colorConfig, format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s%s%s\n", colors.lightRed, fmt.Sprintf(format, args...), colors.reset)
	os.Exit(1)
}
