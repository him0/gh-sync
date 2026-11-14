package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
)

// version is the gh-sync version reported by --version. Release builds
// override it at link time with -ldflags "-X main.version=<tag>" (see
// .github/workflows/release.yml); other builds report "dev".
var version = "dev"

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
//
// Colorization is resolved per output stream: stdoutColors is used for
// messages written to stdout (e.g. "Updated branch ...") and stderrColors for
// messages written to stderr (errors, verbose logs). This keeps ANSI escapes
// out of a stream that is piped even when the other one is a terminal.
//
// dryRun reports the updates and deletions processBranch would make without
// making them. fetch still runs so the report reflects the remote's state.
//
// findMergedPRs looks up merged pull requests for gone branches whose commits
// are not in the default branch (squash or rebase merges). Tests stub it.
//
// sshHosts caches the host names resolved with `ssh -G`, so ssh runs at most
// once per host per sync.
type runConfig struct {
	stdoutColors  colorConfig
	stderrColors  colorConfig
	verbose       bool
	dryRun        bool
	findMergedPRs mergedPRFinder
	sshHosts      map[string]string
}

// mergedPR is a merged pull request and the commit its head pointed to.
type mergedPR struct {
	Number     int    `json:"number"`
	HeadRefOid string `json:"headRefOid"`
}

// mergedPRFinder returns, for each name in heads, the pull requests merged
// into base in remote's repository from a branch with that name. Branch names
// are matched without an owner, so pull requests opened from forks are
// included; the caller only accepts one whose head commit equals the local
// tip. An error means the lookup could not be done (gh missing or not
// authenticated, a remote that is not on GitHub, an API error, a timeout).
type mergedPRFinder func(cfg runConfig, remote Remote, base string, heads []string) (map[string][]mergedPR, error)

func main() {
	err := run(os.Args[1:], findMergedPRsWithGh)
	if err == nil {
		return
	}
	// --help / -h makes run() print the usage to stdout and return
	// flag.ErrHelp. That's a successful invocation, not a failure.
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	os.Exit(1)
}

func run(args []string, findMergedPRs mergedPRFinder) error {
	fs := flag.NewFlagSet("gh-sync", flag.ContinueOnError)
	var (
		verbose     bool
		dryRun      bool
		colorFlag   string
		showVersion bool
	)
	fs.BoolVar(&verbose, "verbose", false, "verbose output")
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be updated or deleted without changing branches")
	fs.StringVar(&colorFlag, "color", "auto", "colorize output (always, never, auto)")
	fs.BoolVar(&showVersion, "version", false, "print version and exit")
	// fs.Parse calls fs.Usage both for --help and for parse errors, so it
	// cannot tell which stream to use. Print the usage after Parse instead:
	// to stdout when help was requested (so `gh sync --help | less` works)
	// and to stderr for usage errors.
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
		} else {
			printUsage(os.Stderr)
		}
		return err
	}
	// gh-sync takes no positional arguments. flag stops parsing at the first
	// non-flag argument, so silently ignoring the rest would turn
	// "gh sync dry-run" or "gh sync foo --dry-run" into a real sync.
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "fatal: unexpected argument: %q\n", fs.Arg(0))
		printUsage(os.Stderr)
		return fmt.Errorf("unexpected argument: %q", fs.Arg(0))
	}

	if showVersion {
		fmt.Printf("gh-sync version %s\n", version)
		return nil
	}

	switch colorFlag {
	case "always", "never", "auto":
	default:
		fmt.Fprintf(os.Stderr, "fatal: invalid --color value: %q (must be one of: always, never, auto)\n", colorFlag)
		return fmt.Errorf("invalid --color value: %q", colorFlag)
	}

	noColor := noColorSet()
	stderrIsTerminal := isTerminal(os.Stderr.Fd())
	cfg := runConfig{
		stdoutColors:  newColorConfig(colorizeOutput(colorFlag, isTerminal(os.Stdout.Fd()), noColor)),
		stderrColors:  newColorConfig(colorizeOutput(colorFlag, stderrIsTerminal, noColor)),
		verbose:       verbose,
		dryRun:        dryRun,
		findMergedPRs: findMergedPRs,
		sshHosts:      make(map[string]string),
	}

	if err := checkGitRepo(); err != nil {
		printError(cfg.stderrColors, "fatal: Not a git repository")
		return err
	}

	remote, err := getMainRemote()
	if err != nil {
		printError(cfg.stderrColors, "%s", err.Error())
		return err
	}

	currentBranch := ""
	if branch, err := getCurrentBranch(); err == nil {
		currentBranch = branch
	}

	// --progress only shows something when git's stderr reaches the user, so
	// pass it through when stderr is a terminal. Otherwise capture it as
	// before, so a failure is reported once, inside the error below.
	fetchArgs := []string{"fetch", "--prune", "--quiet", "--progress", remote.Name}
	var fetchErr error
	if stderrIsTerminal {
		fetchErr = runGitStderr(cfg, fetchArgs...)
	} else {
		fetchErr = runGitSilent(cfg, fetchArgs...)
	}
	if err := fetchErr; err != nil {
		printError(cfg.stderrColors, "Failed to fetch from %s: %s", remote.Name, err.Error())
		return err
	}

	// Determined after the fetch so a remote that was never fetched has its
	// remote-tracking refs, and refs pruned by the fetch are not trusted.
	defaultBranch, err := getDefaultBranch(remote)
	if err != nil {
		printError(cfg.stderrColors, "Failed to determine default branch for %s: %s", remote.Name, err.Error())
		return err
	}

	branchToRemote, err := getBranchToRemoteMapping()
	if err != nil {
		printError(cfg.stderrColors, "Failed to read branch tracking config: %s", err.Error())
		return err
	}

	branches, err := getLocalBranches()
	if err != nil {
		printError(cfg.stderrColors, "Failed to get local branches: %s", err.Error())
		return err
	}

	fullDefaultBranch := fmt.Sprintf("refs/remotes/%s/%s", remote.Name, defaultBranch)

	hadFailure := false
	var candidates []squashCandidate
	for _, branch := range branches {
		if perr := processBranch(cfg, branch, remote, branchToRemote, &currentBranch, defaultBranch, fullDefaultBranch, &candidates); perr != nil {
			hadFailure = true
		}
	}

	// Gone branches whose commits are not in the default branch are looked
	// up together, so the number of gh requests doesn't grow with them.
	if len(candidates) > 0 {
		merged := lookupMergedPRs(cfg, *remote, defaultBranch, candidates)
		for _, c := range candidates {
			pr := merged[c.branch]
			if pr == 0 {
				fmt.Fprintf(os.Stderr, "warning: '%s' was deleted on %s, but appears not merged into '%s'\n", c.branch, remote.Name, defaultBranch)
				continue
			}
			if derr := deleteBranch(cfg, c.branch, c.tip, fmt.Sprintf(", merged via #%d", pr), &currentBranch, defaultBranch); derr != nil {
				hadFailure = true
			}
		}
	}

	if hadFailure {
		return fmt.Errorf("one or more branches failed to sync")
	}
	return nil
}

// usageText is printed by --help and after usage errors.
const usageText = `Usage: gh sync [options]

Options:
  --verbose
        verbose output
  --dry-run
        show what would be updated or deleted without changing branches
  --color string
        colorize output (always, never, auto) (default "auto")
  --version
        print version and exit
`

func printUsage(w io.Writer) {
	// Like the other writes to stdout/stderr, a failed write has nowhere
	// better to be reported.
	_, _ = fmt.Fprint(w, usageText)
}

// isTerminal reports whether fd is a terminal, including the MSYS2 / Cygwin
// pty used by Git Bash (mintty), which isatty.IsTerminal does not recognize.
func isTerminal(fd uintptr) bool {
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// noColorSet reports whether NO_COLOR asks to disable color. Per
// https://no-color.org it counts only when present and not an empty string.
func noColorSet() bool {
	return os.Getenv("NO_COLOR") != ""
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
		name, url, ok := parseFetchRemoteLine(line)
		if ok && !seen[name] {
			seen[name] = true
			remotes = append(remotes, Remote{Name: name, URL: url})
		}
	}

	return remotes, nil
}

// parseFetchRemoteLine parses one line of `git remote -v` and reports whether
// it is a fetch line. The format is "<name>\t<url> (fetch)", and for a
// promisor remote (partial clone) git appends the filter:
// "<name>\t<url> (fetch) [blob:none]".
func parseFetchRemoteLine(line string) (name, url string, ok bool) {
	name, rest, found := strings.Cut(line, "\t")
	if !found || name == "" {
		return "", "", false
	}
	if i := strings.LastIndex(rest, " (fetch)"); i >= 0 {
		tail := rest[i+len(" (fetch)"):]
		if tail == "" || strings.HasPrefix(tail, " [") {
			return name, rest[:i], true
		}
	}
	return "", "", false
}

func getDefaultBranch(remote *Remote) (string, error) {
	// Try to get symbolic ref for remote HEAD first. The ref it points at may
	// have been pruned (e.g. the default branch was renamed on the remote, and
	// `git fetch --prune` does not update HEAD), so it is only trusted when the
	// target still exists.
	output, err := gitOutput("symbolic-ref", fmt.Sprintf("refs/remotes/%s/HEAD", remote.Name))
	if err == nil {
		ref := strings.TrimSpace(string(output))
		prefix := fmt.Sprintf("refs/remotes/%s/", remote.Name)
		if strings.HasPrefix(ref, prefix) && hasRemoteBranch(ref) {
			return strings.TrimPrefix(ref, prefix), nil
		}
	}

	// Check if main branch exists on remote. show-ref --verify --quiet only
	// signals existence through its exit code, so there is no diagnostic to
	// capture here.
	if hasRemoteBranch(fmt.Sprintf("refs/remotes/%s/main", remote.Name)) {
		return "main", nil
	}

	// Check if master branch exists on remote
	if hasRemoteBranch(fmt.Sprintf("refs/remotes/%s/master", remote.Name)) {
		return "master", nil
	}

	return "", fmt.Errorf("could not determine default branch (no remote HEAD, no main, no master on %s); set it with `git remote set-head %s --auto`", remote.Name, remote.Name)
}

// getCurrentBranch returns the name of the checked-out branch. It strips
// "refs/heads/" itself rather than using `symbolic-ref --short`, which prints
// "heads/<name>" when a tag has the same name, so the result matches the
// names returned by getLocalBranches.
func getCurrentBranch() (string, error) {
	output, err := gitOutput("symbolic-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(string(output)), "refs/heads/"), nil
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

// getLocalBranches lists the names of local branches. It uses for-each-ref
// with lstrip=2 rather than `git branch --format=%(refname:short)`, which
// prints "heads/<name>" when a tag has the same name and includes a
// "(HEAD detached at ...)" line when HEAD is detached.
func getLocalBranches() ([]string, error) {
	output, err := gitOutput("for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads/")
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
//
// A gone branch whose commits are not in the default branch is appended to
// candidates instead; run() decides on those after a pull request lookup.
func processBranch(cfg runConfig, branch string, remote *Remote, branchToRemote map[string]string, currentBranch *string, defaultBranch, fullDefaultBranch string, candidates *[]squashCandidate) error {
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
			if cfg.dryRun {
				fmt.Printf("%sWould update branch %s%s%s (was %s).\n", cfg.stdoutColors.green, cfg.stdoutColors.lightGreen, branch, cfg.stdoutColors.reset, shortSHA(oldCommit))
				return nil
			}
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
			fmt.Printf("%sUpdated branch %s%s%s (was %s).\n", cfg.stdoutColors.green, cfg.stdoutColors.lightGreen, branch, cfg.stdoutColors.reset, shortSHA(oldCommit))
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
	oldCommit := getCommitSHA(fullBranch)
	if ahead != 0 {
		// The commits are not in the default branch, which is also the case
		// after a squash or rebase merge.
		*candidates = append(*candidates, squashCandidate{branch: branch, tip: oldCommit})
		return nil
	}
	return deleteBranch(cfg, branch, oldCommit, "", currentBranch, defaultBranch)
}

// deleteBranch deletes branch, checking out defaultBranch first when branch
// is checked out. reason is appended to the "(was <sha>" part of the message.
// With --dry-run it only reports what it would do.
func deleteBranch(cfg runConfig, branch, oldCommit, reason string, currentBranch *string, defaultBranch string) error {
	if branch == *currentBranch {
		if cfg.dryRun {
			fmt.Printf("Would switch to branch %s.\n", defaultBranch)
		} else {
			if err := runGitSilent(cfg, "checkout", "--quiet", defaultBranch); err != nil {
				fmt.Fprintf(os.Stderr, "warning: couldn't checkout '%s': %s\n", defaultBranch, err.Error())
				return err
			}
			fmt.Printf("Switched to branch %s.\n", defaultBranch)
		}
		// In dry-run mode only the in-memory value changes, so later branches
		// are evaluated as they would be in a real run; HEAD is not touched.
		*currentBranch = defaultBranch
	}
	if cfg.dryRun {
		fmt.Printf("%sWould delete branch %s%s%s (was %s%s).\n", cfg.stdoutColors.red, cfg.stdoutColors.lightRed, branch, cfg.stdoutColors.reset, shortSHA(oldCommit), reason)
		return nil
	}
	if err := runGitSilent(cfg, "branch", "-D", branch); err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't delete '%s': %s\n", branch, err.Error())
		return err
	}
	fmt.Printf("%sDeleted branch %s%s%s (was %s%s).\n", cfg.stdoutColors.red, cfg.stdoutColors.lightRed, branch, cfg.stdoutColors.reset, shortSHA(oldCommit), reason)
	return nil
}

// squashCandidate is a gone branch whose commits are not in the default
// branch. It is deleted only if a pull request merged into the default branch
// has its tip as head commit.
type squashCandidate struct {
	branch string
	tip    string
}

// mergedPRChunkSize caps how many branch names one GraphQL request looks up.
const mergedPRChunkSize = 50

// lookupMergedPRs maps each candidate branch to the number of a pull request
// merged into base whose head commit is the branch tip. Branches without one,
// or whose lookup failed, are absent; the caller keeps them with the usual
// warning. Failures are only reported with --verbose.
func lookupMergedPRs(cfg runConfig, remote Remote, base string, candidates []squashCandidate) map[string]int {
	note := func(format string, args ...any) {
		if cfg.verbose {
			fmt.Fprintf(os.Stderr, "note: "+format+"\n", args...)
		}
	}
	// The upstream ref is gone, so @{upstream} can't be resolved; the branch
	// name on the remote (which may differ from the local name) is still in
	// branch.<name>.merge. Two local branches may track the same one.
	headOf := make(map[string]string, len(candidates))
	var heads []string
	seen := make(map[string]bool)
	for _, c := range candidates {
		if c.tip == "" {
			note("couldn't look up a merged pull request for '%s': couldn't read the branch tip", c.branch)
			continue
		}
		head, err := upstreamBranchName(c.branch)
		if err != nil {
			note("couldn't look up a merged pull request for '%s': %s", c.branch, err.Error())
			continue
		}
		headOf[c.branch] = head
		if !seen[head] {
			seen[head] = true
			heads = append(heads, head)
		}
	}

	found := make(map[string][]mergedPR)
	for start := 0; start < len(heads); start += mergedPRChunkSize {
		chunk := heads[start:min(start+mergedPRChunkSize, len(heads))]
		prs, err := cfg.findMergedPRs(cfg, remote, base, chunk)
		if err != nil {
			note("couldn't look up merged pull requests: %s", err.Error())
			continue
		}
		maps.Copy(found, prs)
	}

	merged := make(map[string]int)
	for _, c := range candidates {
		head, ok := headOf[c.branch]
		if !ok {
			continue
		}
		if pr := matchMergedPR(found[head], c.tip); pr != 0 {
			merged[c.branch] = pr
		}
	}
	return merged
}

// upstreamBranchName returns the remote branch name that branch tracks, read
// from branch.<name>.merge.
func upstreamBranchName(branch string) (string, error) {
	merge, err := gitOutput("config", "--get", "branch."+branch+".merge")
	if err != nil {
		return "", err
	}
	mergeRef := strings.TrimSpace(string(merge))
	name, ok := strings.CutPrefix(mergeRef, "refs/heads/")
	if !ok || name == "" {
		return "", fmt.Errorf("unexpected branch.%s.merge value %q", branch, mergeRef)
	}
	return name, nil
}

// matchMergedPR returns the number of the pull request in prs whose head
// commit is tip, or 0 when there is none. A pull request whose head differs
// (commits were added after the merge) does not count.
func matchMergedPR(prs []mergedPR, tip string) int {
	for _, pr := range prs {
		if pr.HeadRefOid == tip {
			return pr.Number
		}
	}
	return 0
}

// ghTimeout bounds each gh call so a stalled network request falls back to
// the "appears not merged" warning instead of hanging the sync.
const ghTimeout = 30 * time.Second

// findMergedPRsWithGh implements mergedPRFinder with one `gh api graphql`
// request. The query is passed on stdin and every value as a raw string
// variable (-f), so branch names never become part of the query text.
func findMergedPRsWithGh(cfg runConfig, remote Remote, base string, heads []string) (map[string][]mergedPR, error) {
	repo, err := parseGitHubURL(remote.URL)
	if err != nil {
		return nil, err
	}
	repo.host = resolveGitHubHost(cfg, remote.URL, repo.host)
	query, vars := buildMergedPRQuery(repo, base, heads)
	args := []string{"api", "graphql", "--hostname", repo.host, "-F", "query=@-"}
	for _, v := range vars {
		args = append(args, "-f", v.name+"="+v.value)
	}
	verboseLog(cfg, "gh", args)

	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(query)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("gh api graphql: timed out after %s", ghTimeout)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("gh api graphql: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	return parseMergedPRResponse(output, heads)
}

// queryVar is a GraphQL variable passed to gh as -f name=value.
type queryVar struct {
	name  string
	value string
}

// buildMergedPRQuery builds a GraphQL query with one aliased pullRequests
// field per head (b0, b1, ...), restricted to pull requests merged into base,
// most recently updated first. Branch names only appear as variables.
func buildMergedPRQuery(repo gitHubRepo, base string, heads []string) (string, []queryVar) {
	vars := []queryVar{{"owner", repo.owner}, {"name", repo.name}, {"base", base}}
	params := []string{"$owner: String!", "$name: String!", "$base: String!"}
	var fields strings.Builder
	for i, head := range heads {
		vars = append(vars, queryVar{fmt.Sprintf("h%d", i), head})
		params = append(params, fmt.Sprintf("$h%d: String!", i))
		fmt.Fprintf(&fields, "    b%d: pullRequests(headRefName: $h%d, baseRefName: $base, states: MERGED, first: 20, orderBy: {field: UPDATED_AT, direction: DESC}) {\n      nodes { number headRefOid }\n    }\n", i, i)
	}
	query := fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $name) {\n%s  }\n}\n", strings.Join(params, ", "), fields.String())
	return query, vars
}

// parseMergedPRResponse maps the b<i> aliases of a buildMergedPRQuery
// response back to heads[i].
func parseMergedPRResponse(output []byte, heads []string) (map[string][]mergedPR, error) {
	var resp struct {
		Data struct {
			Repository map[string]*struct {
				Nodes []mergedPR `json:"nodes"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(output, &resp); err != nil {
		return nil, fmt.Errorf("unexpected gh api graphql output: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
	}
	if resp.Data.Repository == nil {
		return nil, errors.New("repository not found")
	}
	prs := make(map[string][]mergedPR, len(heads))
	for i, head := range heads {
		field := resp.Data.Repository[fmt.Sprintf("b%d", i)]
		if field == nil {
			return nil, fmt.Errorf("missing b%d in gh api graphql output", i)
		}
		prs[head] = field.Nodes
	}
	return prs, nil
}

// gitHubRepo identifies a repository on a GitHub host (github.com or GitHub
// Enterprise Server).
type gitHubRepo struct {
	host  string
	owner string
	name  string
}

// scpLikeURLRe matches scp-like git URLs such as git@github.com:owner/repo.git.
var scpLikeURLRe = regexp.MustCompile(`^(?:[^@/]+@)?([^/:]+):(.+)$`)

// parseGitHubURL extracts host, owner and repository name from a remote URL
// in https, ssh, git or scp-like form. Local paths and URLs whose path is not
// exactly "<owner>/<repo>" are rejected. Whether the host is actually a GitHub
// host is left to gh, which fails for hosts it isn't authenticated to.
func parseGitHubURL(raw string) (gitHubRepo, error) {
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return gitHubRepo{}, err
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return gitHubRepo{}, fmt.Errorf("not a GitHub repository URL: %q", raw)
		}
		host, path = u.Hostname(), u.Path
	} else if m := scpLikeURLRe.FindStringSubmatch(raw); m != nil && len(m[1]) > 1 {
		// A one-letter "host" is a Windows drive (C:/repo or C:\repo).
		host, path = m[1], m[2]
	} else {
		return gitHubRepo{}, fmt.Errorf("not a GitHub repository URL: %q", raw)
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return gitHubRepo{}, fmt.Errorf("not a GitHub repository URL: %q", raw)
	}
	return gitHubRepo{host: host, owner: parts[0], name: parts[1]}, nil
}

// sshTimeout bounds `ssh -G`, which only reads the SSH config.
const sshTimeout = 5 * time.Second

// resolveGitHubHost returns the host to pass to gh for a remote whose URL
// names host. For an SSH remote the host may be an alias from ~/.ssh/config
// (Host github-work / HostName github.com) or ssh.github.com (SSH over port
// 443), neither of which gh has credentials for, so the real host name is
// read from `ssh -G`. Other remotes, and SSH remotes ssh can't resolve, keep
// host.
func resolveGitHubHost(cfg runConfig, rawURL, host string) string {
	if !isSSHURL(rawURL) {
		return host
	}
	if resolved, ok := cfg.sshHosts[host]; ok {
		return resolved
	}
	var out []byte
	var err error
	if strings.HasPrefix(host, "-") {
		// Never let a host name be read as an ssh option.
		err = fmt.Errorf("invalid host %q", host)
	} else {
		args := []string{"-G", "--", host}
		verboseLog(cfg, "ssh", args)
		ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
		out, err = exec.CommandContext(ctx, "ssh", args...).Output()
		cancel()
	}
	if err != nil && cfg.verbose {
		fmt.Fprintf(os.Stderr, "note: couldn't resolve SSH host '%s': %s\n", host, err.Error())
	}
	resolved := sshResolvedHost(host, out, err)
	if cfg.sshHosts != nil {
		cfg.sshHosts[host] = resolved
	}
	return resolved
}

// sshResolvedHost picks the GitHub host from the output of `ssh -G <host>`:
// its hostname line, or host itself when ssh failed or printed none.
// ssh.github.com is mapped to github.com.
func sshResolvedHost(host string, sshOutput []byte, sshErr error) string {
	resolved := host
	if sshErr == nil {
		for line := range strings.Lines(string(sshOutput)) {
			key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
			if ok && strings.EqualFold(key, "hostname") && value != "" {
				resolved = value
				break
			}
		}
	}
	if strings.EqualFold(resolved, "ssh.github.com") {
		return "github.com"
	}
	return resolved
}

// isSSHURL reports whether a remote URL uses SSH: ssh:// (or git+ssh://,
// ssh+git://) or the scp-like form host:path.
func isSSHURL(raw string) bool {
	if scheme, _, ok := strings.Cut(raw, "://"); ok {
		switch scheme {
		case "ssh", "git+ssh", "ssh+git":
			return true
		}
		return false
	}
	m := scpLikeURLRe.FindStringSubmatch(raw)
	return m != nil && len(m[1]) > 1
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

// runGitSilent runs git and captures its stderr. On failure the captured
// stderr is wrapped into the returned error; callers print that error, so it
// is not printed here as well (even with --verbose).
func runGitSilent(cfg runConfig, args ...string) error {
	verboseLog(cfg, "git", args)
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

// runGitStderr runs git with its stderr connected to os.Stderr, so progress
// and diagnostics reach the user as git writes them. The returned error is
// git's bare exit status: its message has already been shown.
func runGitStderr(cfg runConfig, args ...string) error {
	verboseLog(cfg, "git", args)
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func verboseLog(cfg runConfig, cmd string, args []string) {
	if cfg.verbose {
		msg := fmt.Sprintf("$ %s %s", cmd, strings.Join(args, " "))
		if cfg.stderrColors.magenta != "" {
			msg = fmt.Sprintf("%s%s%s", cfg.stderrColors.magenta, msg, cfg.stderrColors.reset)
		}
		fmt.Fprintln(os.Stderr, msg)
	}
}

// colorizeOutput resolves whether a single output stream should be colorized.
// isTerminal reports whether that stream is a terminal and noColor whether the
// NO_COLOR environment variable (https://no-color.org) is set to a non-empty
// value. run() calls it
// once per stream (stdout and stderr) so each is decided independently.
//
//   - "always": colorize regardless of isTerminal and noColor.
//   - "never":  never colorize.
//   - "auto":   colorize only when the stream is a terminal and NO_COLOR is
//     unset.
func colorizeOutput(flag string, isTerminal, noColor bool) bool {
	switch flag {
	case "always":
		return true
	case "never":
		return false
	default:
		// "auto" and any unrecognized value behave the same; run() rejects
		// unknown values before reaching here.
		return isTerminal && !noColor
	}
}

func printError(colors colorConfig, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s%s%s\n", colors.lightRed, fmt.Sprintf(format, args...), colors.reset)
}
