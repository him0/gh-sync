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
	"path"
	"path/filepath"
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
//
// sshHosts caches the host names resolved with `ssh -G`, so ssh runs at most
// once per host per sync. ghHosts does the same for checkGhHost.
//
// removeWorktrees makes deleteBranch remove the worktree of a branch it is
// about to delete, when git agrees to without --force, instead of keeping the
// branch. It is opt-in (--remove-worktrees or gh-sync.removeWorktrees).
//
// report records what was printed, so run() can tell whether the sync left
// everything as it was, and collects the branches skipped because of another
// worktree, which are reported together at the end.
type runConfig struct {
	stdoutColors    colorConfig
	stderrColors    colorConfig
	verbose         bool
	dryRun          bool
	removeWorktrees bool
	findMergedPRs   mergedPRFinder
	sshHosts        map[string]string
	ghHosts         map[string]bool
	report          *syncReport
}

// syncReport is shared by every copy of a runConfig, so processBranch and
// deleteBranch can add to it without another parameter.
type syncReport struct {
	changed bool // a branch update, deletion or switch was printed
	warned  bool // a warning was printed
	// notDeleted and notUpdated are the branches that would have been deleted
	// or fast-forwarded but are checked out in another worktree, in the order
	// they were found. printWorktreeSkips reports them.
	notDeleted []worktreeSkip
	notUpdated []worktreeSkip
	// worktrees is read by worktreeInfo on first use, so `git worktree list`
	// runs at most once per sync, and only when a worktree is in the way.
	worktrees *worktreeInfo
}

// worktreeInfo is what gh-sync needs to know about the repository's
// worktrees: the path of the main one ("" when it can't be determined) and
// which ones are locked, by path.
type worktreeInfo struct {
	main   string
	locked map[string]bool
}

// worktreeInfo returns the repository's worktrees as they were when it was
// first called. Removing a worktree later doesn't change what callers use it
// for: the main worktree stays, and a removed one is not looked up again.
func (r *syncReport) worktreeInfo() worktreeInfo {
	if r.worktrees == nil {
		info := readWorktreeInfo()
		r.worktrees = &info
	}
	return *r.worktrees
}

// worktreeSkip is a branch left alone because it is checked out in another
// worktree, at path.
type worktreeSkip struct {
	branch string
	path   string
}

// upToDate reports whether the sync changed nothing and has nothing to
// report. A branch that could not be fast-forwarded because of its worktree
// is only a note, not a warning, but the repository is not up to date.
func (r *syncReport) upToDate() bool {
	return !r.changed && !r.warned && len(r.notDeleted) == 0 && len(r.notUpdated) == 0
}

// mergedPR is a merged pull request, the commit its head pointed to and its
// commits (at most the last mergedPRCommitLimit of them).
type mergedPR struct {
	Number     int
	HeadRefOid string
	Commits    []string
}

// mergedPRFinder returns, for each name in heads, the pull requests merged
// into base in remote's repository from a branch with that name. Branch names
// are matched without an owner, so pull requests opened from forks are
// included; the caller only accepts one that contains the local tip (see
// matchMergedPR). An error means the lookup could not be done (gh missing or
// not authenticated, a remote that is not on GitHub, an API error, a
// timeout).
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
		removeFlag  bool
		colorFlag   string
		remoteFlag  string
		excludeFlag stringList
		showVersion bool
	)
	fs.BoolVar(&verbose, "verbose", false, "verbose output")
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be updated or deleted without changing branches")
	fs.BoolVar(&removeFlag, "remove-worktrees", false, "remove the clean worktree of a branch to delete, then delete the branch")
	fs.StringVar(&colorFlag, "color", "auto", "colorize output (always, never, auto)")
	fs.StringVar(&remoteFlag, "remote", "", "sync against this remote instead of the automatically chosen one")
	fs.Var(&excludeFlag, "exclude", "skip local branches matching this glob (repeatable)")
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
		ghHosts:       make(map[string]bool),
		report:        &syncReport{},
	}

	if err := checkGitRepo(); err != nil {
		printError(cfg.stderrColors, "fatal: Not a git repository")
		return err
	}

	excludes, err := getExcludePatterns(excludeFlag)
	if err != nil {
		printError(cfg.stderrColors, "fatal: %s", err.Error())
		return err
	}

	removeGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "remove-worktrees" {
			removeGiven = true
		}
	})
	cfg.removeWorktrees, err = getRemoveWorktrees(removeFlag, removeGiven)
	if err != nil {
		printError(cfg.stderrColors, "fatal: %s", err.Error())
		return err
	}

	remote, remotes, err := getMainRemote(remoteFlag)
	if err != nil {
		printError(cfg.stderrColors, "%s", err.Error())
		return err
	}
	if err := checkRemoteName(remote.Name); err != nil {
		printError(cfg.stderrColors, "fatal: %s", err.Error())
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
			printWarning(cfg, "couldn't fetch %s; skipping the branches that track it: %s", r, err.Error())
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
		if pattern, ok := matchExclude(excludes, branch.name); ok {
			printNote(cfg, "skipping '%s' (excluded by '%s')", branch.name, pattern)
			continue
		}
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
		matches := lookupMergedPRs(cfg, *remote, defaultBranch, candidates)
		for _, c := range candidates {
			m := matches[c.branch]
			if m.outcome != prMerged {
				printWarning(cfg, "'%s' was deleted on %s, but %s; not deleted", c.branch, branchToRemote[c.branch], keptReason(c, m, defaultBranch))
				continue
			}
			if derr := deleteBranch(cfg, c.branch, c.tip, c.worktreePath, fmt.Sprintf(", merged via #%d", m.number), &currentBranch, remote.Name, defaultBranch); derr != nil {
				hadFailure = true
			}
		}
	}

	printWorktreeSkips(cfg)

	if hadFailure {
		return fmt.Errorf("one or more branches failed to sync")
	}
	// stdout lists only what changed, so scripts can test it for emptiness.
	// The message is for someone at a terminal, like fetch progress.
	if stderrIsTerminal && cfg.report.upToDate() {
		fmt.Fprintln(os.Stderr, "Already up to date.")
	}
	return nil
}

// fetchRemote runs `git fetch --prune` for name. --progress only shows
// something when git's stderr reaches the user, so stderr is passed through
// when it is a terminal. Otherwise it is captured, so a failure is reported
// once, inside the returned error. A name checkRemoteName rejects is not
// passed to git.
func fetchRemote(cfg runConfig, name string, stderrIsTerminal bool) error {
	if err := checkRemoteName(name); err != nil {
		return err
	}
	args := []string{"fetch", "--prune", "--quiet", "--progress", name}
	if stderrIsTerminal {
		return runGitStderr(cfg, args...)
	}
	return runGitSilent(cfg, args...)
}

// checkRemoteName rejects a remote name git would read as an option: git
// allows names such as "--upload-pack=<command>", and `git fetch` given one
// runs that command. --end-of-options would need git 2.24.
func checkRemoteName(name string) error {
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("remote name '%s' starts with '-'", name)
	}
	return nil
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
  --remove-worktrees
        remove the clean worktree of a branch to delete, then delete the branch
  --color string
        colorize output (always, never, auto) (default "auto")
  --remote name
        sync against this remote instead of the automatically chosen one
  --exclude glob
        skip local branches matching this glob (repeatable)
  --version
        print version and exit
`

func printUsage(w io.Writer) {
	// Like the other writes to stdout/stderr, a failed write has nowhere
	// better to be reported.
	_, _ = fmt.Fprint(w, usageText)
}

// stringList is a flag.Value that collects every occurrence of a repeatable
// flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// getExcludePatterns returns the branch exclude patterns: every
// gh-sync.exclude value from git config followed by the --exclude flags.
// Both sources add to the set; neither replaces the other. A pattern that
// path.Match cannot parse is an error, so a typo doesn't silently match
// nothing.
func getExcludePatterns(flags []string) ([]string, error) {
	var patterns []string
	output, err := gitOutput("config", "--get-all", "gh-sync.exclude")
	if err != nil && !isExitCode(err, 1) {
		return nil, fmt.Errorf("couldn't read gh-sync.exclude: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			patterns = append(patterns, p)
		}
	}
	patterns = append(patterns, flags...)
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("invalid exclude pattern %q: %w", p, err)
		}
	}
	return patterns, nil
}

// getRemoveWorktrees returns whether worktrees may be removed: the
// --remove-worktrees flag when it was given, with either value, and otherwise
// the gh-sync.removeWorktrees git config, which defaults to false. A config
// value git can't read as a boolean is an error, so a typo doesn't silently
// leave the option off.
//
// The config is not read when the flag was given, so the flag also works as a
// way around a broken config value.
func getRemoveWorktrees(flagValue, flagGiven bool) (bool, error) {
	if flagGiven {
		return flagValue, nil
	}
	stdout, stderr, err := gitOutputC("config", "--type=bool", "--get", "gh-sync.removeWorktrees")
	if isExitCode(err, 1) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("couldn't read gh-sync.removeWorktrees: %s", gitReason(stderr, err))
	}
	return strings.TrimSpace(stdout) == "true", nil
}

// matchExclude returns the first pattern in patterns that matches branch.
// Patterns use path.Match syntax, so "*" does not cross a "/": "wip/*"
// matches "wip/foo" but not "wip/foo/bar".
func matchExclude(patterns []string, branch string) (string, bool) {
	for _, p := range patterns {
		// Patterns were validated by getExcludePatterns.
		if ok, _ := path.Match(p, branch); ok {
			return p, true
		}
	}
	return "", false
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
	return commandOutput(exec.Command("git", args...))
}

// commandOutput runs cmd and returns its stdout. On failure the captured
// stderr is wrapped into the error.
func commandOutput(cmd *exec.Cmd) ([]byte, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
	}
	return out, err
}

// gitOutputC runs git in the C locale and returns its stdout and stderr
// separately, for callers that read git's own message: to recognize it, or to
// show it without the "exit status N" that gitOutput puts in front.
func gitOutputC(args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	return string(out), errBuf.String(), err
}

// gitReason returns the first line of git's error output without its
// "fatal: " or "error: " prefix, to embed in a message of gh-sync's own. When
// git printed nothing it falls back to err.
func gitReason(stderr string, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	line = strings.TrimSpace(line)
	for _, prefix := range []string{"fatal: ", "error: "} {
		line = strings.TrimPrefix(line, prefix)
	}
	if line == "" {
		return err.Error()
	}
	return line
}

// isExitCode reports whether err is a command that exited with code.
func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func checkGitRepo() error {
	return exec.Command("git", "rev-parse", "--git-dir").Run()
}

// getMainRemote returns the remote named name, or when name is empty the
// first of upstream, github and origin that exists, else the first remote.
// It also returns all remotes.
func getMainRemote(name string) (*Remote, []Remote, error) {
	// Priority order: upstream, github, origin, others
	priorityOrder := []string{"upstream", "github", "origin"}

	remotes, err := getRemotes()
	if err != nil {
		return nil, nil, err
	}

	if name != "" {
		for _, remote := range remotes {
			if remote.Name == name {
				return &remote, remotes, nil
			}
		}
		return nil, nil, fmt.Errorf("fatal: no such remote '%s'", name)
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
		if strings.HasPrefix(ref, prefix) && refExists(ref) {
			return strings.TrimPrefix(ref, prefix), nil
		}
	}

	// Otherwise fall back to main, then master.
	if refExists(fmt.Sprintf("refs/remotes/%s/main", remote.Name)) {
		return "main", nil
	}
	if refExists(fmt.Sprintf("refs/remotes/%s/master", remote.Name)) {
		return "master", nil
	}

	return "", fmt.Errorf("could not determine default branch (no remote HEAD, no main, no master on %s); set it with `git remote set-head %s --auto`", remote.Name, remote.Name)
}

// getCurrentBranch returns the name of the checked-out branch. It strips
// "refs/heads/" itself rather than using `symbolic-ref --short`, which prints
// "heads/<name>" when a tag has the same name, so the result matches the
// names returned by readRefs.
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

	output, err := gitOutput("config", "--get-regexp", `^branch\..*\.remote$`)
	if err != nil {
		// git config exits 1 when no entries match — that's expected for a
		// fresh repo with no tracked branches and is not a failure.
		if isExitCode(err, 1) {
			return branchToRemote, nil
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
	// worktreePath is the worktree the branch is checked out in, or "" when
	// it is not checked out. For the current worktree's branch it is the
	// current worktree.
	worktreePath string
}

// refSnapshot is the state of the local branches and of the remote-tracking
// refs of the fetched remotes, read with one git call.
type refSnapshot struct {
	branches []localBranch
	// remoteRefs holds the full names of those remote-tracking refs.
	remoteRefs map[string]bool
}

// refFormat is the for-each-ref format readRefs parses: NUL-separated fields,
// one ref per line. Ref names cannot contain NUL or newline. %(worktreepath)
// needs git 2.23 or later.
const refFormat = "%(refname)%00%(objectname)%00%(upstream)%00%(upstream:track)%00%(worktreepath)"

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
	output, err := commandOutput(cmd)
	if err != nil {
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
		if len(fields) != 5 {
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
		b := localBranch{name: name, tip: fields[1], upstream: fields[2], worktreePath: fields[4]}
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
				printWarning(cfg, "'%s' upstream config is broken; skipping", branch)
				return nil
			}
			if b.upstream == "" {
				// git resolves no remote-tracking ref for the upstream:
				// branch.<name>.merge is missing or invalid, or the upstream
				// is outside remote.<name>.fetch (a single-branch or shallow
				// clone, or refs/pull/<N>/head from `gh pr checkout`). The
				// remote branch may well exist, so this is not "gone".
				printNote(cfg, "'%s' has no remote-tracking ref for its upstream; skipping", branch)
				return nil
			}
			// %(upstream:track) is "[gone]": the remote-tracking ref existed
			// and was pruned, so the branch was deleted on the remote.
			gone = true
		}
	} else if remoteRefs[sameName] {
		remoteBranch = sameName
		if branchToRemote[branch] == "" {
			// No upstream config, but a same-named ref exists on the main
			// remote. Fast-forward by name match (mirrors hub sync), but emit
			// a hint so the user knows two unrelated branches with the same
			// name are being treated as related.
			printNote(cfg, "'%s' has no upstream config; matching by name against %s/%s", branch, remote.Name, branch)
		}
	}

	if remoteBranch != "" {
		if !counted {
			var err error
			ahead, behind, err = getCommitDifference(fullBranch, remoteBranch)
			if err != nil {
				printWarning(cfg, "couldn't compare '%s' against '%s': %s", branch, remoteBranch, err.Error())
				return err
			}
		}
		switch {
		case ahead == 0 && behind == 0:
			return nil
		case ahead == 0 && behind > 0:
			oldCommit := b.tip
			if inOtherWorktree(branch, b.worktreePath, *currentBranch) {
				// git refuses to move it; report it instead of failing.
				cfg.report.notUpdated = append(cfg.report.notUpdated, worktreeSkip{branch: branch, path: b.worktreePath})
				return nil
			}
			if cfg.dryRun {
				printChange(cfg, "%sWould update branch %s%s%s (was %s).\n", cfg.stdoutColors.green, cfg.stdoutColors.lightGreen, branch, cfg.stdoutColors.reset, shortSHA(oldCommit))
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
				printWarning(cfg, "couldn't fast-forward '%s': %s", branch, updateErr.Error())
				return updateErr
			}
			printChange(cfg, "%sUpdated branch %s%s%s (was %s).\n", cfg.stdoutColors.green, cfg.stdoutColors.lightGreen, branch, cfg.stdoutColors.reset, shortSHA(oldCommit))
		default:
			against := strings.TrimPrefix(remoteBranch, "refs/remotes/")
			if behind == 0 {
				printWarning(cfg, "'%s' has %s not in '%s'; not updated", branch, pluralize(ahead, "commit", "commits"), against)
			} else {
				printWarning(cfg, "'%s' has diverged from '%s' (%d ahead, %d behind); not updated", branch, against, ahead, behind)
			}
		}
		return nil
	}

	if !gone {
		return nil
	}

	ahead, _, err := getCommitDifference(fullBranch, fullDefaultBranch)
	if err != nil {
		printWarning(cfg, "couldn't compare '%s' against '%s': %s", branch, fullDefaultBranch, err.Error())
		return err
	}
	if ahead != 0 {
		// The commits are not in the default branch, which is also the case
		// after a squash or rebase merge.
		*candidates = append(*candidates, squashCandidate{branch: branch, tip: b.tip, worktreePath: b.worktreePath, ahead: ahead})
		return nil
	}
	return deleteBranch(cfg, branch, b.tip, b.worktreePath, "", currentBranch, remote.Name, defaultBranch)
}

// inOtherWorktree reports whether branch is checked out (at worktreePath) in
// a worktree other than the current one, whose branch is currentBranch.
func inOtherWorktree(branch, worktreePath, currentBranch string) bool {
	return worktreePath != "" && branch != currentBranch
}

// deleteBranch deletes branch, checking out defaultBranch first when branch
// is checked out. reason is appended to the "(was <sha>" part of the message.
// With --dry-run it only reports what it would do.
//
// A branch checked out in another worktree (worktreePath) can't be deleted
// while that worktree exists. By default it is kept and recorded in the
// report for the warning that printWorktreeSkips prints. With
// cfg.removeWorktrees the worktree is removed first; when it can't be, the
// branch is kept with a warning of its own, which is a skip and not a
// failure. The main worktree can't be removed at all (gh-sync may be running
// in a linked one), so a branch checked out there is recorded as by default.
func deleteBranch(cfg runConfig, branch, oldCommit, worktreePath, reason string, currentBranch *string, remoteName, defaultBranch string) error {
	if inOtherWorktree(branch, worktreePath, *currentBranch) {
		if !cfg.removeWorktrees || worktreePath == cfg.report.worktreeInfo().main {
			cfg.report.notDeleted = append(cfg.report.notDeleted, worktreeSkip{branch: branch, path: worktreePath})
			return nil
		}
		if !removeWorktree(cfg, branch, worktreePath) {
			return nil
		}
	}
	if branch == *currentBranch {
		if cfg.dryRun {
			printChange(cfg, "Would switch to branch %s.\n", defaultBranch)
		} else {
			if err := runGitSilent(cfg, checkoutDefaultBranchArgs(remoteName, defaultBranch)...); err != nil {
				printWarning(cfg, "couldn't checkout '%s': %s", defaultBranch, err.Error())
				return err
			}
			printChange(cfg, "Switched to branch %s.\n", defaultBranch)
		}
		// In dry-run mode only the in-memory value changes, so later branches
		// are evaluated as they would be in a real run; HEAD is not touched.
		*currentBranch = defaultBranch
	}
	if cfg.dryRun {
		printChange(cfg, "%sWould delete branch %s%s%s (was %s%s).\n", cfg.stdoutColors.red, cfg.stdoutColors.lightRed, branch, cfg.stdoutColors.reset, shortSHA(oldCommit), reason)
		return nil
	}
	if err := runGitSilent(cfg, "branch", "-D", branch); err != nil {
		printWarning(cfg, "couldn't delete '%s': %s", branch, err.Error())
		return err
	}
	printChange(cfg, "%sDeleted branch %s%s%s (was %s%s).\n", cfg.stdoutColors.red, cfg.stdoutColors.lightRed, branch, cfg.stdoutColors.reset, shortSHA(oldCommit), reason)
	return nil
}

// removeWorktree removes the worktree at path, in which branch is checked
// out, and reports whether the branch can now be deleted. It prints the
// removal, or a warning saying why the worktree, and so the branch, is kept.
// With --dry-run it removes nothing and predicts the outcome.
//
// The removal is left to `git worktree remove` without --force, which
// refuses a worktree with modified or untracked files and a locked one. It
// does delete ignored files, and it can't know whether a shell or an agent is
// still running in the worktree; that is why removing worktrees is opt-in.
func removeWorktree(cfg runConfig, branch, path string) bool {
	info := cfg.report.worktreeInfo()
	// Worked out before the removal: symlinks can't be resolved afterwards.
	shown := displayWorktreePath(path, info.main)
	var refusal string
	if cfg.dryRun {
		refusal = predictRemoveRefusal(path, info.locked[path])
	} else {
		// path is absolute, but `--` makes sure it is never read as an option.
		args := []string{"worktree", "remove", "--", path}
		verboseLog(cfg, "git", args)
		if _, stderr, err := gitOutputC(args...); err != nil {
			refusal = removeRefusal(stderr, err)
		}
	}
	if refusal != "" {
		printWarning(cfg, "'%s' is checked out in %s, which %s; not deleted", branch, shown, refusal)
		return false
	}
	if cfg.dryRun {
		printChange(cfg, "Would remove worktree %s.\n", shown)
	} else {
		printChange(cfg, "Removed worktree %s.\n", shown)
	}
	return true
}

// The reasons a worktree is kept that gh-sync words itself. They complete
// "'<branch>' is checked out in <path>, which ...".
const (
	refusalDirty  = "has modified or untracked files"
	refusalLocked = "is locked"
)

// removeRefusal turns the error output of a failed `git worktree remove` (in
// the C locale) into the reason for the warning. The two refusals gh-sync
// expects get its own wording; anything else (submodules, a broken worktree)
// is passed on as the first line of git's message. git's advice to use
// --force is never repeated: the user should look at the worktree first.
func removeRefusal(stderr string, err error) string {
	switch {
	case strings.Contains(stderr, "contains modified or untracked files"):
		return refusalDirty
	case strings.Contains(stderr, "cannot remove a locked working tree"):
		return refusalLocked
	}
	reason := gitReason(stderr, err)
	if i := strings.Index(reason, ", use --force"); i >= 0 {
		reason = reason[:i]
	}
	return "couldn't be removed: " + reason
}

// predictRemoveRefusal returns, for --dry-run, the reason `git worktree
// remove` would refuse the worktree at path, or "" when it is expected to
// remove it. It checks what is cheap to check, in git's order: the lock, then
// the same `git status` git runs.
//
// It can't see everything: git also refuses a worktree with initialized
// submodules, or one whose administrative files are broken, and the worktree
// can change before the real run.
func predictRemoveRefusal(path string, locked bool) string {
	if locked {
		return refusalLocked
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// git removes the record of a worktree whose directory is gone.
		return ""
	}
	stdout, stderr, err := gitOutputC("-C", path, "status", "--porcelain", "--ignore-submodules=none")
	if err != nil {
		return "couldn't be checked: " + gitReason(stderr, err)
	}
	if strings.TrimSpace(stdout) != "" {
		return refusalDirty
	}
	return ""
}

// printWorktreeSkips reports the branches that were not deleted or not
// updated because they are checked out in another worktree. One message per
// kind, instead of one per branch, keeps the output short in a repository
// with many worktrees.
//
// A merged branch that is still checked out is leftover to clean up, so it is
// a warning with the branches and their worktrees, and a hint. A branch that
// is merely behind needs no action (its worktree may be in use), so it is a
// note with the count; the list is printed with --verbose.
//
// With cfg.removeWorktrees the only branches not deleted here are those
// checked out in the main worktree, so the hint is about that instead.
func printWorktreeSkips(cfg runConfig) {
	r := cfg.report
	if len(r.notDeleted) == 0 && len(r.notUpdated) == 0 {
		return
	}
	mainWorktree := r.worktreeInfo().main
	if n := len(r.notDeleted); n > 0 {
		printWarning(cfg, "%s; not deleted:\n%s",
			pluralize(n, "merged branch is still checked out in another worktree", "merged branches are still checked out in other worktrees"),
			strings.Join(worktreeSkipLines(r.notDeleted, mainWorktree), "\n"))
		if cfg.removeWorktrees {
			fmt.Fprintln(os.Stderr, "hint: the main worktree can't be removed; check out another branch in it, then run gh sync again")
		} else {
			fmt.Fprintln(os.Stderr, "hint: remove a worktree with `git worktree remove <path>`, or run `gh sync --remove-worktrees` to remove the clean ones")
		}
	}
	if n := len(r.notUpdated); n > 0 {
		printNotice("%s; not updated",
			pluralize(n, "branch checked out in another worktree is behind its upstream", "branches checked out in other worktrees are behind their upstream"))
		if cfg.verbose {
			for _, line := range worktreeSkipLines(r.notUpdated, mainWorktree) {
				fmt.Fprintln(os.Stderr, line)
			}
		}
	}
}

// worktreeSkipLines formats skips as indented lines of branch and worktree
// path, with the branch names padded so the paths line up.
func worktreeSkipLines(skips []worktreeSkip, mainWorktree string) []string {
	width := 0
	for _, s := range skips {
		width = max(width, len([]rune(s.branch)))
	}
	lines := make([]string, 0, len(skips))
	for _, s := range skips {
		lines = append(lines, fmt.Sprintf("  %-*s   %s", width, s.branch, displayWorktreePath(s.path, mainWorktree)))
	}
	return lines
}

// readWorktreeInfo reads `git worktree list --porcelain`: one block of lines
// per worktree, the main one first, each starting with "worktree <path>" and
// containing "locked" or "locked <reason>" when it is locked. When the list
// can't be read, nothing is known: no main worktree and no locks.
func readWorktreeInfo() worktreeInfo {
	info := worktreeInfo{locked: make(map[string]bool)}
	output, err := gitOutput("worktree", "list", "--porcelain")
	if err != nil {
		return info
	}
	current, first := "", true
	for line := range strings.SplitSeq(string(output), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			current = path
			if first {
				info.main, first = path, false
			}
		} else if line == "locked" || strings.HasPrefix(line, "locked ") {
			info.locked[current] = true
		}
	}
	return info
}

// displayWorktreePath returns path relative to mainWorktree when it is inside
// it (such as .claude/worktrees/<name>), which is shorter. Any other path,
// and every path when mainWorktree is "", is returned as it is.
//
// Symlinks are resolved before comparing, so /tmp and /private/tmp on macOS
// count as the same directory.
func displayWorktreePath(path, mainWorktree string) string {
	if mainWorktree == "" {
		return path
	}
	rel, err := filepath.Rel(resolveSymlinks(mainWorktree), resolveSymlinks(path))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return path
	}
	return rel
}

// resolveSymlinks returns path with its symlinks resolved, or path itself
// when that fails (for example when it no longer exists).
func resolveSymlinks(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// checkoutDefaultBranchArgs returns the git arguments that switch to
// defaultBranch. Without a local branch of that name, a plain checkout makes
// git guess the remote-tracking branch to create it from, which fails when
// several remotes have one (a fork with origin and upstream). The local branch
// is then created explicitly from the main remote instead.
func checkoutDefaultBranchArgs(remoteName, defaultBranch string) []string {
	if refExists("refs/heads/" + defaultBranch) {
		return []string{"checkout", "--quiet", defaultBranch}
	}
	return []string{"checkout", "--quiet", "-b", defaultBranch, "--track", "refs/remotes/" + remoteName + "/" + defaultBranch}
}

// squashCandidate is a gone branch whose commits are not in the default
// branch. It is deleted only if a pull request merged into the default branch
// contains its tip (see matchMergedPR).
type squashCandidate struct {
	branch       string
	tip          string
	worktreePath string
	ahead        int // commits that are not in the default branch
}

// prOutcome says what the pull request lookup found for a candidate branch.
type prOutcome int

const (
	// prNoMatch: no merged pull request contains the tip or has a head that
	// is an ancestor of it. The branch is kept.
	prNoMatch prOutcome = iota
	// prMerged: the tip is the head or one of the commits of a merged pull
	// request. The branch is deleted.
	prMerged
	// prCommitsAdded: the tip descends from the head of a merged pull
	// request, so commits were added locally after the merge. The branch is
	// kept.
	prCommitsAdded
	// prLookupFailed: the pull requests could not be looked up. The branch
	// is kept.
	prLookupFailed
)

// prMatch is the result of the pull request lookup for one candidate branch.
type prMatch struct {
	outcome prOutcome
	number  int // the pull request, for prMerged and prCommitsAdded
	added   int // commits after the pull request head, for prCommitsAdded
}

// mergedPRChunkSize caps how many branch names one GraphQL request looks up.
const mergedPRChunkSize = 50

// lookupMergedPRs returns, for each candidate branch, how it relates to the
// pull requests merged into base from its upstream branch.
//
// A failed request is reported here with a warning, once per distinct reason,
// so the reason precedes the per-branch warnings the caller prints. A branch
// whose upstream name can't be read is only reported with --verbose: that is
// a problem with the branch's config, not with the lookup as a whole.
func lookupMergedPRs(cfg runConfig, remote Remote, base string, candidates []squashCandidate) map[string]prMatch {
	// The upstream ref is gone, so @{upstream} can't be resolved; the branch
	// name on the remote (which may differ from the local name) is still in
	// branch.<name>.merge. Two local branches may track the same one.
	headOf := make(map[string]string, len(candidates))
	var heads []string
	seen := make(map[string]bool)
	for _, c := range candidates {
		head, err := upstreamBranchName(c.branch)
		if err != nil {
			printNote(cfg, "couldn't look up a merged pull request for '%s': %s", c.branch, err.Error())
			continue
		}
		headOf[c.branch] = head
		if !seen[head] {
			seen[head] = true
			heads = append(heads, head)
		}
	}

	// found gets an entry for every head of a chunk that was looked up, even
	// when it has no pull requests, so a missing entry means a failed lookup.
	found := make(map[string][]mergedPR)
	reported := make(map[string]bool)
	for start := 0; start < len(heads); start += mergedPRChunkSize {
		chunk := heads[start:min(start+mergedPRChunkSize, len(heads))]
		prs, err := cfg.findMergedPRs(cfg, remote, base, chunk)
		if err != nil {
			if reason := err.Error(); !reported[reason] {
				reported[reason] = true
				printWarning(cfg, "couldn't look up merged pull requests: %s", reason)
			}
			continue
		}
		for _, head := range chunk {
			found[head] = prs[head]
		}
	}

	matches := make(map[string]prMatch, len(candidates))
	for _, c := range candidates {
		head, ok := headOf[c.branch]
		if !ok {
			matches[c.branch] = prMatch{outcome: prLookupFailed}
			continue
		}
		prs, ok := found[head]
		if !ok {
			matches[c.branch] = prMatch{outcome: prLookupFailed}
			continue
		}
		matches[c.branch] = matchMergedPR(prs, c.tip)
	}
	return matches
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

// matchMergedPR decides what the merged pull requests prs mean for a branch
// at tip.
//
// The branch counts as merged when tip is the head of a pull request or one
// of its commits. In the second case the local branch is merely behind the
// pull request head (GitHub's "Update branch", or a commit pushed from
// elsewhere before the merge): every local commit is part of the merged pull
// request, so deleting is as safe as fast-forwarding first would have been.
//
// Otherwise, a pull request whose head is an ancestor of tip means commits
// were added locally after the merge; the first such pull request is
// reported. A merged match on any pull request wins over that.
func matchMergedPR(prs []mergedPR, tip string) prMatch {
	for _, pr := range prs {
		if pr.HeadRefOid == tip || slices.Contains(pr.Commits, tip) {
			return prMatch{outcome: prMerged, number: pr.Number}
		}
	}
	for _, pr := range prs {
		if added := commitsAddedAfter(pr.HeadRefOid, tip); added > 0 {
			return prMatch{outcome: prCommitsAdded, number: pr.Number, added: added}
		}
	}
	return prMatch{outcome: prNoMatch}
}

// commitsAddedAfter returns how many commits tip has on top of head, or 0
// when head is not an ancestor of tip. A head that is missing locally can't
// be an ancestor of tip, and is not fetched to find out.
func commitsAddedAfter(head, tip string) int {
	// head comes from the API response: don't let git read it as an option.
	if head == "" || strings.HasPrefix(head, "-") {
		return 0
	}
	// In a partial clone, asking for a missing object makes git fetch it
	// from the promisor remote; GIT_NO_LAZY_FETCH (Git 2.45+) prevents that.
	exists := exec.Command("git", "cat-file", "-e", head+"^{commit}")
	exists.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	if exists.Run() != nil {
		return 0
	}
	if exec.Command("git", "merge-base", "--is-ancestor", head, tip).Run() != nil {
		return 0
	}
	output, err := gitOutput("rev-list", "--count", head+".."+tip, "--")
	if err != nil {
		return 0
	}
	added, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0
	}
	return added
}

// keptReason returns the part of the warning that says why candidate c is
// kept, for a pull request lookup result m other than prMerged.
func keptReason(c squashCandidate, m prMatch, defaultBranch string) string {
	notInDefault := fmt.Sprintf("%s not in '%s'", pluralize(c.ahead, "commit is", "commits are"), defaultBranch)
	switch m.outcome {
	case prCommitsAdded:
		return fmt.Sprintf("%s added after it was merged via #%d", pluralize(m.added, "commit was", "commits were"), m.number)
	case prLookupFailed:
		return notInDefault + " and pull requests couldn't be looked up"
	default:
		return notInDefault + " and no merged pull request matches it"
	}
}

// pluralize returns "<n> <singular>" when n is 1 and "<n> <plural>"
// otherwise.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// ghTimeout bounds each gh call so a stalled network request ends as a failed
// lookup, which keeps the branch with a warning, instead of hanging the sync.
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
	if err := checkGhHost(cfg, repo.host); err != nil {
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
	output, err := commandOutput(cmd)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("gh api graphql: timed out after %s", ghTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	return parseMergedPRResponse(output, heads)
}

// queryVar is a GraphQL variable passed to gh as -f name=value.
type queryVar struct {
	name  string
	value string
}

// mergedPRCommitLimit caps how many commits are requested per pull request,
// counted back from its head. A local tip further behind than that is not
// recognized as part of the pull request, and the branch is kept.
const mergedPRCommitLimit = 100

// buildMergedPRQuery builds a GraphQL query with one aliased pullRequests
// field per head (b0, b1, ...), restricted to pull requests merged into base,
// most recently updated first, each with its last commits. Branch names only
// appear as variables.
func buildMergedPRQuery(repo gitHubRepo, base string, heads []string) (string, []queryVar) {
	vars := []queryVar{{"owner", repo.owner}, {"name", repo.name}, {"base", base}}
	params := []string{"$owner: String!", "$name: String!", "$base: String!"}
	var fields strings.Builder
	for i, head := range heads {
		vars = append(vars, queryVar{fmt.Sprintf("h%d", i), head})
		params = append(params, fmt.Sprintf("$h%d: String!", i))
		fmt.Fprintf(&fields, "    b%d: pullRequests(headRefName: $h%d, baseRefName: $base, states: MERGED, first: 20, orderBy: {field: UPDATED_AT, direction: DESC}) {\n      nodes { number headRefOid commits(last: %d) { nodes { commit { oid } } } }\n    }\n", i, i, mergedPRCommitLimit)
	}
	query := fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $name) {\n%s  }\n}\n", strings.Join(params, ", "), fields.String())
	return query, vars
}

// mergedPRNode is a pull request in the shape buildMergedPRQuery asks for.
type mergedPRNode struct {
	Number     int    `json:"number"`
	HeadRefOid string `json:"headRefOid"`
	Commits    struct {
		Nodes []struct {
			Commit struct {
				Oid string `json:"oid"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// parseMergedPRResponse maps the b<i> aliases of a buildMergedPRQuery
// response back to heads[i].
func parseMergedPRResponse(output []byte, heads []string) (map[string][]mergedPR, error) {
	var resp struct {
		Data struct {
			Repository map[string]*struct {
				Nodes []mergedPRNode `json:"nodes"`
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
		list := make([]mergedPR, 0, len(field.Nodes))
		for _, node := range field.Nodes {
			pr := mergedPR{Number: node.Number, HeadRefOid: node.HeadRefOid}
			for _, c := range node.Commits.Nodes {
				pr.Commits = append(pr.Commits, c.Commit.Oid)
			}
			list = append(list, pr)
		}
		prs[head] = list
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
// host is decided by checkGhHost.
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

// ghTokenEnvVars are the environment variables gh reads a token from.
// GH_ENTERPRISE_TOKEN and GITHUB_ENTERPRISE_TOKEN apply to every host other
// than github.com, whatever it is; in a codespace GITHUB_TOKEN does too.
var ghTokenEnvVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// checkGhHost returns an error unless host is a GitHub host that gh may send
// a token to. The host comes from the remote URL, which can be any git
// server, and gh sends a token from the environment (see ghTokenEnvVars) to
// any host it is asked to call. So gh is only called for:
//
//   - github.com and GitHub Enterprise Cloud with data residency (*.ghe.com),
//   - the host named by GH_HOST,
//   - a host `gh auth login` stored credentials for.
func checkGhHost(cfg runConfig, host string) error {
	allowed, cached := cfg.ghHosts[host]
	if !cached {
		allowed = isGitHubCloudHost(host) || strings.EqualFold(host, os.Getenv("GH_HOST")) || ghHasStoredToken(cfg, host)
		if cfg.ghHosts != nil {
			cfg.ghHosts[host] = allowed
		}
	}
	if !allowed {
		return fmt.Errorf("%q is not a GitHub host gh is logged in to (run `gh auth login --hostname <host>`, or set GH_HOST for GitHub Enterprise Server)", host)
	}
	return nil
}

// isGitHubCloudHost reports whether host is operated by GitHub: github.com,
// one of its subdomains, or a *.ghe.com tenant.
func isGitHubCloudHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".ghe.com")
}

// ghHasStoredToken reports whether gh has credentials of its own for host,
// from `gh auth login`. `gh auth token` is run without the token environment
// variables, which would otherwise answer for any host. The token it prints
// is discarded.
func ghHasStoredToken(cfg runConfig, host string) bool {
	args := []string{"auth", "token", "--hostname", host}
	verboseLog(cfg, "gh", args)
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = withoutEnvVars(os.Environ(), ghTokenEnvVars)
	return cmd.Run() == nil
}

// withoutEnvVars returns env ("NAME=value" entries) without the variables in
// names. Names are compared without regard to case, as Windows does.
func withoutEnvVars(env, names []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, name) }) {
			kept = append(kept, entry)
		}
	}
	return kept
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
	if err != nil {
		printNote(cfg, "couldn't resolve SSH host '%s': %s", host, err.Error())
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

// refExists reports whether the full ref name exists.
func refExists(ref string) bool {
	return exec.Command("git", "show-ref", "--verify", "--quiet", ref).Run() == nil
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
	_, err := gitOutput(args...)
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
// value. run() calls it once per stream, so each is decided independently.
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
func printWarning(cfg runConfig, format string, args ...any) {
	cfg.report.warned = true
	fmt.Fprintf(os.Stderr, "%swarning: %s%s\n", cfg.stderrColors.yellow, fmt.Sprintf(format, args...), cfg.stderrColors.reset)
}

// printChange prints a branch update, deletion or switch to stdout, or what
// --dry-run would do.
func printChange(cfg runConfig, format string, args ...any) {
	cfg.report.changed = true
	fmt.Printf(format, args...)
}

// printNotice prints "note: <message>" to stderr, with or without --verbose.
// It is for something the user should know about but need not act on, so
// unlike a warning it is not colored.
func printNotice(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "note: %s\n", fmt.Sprintf(format, args...))
}

// printNote prints "note: <message>" to stderr with --verbose only: details
// about a single branch that was skipped.
func printNote(cfg runConfig, format string, args ...any) {
	if cfg.verbose {
		fmt.Fprintf(os.Stderr, "note: %s\n", fmt.Sprintf(format, args...))
	}
}
