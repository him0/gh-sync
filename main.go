package main

import (
	"bytes"
	"errors"
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

// runConfig bundles per-invocation settings so run() and its helpers stay
// independent of package-level mutable state — making run() safely re-entrant
// from tests.
type runConfig struct {
	colors  colorConfig
	verbose bool
}

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	// --help / -h prints usage to stderr from inside fs.Parse and returns
	// flag.ErrHelp. That's a successful invocation, not a failure.
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	os.Exit(1)
}

func run(args []string) error {
	fs := flag.NewFlagSet("gh-sync", flag.ContinueOnError)
	var (
		verbose   bool
		colorFlag string
	)
	fs.BoolVar(&verbose, "verbose", false, "verbose output")
	fs.StringVar(&colorFlag, "color", "auto", "colorize output (always, never, auto)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gh-sync [options]\n")
		fmt.Fprintf(os.Stderr, "\nOptions:\n")
		fmt.Fprintf(os.Stderr, "  --verbose\n        verbose output\n")
		fmt.Fprintf(os.Stderr, "  --color string\n        colorize output (always, never, auto) (default \"auto\")\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch colorFlag {
	case "always", "never", "auto":
	default:
		fmt.Fprintf(os.Stderr, "fatal: invalid --color value: %q (must be one of: always, never, auto)\n", colorFlag)
		return fmt.Errorf("invalid --color value: %q", colorFlag)
	}

	cfg := runConfig{
		colors:  newColorConfig(colorizeOutput(colorFlag)),
		verbose: verbose,
	}

	if err := checkGitRepo(); err != nil {
		printError(cfg.colors, "fatal: Not a git repository")
		return err
	}

	remote, err := getMainRemote()
	if err != nil {
		printError(cfg.colors, "%s", err.Error())
		return err
	}

	defaultBranch, err := getDefaultBranch(remote)
	if err != nil {
		printError(cfg.colors, "Failed to determine default branch for %s: %s", remote.Name, err.Error())
		return err
	}

	currentBranch := ""
	if branch, err := getCurrentBranch(); err == nil {
		currentBranch = branch
	}

	if err := runGitSilent(cfg, "fetch", "--prune", "--quiet", "--progress", remote.Name); err != nil {
		printError(cfg.colors, "Failed to fetch from %s: %s", remote.Name, err.Error())
		return err
	}

	branchToRemote, err := getBranchToRemoteMapping()
	if err != nil {
		printError(cfg.colors, "Failed to read branch tracking config: %s", err.Error())
		return err
	}

	branches, err := getLocalBranches()
	if err != nil {
		printError(cfg.colors, "Failed to get local branches: %s", err.Error())
		return err
	}

	fullDefaultBranch := fmt.Sprintf("refs/remotes/%s/%s", remote.Name, defaultBranch)

	hadFailure := false
	for _, branch := range branches {
		if perr := processBranch(cfg, branch, remote, branchToRemote, &currentBranch, defaultBranch, fullDefaultBranch); perr != nil {
			hadFailure = true
		}
	}

	if hadFailure {
		return fmt.Errorf("one or more branches failed to sync")
	}
	return nil
}

// gitOutput runs git with the given args, captures stdout, and on failure
// wraps the error with the captured stderr so callers can surface git's real
// diagnostic instead of a bare "exit status N".
func gitOutput(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
	}
	return out, err
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
	output, err := gitOutput("remote", "-v")
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

func getDefaultBranch(remote *Remote) (string, error) {
	// Try to get symbolic ref for remote HEAD first
	cmd := exec.Command("git", "symbolic-ref", fmt.Sprintf("refs/remotes/%s/HEAD", remote.Name))
	output, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(output))
		prefix := fmt.Sprintf("refs/remotes/%s/", remote.Name)
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix), nil
		}
	}

	// Check if main branch exists on remote
	cmd = exec.Command("git", "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/remotes/%s/main", remote.Name))
	if cmd.Run() == nil {
		return "main", nil
	}

	// Check if master branch exists on remote
	cmd = exec.Command("git", "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/remotes/%s/master", remote.Name))
	if cmd.Run() == nil {
		return "master", nil
	}

	return "", fmt.Errorf("could not determine default branch (no remote HEAD, no main, no master on %s); set it with `git remote set-head %s --auto`", remote.Name, remote.Name)
}

func getCurrentBranch() (string, error) {
	output, err := gitOutput("symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// configRemoteRe matches lines from `git config --get-regexp branch\..*\.remote`.
// The remote name is captured as a non-whitespace run so trailing CR (CRLF
// inputs on Windows) or trailing spaces are not absorbed into the value.
var configRemoteRe = regexp.MustCompile(`^branch\.(.+?)\.remote\s+(\S+)`)

// getBranchToRemoteMapping returns a map of local branch → configured remote.
// A missing config entry (git config exits non-zero when no keys match) is
// reported as nil error with an empty map, distinguishing it from a true
// failure (corrupt config, permission denied) which is returned to the caller.
func getBranchToRemoteMapping() (map[string]string, error) {
	branchToRemote := make(map[string]string)

	cmd := exec.Command("git", "config", "--get-regexp", `^branch\..*\.remote$`)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		// git config exits 1 when no entries match — that's expected for a
		// fresh repo with no tracked branches and is not a failure.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return branchToRemote, nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if matches := configRemoteRe.FindStringSubmatch(line); len(matches) > 0 {
			branchToRemote[matches[1]] = matches[2]
		}
	}

	return branchToRemote, nil
}

func getLocalBranches() ([]string, error) {
	output, err := gitOutput("branch", "--format=%(refname:short)")
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

// processBranch synchronizes a single local branch against the main remote.
// It returns a non-nil error when an operation it attempted failed (and was
// announced to the user); branches that are simply skipped (no upstream
// configured, already up to date, or diverged with unpushed commits) return
// nil. The error is used purely to drive the process exit code — diagnostic
// messages are printed inline.
func processBranch(cfg runConfig, branch string, remote *Remote, branchToRemote map[string]string, currentBranch *string, defaultBranch, fullDefaultBranch string) error {
	fullBranch := fmt.Sprintf("refs/heads/%s", branch)
	remoteBranch := fmt.Sprintf("refs/remotes/%s/%s", remote.Name, branch)
	gone := false

	if branchToRemote[branch] == remote.Name {
		cmd := exec.Command("git", "rev-parse", "--symbolic-full-name", fmt.Sprintf("%s@{upstream}", branch))
		output, err := cmd.Output()
		if err == nil {
			remoteBranch = strings.TrimSpace(string(output))
		} else {
			// rev-parse @{upstream} can fail for two distinct reasons:
			//   (a) the remote tracking ref was pruned (truly gone), or
			//   (b) branch.<name>.merge is missing/malformed (misconfig).
			// Treat the branch as gone only when (a) is the case — otherwise
			// the deletion path below would force-delete a branch over a
			// config typo.
			candidate := fmt.Sprintf("refs/remotes/%s/%s", remote.Name, branch)
			if hasRemoteBranch(candidate) {
				fmt.Fprintf(os.Stderr, "warning: '%s' upstream config is broken; skipping\n", branch)
				return nil
			}
			remoteBranch = ""
			gone = true
		}
	} else if branchToRemote[branch] == "" && hasRemoteBranch(remoteBranch) {
		// No upstream config, but a same-named ref exists on the main remote.
		// Fast-forward by name match (mirrors hub sync), but emit a hint so
		// the user knows two unrelated branches with the same name are being
		// treated as related.
		if cfg.verbose {
			fmt.Fprintf(os.Stderr, "note: '%s' has no upstream config; matching by name against %s/%s\n", branch, remote.Name, branch)
		}
	} else if !hasRemoteBranch(remoteBranch) {
		remoteBranch = ""
	}

	if remoteBranch != "" {
		ahead, behind, err := getCommitDifference(fullBranch, remoteBranch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't compare '%s' against '%s': %s\n", branch, remoteBranch, err.Error())
			return err
		}
		switch {
		case ahead == 0 && behind == 0:
			return nil
		case ahead == 0 && behind > 0:
			oldCommit := getCommitSHA(fullBranch)
			var updateErr error
			if branch == *currentBranch {
				updateErr = runGitSilent(cfg, "merge", "--ff-only", "--quiet", remoteBranch)
			} else {
				// `branch -f` (unlike update-ref) refuses to move a ref that
				// is checked out in another worktree, preventing silent
				// desync of sibling worktrees.
				updateErr = runGitSilent(cfg, "branch", "-f", branch, remoteBranch)
			}
			if updateErr != nil {
				fmt.Fprintf(os.Stderr, "warning: couldn't fast-forward '%s': %s\n", branch, updateErr.Error())
				return updateErr
			}
			fmt.Printf("%sUpdated branch %s%s%s (was %s).\n", cfg.colors.green, cfg.colors.lightGreen, branch, cfg.colors.reset, shortSHA(oldCommit))
		default:
			fmt.Fprintf(os.Stderr, "warning: '%s' seems to contain unpushed commits\n", branch)
		}
		return nil
	}

	if !gone {
		return nil
	}

	ahead, _, err := getCommitDifference(fullBranch, fullDefaultBranch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't compare '%s' against '%s': %s\n", branch, fullDefaultBranch, err.Error())
		return err
	}
	if ahead != 0 {
		fmt.Fprintf(os.Stderr, "warning: '%s' was deleted on %s, but appears not merged into '%s'\n", branch, remote.Name, defaultBranch)
		return nil
	}

	// ahead == 0: every commit on this branch is already in the default
	// branch, so it's safe to delete.
	oldCommit := getCommitSHA(fullBranch)
	if branch == *currentBranch {
		if err := runGitSilent(cfg, "checkout", "--quiet", defaultBranch); err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't checkout '%s': %s\n", defaultBranch, err.Error())
			return err
		}
		*currentBranch = defaultBranch
	}
	if err := runGitSilent(cfg, "branch", "-D", branch); err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't delete '%s': %s\n", branch, err.Error())
		return err
	}
	fmt.Printf("%sDeleted branch %s%s%s (was %s).\n", cfg.colors.red, cfg.colors.lightRed, branch, cfg.colors.reset, shortSHA(oldCommit))
	return nil
}

func hasRemoteBranch(remoteBranch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", remoteBranch)
	return cmd.Run() == nil
}

func getCommitDifference(branch1, branch2 string) (ahead, behind int, err error) {
	// For "branch1...branch2", --left-right --count outputs "<left>\t<right>",
	// where left = commits only in branch1 (ahead) and right = commits only in branch2 (behind).
	// Use `--` to defend against branch names that could be misread as flags.
	output, err := gitOutput("rev-list", "--left-right", "--count", fmt.Sprintf("%s...%s", branch1, branch2), "--")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(strings.TrimSpace(string(output)))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output: %q", string(output))
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse ahead count: %w", err)
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse behind count: %w", err)
	}

	return ahead, behind, nil
}

// getCommitSHA returns the full SHA for ref, or an empty string if rev-parse
// fails. Callers use shortSHA to format the result for display so a missing
// SHA is rendered as "unknown" without risking a slice-out-of-range panic.
func getCommitSHA(ref string) string {
	cmd := exec.Command("git", "rev-parse", ref)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// shortSHA renders a SHA prefix safely, returning "unknown" when sha is empty
// and never slicing past the available length.
func shortSHA(sha string) string {
	if sha == "" {
		return "unknown"
	}
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

func runGitSilent(cfg runConfig, args ...string) error {
	verboseLog(cfg, "git", args)
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if cfg.verbose && msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

func verboseLog(cfg runConfig, cmd string, args []string) {
	if cfg.verbose {
		msg := fmt.Sprintf("$ %s %s", cmd, strings.Join(args, " "))
		if cfg.colors.magenta != "" {
			msg = fmt.Sprintf("%s%s%s", cfg.colors.magenta, msg, cfg.colors.reset)
		}
		fmt.Fprintln(os.Stderr, msg)
	}
}

// colorizeOutput resolves the effective colorization decision for terminal
// output. Most of gh-sync's user-facing output (verboseLog, printError,
// runGitSilent's stderr echo) writes to stderr, so "auto" is keyed on
// stderr's TTY status. The NO_COLOR environment variable
// (https://no-color.org) suppresses color when set to any value unless the
// user explicitly opts in with --color=always.
func colorizeOutput(flag string) bool {
	switch flag {
	case "always":
		return true
	case "never":
		return false
	default:
		// "auto" and any unrecognized value behave the same; run() rejects
		// unknown values before reaching here.
		if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
			return false
		}
		return isatty.IsTerminal(os.Stderr.Fd())
	}
}

func printError(colors colorConfig, format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s%s%s\n", colors.lightRed, fmt.Sprintf(format, args...), colors.reset)
}
