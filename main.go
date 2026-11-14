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
	"slices"
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
	yellow     string
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
		yellow:     "\033[33m",
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
type runConfig struct {
	stdoutColors  colorConfig
	stderrColors  colorConfig
	verbose       bool
	dryRun        bool
	findMergedPRs mergedPRFinder
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
	}

	if err := checkGitRepo(); err != nil {
		printError(cfg.stderrColors, "fatal: Not a git repository")
		return err
	}

	remote, remotes, err := getMainRemote()
	if err != nil {
		printError(cfg.stderrColors, "%s", err.Error())
		return err
	}

	currentBranch := ""
	if branch, err := getCurrentBranch(); err == nil {
		currentBranch = branch
	}

	branchToRemote, err := getBranchToRemoteMapping()
	if err != nil {
		printError(cfg.stderrColors, "Failed to read branch tracking config: %s", err.Error())
		return err
	}

	if err := fetchRemote(cfg, remote.Name, stderrIsTerminal); err != nil {
		printError(cfg.stderrColors, "Failed to fetch from %s: %s", remote.Name, err.Error())
		return err
	}

	// Remotes that local branches track, such as a fork's origin when the
	// main remote is upstream, are fetched too, so their branches can be
	// updated and their deleted branches detected. A failure here only
	// affects the branches tracking that remote: they are skipped.
	hadFailure := false
	fetched := map[string]bool{remote.Name: true}
	failed := make(map[string]bool)
	for _, r := range trackedRemotes(remotes, remote.Name, branchToRemote) {
		if err := fetchRemote(cfg, r, stderrIsTerminal); err != nil {
			printWarning(cfg.stderrColors, "couldn't fetch %s; skipping the branches that track it: %s", r, err.Error())
			failed[r] = true
			hadFailure = true
			continue
		}
		fetched[r] = true
	}

	// Determined after the fetch so a remote that was never fetched has its
	// remote-tracking refs, and refs pruned by the fetch are not trusted.
	defaultBranch, err := getDefaultBranch(remote)
	if err != nil {
		printError(cfg.stderrColors, "Failed to determine default branch for %s: %s", remote.Name, err.Error())
		return err
	}

	refs, err := readRefs(slices.Sorted(maps.Keys(fetched)))
	if err != nil {
		printError(cfg.stderrColors, "Failed to get local branches: %s", err.Error())
		return err
	}

	fullDefaultBranch := fmt.Sprintf("refs/remotes/%s/%s", remote.Name, defaultBranch)

	var candidates []squashCandidate
	for _, branch := range refs.branches {
		if failed[branchToRemote[branch.name]] {
			continue
		}
		if perr := processBranch(cfg, branch, refs.remoteRefs, remote, fetched, branchToRemote, &currentBranch, defaultBranch, fullDefaultBranch, &candidates); perr != nil {
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
				printWarning(cfg.stderrColors, "'%s' was deleted on %s, but appears not merged into '%s'", c.branch, branchToRemote[c.branch], defaultBranch)
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

// fetchRemote runs `git fetch --prune` for name. --progress only shows
// something when git's stderr reaches the user, so stderr is passed through
// when it is a terminal. Otherwise it is captured, so a failure is reported
// once, inside the returned error.
func fetchRemote(cfg runConfig, name string, stderrIsTerminal bool) error {
	args := []string{"fetch", "--prune", "--quiet", "--progress", name}
	if stderrIsTerminal {
		return runGitStderr(cfg, args...)
	}
	return runGitSilent(cfg, args...)
}

// trackedRemotes returns, sorted, the remotes other than main that some
// local branch tracks. Values of branch.<name>.remote that are not a
// configured remote (such as "." for a local upstream) are left out.
func trackedRemotes(remotes []Remote, main string, branchToRemote map[string]string) []string {
	known := make(map[string]bool, len(remotes))
	for _, r := range remotes {
		known[r.Name] = true
	}
	set := make(map[string]bool)
	for _, r := range branchToRemote {
		if r != main && known[r] {
			set[r] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
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

// getMainRemote returns the main remote and all remotes.
func getMainRemote() (*Remote, []Remote, error) {
	// Priority order: upstream, github, origin, others
	priorityOrder := []string{"upstream", "github", "origin"}

	remotes, err := getRemotes()
	if err != nil {
		return nil, nil, err
	}

	if len(remotes) == 0 {
		return nil, nil, fmt.Errorf("no git remotes found")
	}

	// Check priority remotes first
	for _, priority := range priorityOrder {
		for _, remote := range remotes {
			if remote.Name == priority {
				return &remote, remotes, nil
			}
		}
	}

	// Return first remote if no priority match
	return &remotes[0], remotes, nil
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

// localBranch is a local branch as read by readRefs.
type localBranch struct {
	name string
	// tip is the commit the branch points at.
	tip string
	// upstream is the remote-tracking ref the branch tracks, or "" when git
	// cannot resolve one (no upstream config, a broken one, or an upstream
	// outside the remote's fetch refspec).
	upstream string
	// gone reports that upstream is set but the remote-tracking ref does not
	// exist (it was pruned).
	gone bool
	// ahead and behind count the commits only in the branch and only in
	// upstream. Both are 0 when upstream is "" or gone.
	ahead, behind int
}

// refSnapshot is the state of the local branches and the main remote's
// remote-tracking refs of the fetched remotes, read with one git call.
type refSnapshot struct {
	branches []localBranch
	// remoteRefs holds the full names of the remote-tracking refs read.
	remoteRefs map[string]bool
}

// refFormat is the for-each-ref format readRefs parses: NUL-separated fields,
// one ref per line. Ref names cannot contain NUL or newline.
const refFormat = "%(refname)%00%(objectname)%00%(upstream)%00%(upstream:track)"

// readRefs reads every local branch with its tip, upstream and ahead/behind
// counts, and the remote-tracking refs of remotes, with a single for-each-ref,
// instead of a few git calls per branch.
//
// It reads full ref names rather than `git branch --format=%(refname:short)`,
// which prints "heads/<name>" when a tag has the same name and includes a
// "(HEAD detached at ...)" line when HEAD is detached.
func readRefs(remotes []string) (refSnapshot, error) {
	args := []string{"for-each-ref", "--format=" + refFormat, "refs/heads/"}
	for _, r := range remotes {
		args = append(args, "refs/remotes/"+r+"/")
	}
	cmd := exec.Command("git", args...)
	// %(upstream:track) is translated; the C locale keeps it parseable.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return refSnapshot{}, fmt.Errorf("%w: %s", err, msg)
		}
		return refSnapshot{}, err
	}
	return parseRefs(string(output))
}

// parseRefs parses the output of for-each-ref with refFormat.
func parseRefs(output string) (refSnapshot, error) {
	snap := refSnapshot{remoteRefs: make(map[string]bool)}
	remotePrefix := "refs/remotes/"
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 4 {
			return refSnapshot{}, fmt.Errorf("unexpected for-each-ref output: %q", line)
		}
		refname := fields[0]
		if strings.HasPrefix(refname, remotePrefix) {
			snap.remoteRefs[refname] = true
			continue
		}
		name, ok := strings.CutPrefix(refname, "refs/heads/")
		if !ok || name == "" {
			continue
		}
		b := localBranch{name: name, tip: fields[1], upstream: fields[2]}
		if b.upstream != "" {
			var err error
			b.ahead, b.behind, b.gone, err = parseTrack(fields[3])
			if err != nil {
				return refSnapshot{}, fmt.Errorf("branch '%s': %w", name, err)
			}
		}
		snap.branches = append(snap.branches, b)
	}
	return snap, nil
}

// parseTrack parses %(upstream:track) in the C locale: "" (in sync),
// "[gone]", "[ahead N]", "[behind M]" or "[ahead N, behind M]".
func parseTrack(track string) (ahead, behind int, gone bool, err error) {
	if track == "" {
		return 0, 0, false, nil
	}
	inner, ok := strings.CutPrefix(track, "[")
	if ok {
		inner, ok = strings.CutSuffix(inner, "]")
	}
	if !ok {
		return 0, 0, false, fmt.Errorf("unexpected upstream track %q", track)
	}
	if inner == "gone" {
		return 0, 0, true, nil
	}
	for part := range strings.SplitSeq(inner, ", ") {
		key, num, found := strings.Cut(part, " ")
		n, convErr := strconv.Atoi(num)
		if !found || convErr != nil || n < 0 {
			return 0, 0, false, fmt.Errorf("unexpected upstream track %q", track)
		}
		switch key {
		case "ahead":
			ahead = n
		case "behind":
			behind = n
		default:
			return 0, 0, false, fmt.Errorf("unexpected upstream track %q", track)
		}
	}
	return ahead, behind, false, nil
}

// processBranch synchronizes a single local branch: against its upstream when
// it tracks the main remote or another fetched remote, otherwise against the
// same-named branch on the main remote.
// It returns a non-nil error when an operation it attempted failed (and was
// announced to the user); branches that are simply skipped (no upstream
// configured, already up to date, or diverged with unpushed commits) return
// nil. The error is used purely to drive the process exit code — diagnostic
// messages are printed inline.
//
// A gone branch whose commits are not in the default branch is appended to
// candidates instead; run() decides on those after a pull request lookup.
func processBranch(cfg runConfig, b localBranch, remoteRefs map[string]bool, remote *Remote, fetched map[string]bool, branchToRemote map[string]string, currentBranch *string, defaultBranch, fullDefaultBranch string, candidates *[]squashCandidate) error {
	branch := b.name
	fullBranch := fmt.Sprintf("refs/heads/%s", branch)
	sameName := fmt.Sprintf("refs/remotes/%s/%s", remote.Name, branch)
	remoteBranch := ""
	gone := false
	// ahead and behind are known from for-each-ref when comparing against the
	// upstream; a branch matched by name is compared with rev-list below.
	counted := false
	var ahead, behind int

	if tracked := branchToRemote[branch]; fetched[tracked] {
		// The branch tracks the main remote or another remote fetched above
		// (e.g. a fork's origin): compare it with its own upstream.
		trackedSameName := fmt.Sprintf("refs/remotes/%s/%s", tracked, branch)
		if b.upstream != "" && !b.gone {
			remoteBranch = b.upstream
			ahead, behind, counted = b.ahead, b.behind, true
		} else {
			// While a same-named remote branch still exists, a missing
			// upstream points at broken config rather than a deleted branch:
			// skip it, so a config typo can't lead to deleting it below.
			if remoteRefs[trackedSameName] {
				printWarning(cfg.stderrColors, "'%s' upstream config is broken; skipping", branch)
				return nil
			}
			if b.upstream == "" {
				// git resolves no remote-tracking ref for the upstream:
				// branch.<name>.merge is missing or invalid, or the upstream
				// is outside remote.<name>.fetch (a single-branch or shallow
				// clone, or refs/pull/<N>/head from `gh pr checkout`). The
				// remote branch may well exist, so this is not "gone".
				if cfg.verbose {
					fmt.Fprintf(os.Stderr, "note: '%s' has no remote-tracking ref for its upstream; skipping\n", branch)
				}
				return nil
			}
			// %(upstream:track) is "[gone]": the remote-tracking ref existed
			// and was pruned, so the branch was deleted on the remote.
			gone = true
		}
	} else if remoteRefs[sameName] {
		remoteBranch = sameName
		if branchToRemote[branch] == "" && cfg.verbose {
			// No upstream config, but a same-named ref exists on the main
			// remote. Fast-forward by name match (mirrors hub sync), but emit
			// a hint so the user knows two unrelated branches with the same
			// name are being treated as related.
			fmt.Fprintf(os.Stderr, "note: '%s' has no upstream config; matching by name against %s/%s\n", branch, remote.Name, branch)
		}
	}

	if remoteBranch != "" {
		if !counted {
			var err error
			ahead, behind, err = getCommitDifference(fullBranch, remoteBranch)
			if err != nil {
				printWarning(cfg.stderrColors, "couldn't compare '%s' against '%s': %s", branch, remoteBranch, err.Error())
				return err
			}
		}
		switch {
		case ahead == 0 && behind == 0:
			return nil
		case ahead == 0 && behind > 0:
			oldCommit := b.tip
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
				printWarning(cfg.stderrColors, "couldn't fast-forward '%s': %s", branch, updateErr.Error())
				return updateErr
			}
			fmt.Printf("%sUpdated branch %s%s%s (was %s).\n", cfg.stdoutColors.green, cfg.stdoutColors.lightGreen, branch, cfg.stdoutColors.reset, shortSHA(oldCommit))
		default:
			printWarning(cfg.stderrColors, "'%s' seems to contain unpushed commits", branch)
		}
		return nil
	}

	if !gone {
		return nil
	}

	ahead, _, err := getCommitDifference(fullBranch, fullDefaultBranch)
	if err != nil {
		printWarning(cfg.stderrColors, "couldn't compare '%s' against '%s': %s", branch, fullDefaultBranch, err.Error())
		return err
	}
	if ahead != 0 {
		// The commits are not in the default branch, which is also the case
		// after a squash or rebase merge.
		*candidates = append(*candidates, squashCandidate{branch: branch, tip: b.tip})
		return nil
	}
	return deleteBranch(cfg, branch, b.tip, "", currentBranch, defaultBranch)
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
				printWarning(cfg.stderrColors, "couldn't checkout '%s': %s", defaultBranch, err.Error())
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
		printWarning(cfg.stderrColors, "couldn't delete '%s': %s", branch, err.Error())
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

// printWarning prints "warning: <message>" to stderr, in yellow when colors
// are enabled.
func printWarning(colors colorConfig, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%swarning: %s%s\n", colors.yellow, fmt.Sprintf(format, args...), colors.reset)
}
