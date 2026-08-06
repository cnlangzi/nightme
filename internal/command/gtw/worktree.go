package gtw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// GitRunner abstracts the git CLI. The default implementation uses
// os/exec. Tests can inject a fake to assert on argv without spawning
// a process.
type GitRunner interface {
	Run(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error)
}

// ExecGitRunner is the production GitRunner: it shells out to the
// `git` binary on PATH. dir may be empty (run in the caller's cwd);
// otherwise it is the working directory of the spawned process.
type ExecGitRunner struct{}

// Run implements GitRunner by exec.CommandContext.
func (ExecGitRunner) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	if len(args) == 0 {
		return "", "", errors.New("gtw: git: empty argv")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimRight(stdout.String(), "\n"), strings.TrimRight(stderr.String(), "\n"), err
}

// ErrNotInGitRepo is returned by RepoRoot when cwd (or the
// `dir` argument) is not inside a git working tree. The caller
// surfaces this to the user as "send this from inside a git repo".
var ErrNotInGitRepo = errors.New("gtw: not in a git repository")

// RepoRoot returns the absolute path of the current repository's
// working tree root, given any path inside it. Equivalent to
// `git rev-parse --show-toplevel` but does not depend on the caller's
// cwd (uses `dir` as the starting point).
func RepoRoot(ctx context.Context, dir string, git GitRunner) (string, error) {
	out, _, err := git.Run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotInGitRepo, err)
	}
	// `git rev-parse --show-toplevel` always prints exactly one
	// line; the GitRunner strips trailing newlines, but the test
	// fake may not. Trim defensively.
	out = strings.TrimSpace(out)
	if out == "" {
		return "", ErrNotInGitRepo
	}
	return out, nil
}

// RemoteOriginURL returns the URL of the "origin" remote, or "" if
// no origin is configured. Consumed by gtw.RunFix and
// gtw.RebuildContext to drive Provider detection (URL hint +
// optional API probe for self-hosted GitHub Enterprise / GitLab).
//
// The canonical Provider-abstraction design is in
// docs/feat/F-50-git-provider.md (F-50 landed in v1.3.x as the
// "do it once and for all" replacement for the dangling
// "F-45 §7.2" reference that used to live here).
func RemoteOriginURL(ctx context.Context, dir string, git GitRunner) (string, error) {
	out, _, err := git.Run(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// BranchExists reports whether `branch` is present in the local
// repository (refs/heads/<branch>). Used by the §5.3.1 decision
// card to detect collisions.
//
// The "miss" case is signalled by `git show-ref --verify --quiet`
// exiting non-zero with a short stderr ("!" with no further
// output). Any other non-zero exit is treated as a real error
// (e.g. repo corruption) and bubbled up. We use a duck-typed
// ExitCode() check on the runner's error to keep tests free of
// *exec.ExitError's unexported fields.
func BranchExists(ctx context.Context, dir, branch string, git GitRunner) (bool, error) {
	stdout, stderr, err := git.Run(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		_ = stdout
		return true, nil
	}
	// "Quiet" mode never prints. The non-quiet variant below
	// would, but we use a simpler heuristic: a real error has
	// non-empty stderr; a miss has empty stderr.
	if stderr != "" {
		return false, err
	}
	return false, nil
}

// CurrentBranch returns the active branch name, or "" if the repo
// is in detached HEAD state. Used by rebuildGTWContext to figure
// out whether the current cwd is part of an active /gtw fix.
func CurrentBranch(ctx context.Context, dir string, git GitRunner) (string, error) {
	out, stderr, err := git.Run(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		// Detached HEAD / missing ref → empty stdout → treat as
		// "no current branch" rather than a hard error.
		if stderr == "" {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// WorktreeListPath returns the absolute path of the worktree that
// currently holds `branch`, or "" if `branch` is not checked out
// in any worktree. Used by the §5.3.1 "join existing worktree"
// branch of the decision card.
func WorktreeListPath(ctx context.Context, dir, branch string, git GitRunner) (string, error) {
	out, _, err := git.Run(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	_ = strings.SplitSeq(out, "\n") // doc reference: porcelain is line-oriented
	// Walk the porcelain output once, tracking the most recent
	// "worktree <path>" line so we can return the path whose
	// "branch refs/heads/<branch>" line follows.
	lines := strings.Split(out, "\n")
	var path string
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, "worktree "); ok {
			path = rest
			continue
		}
		if strings.TrimSpace(line) == "branch refs/heads/"+branch {
			return path, nil
		}
	}
	return "", nil
}

// WorktreeAdd creates a fresh worktree at `path` based on `base`
// (any commit-ish: branch, tag, HEAD, etc.). Equivalent to
//
//	git worktree add -b <newBranch> <path> <base>
//
// when `newBranch` is non-empty, or
//
//	git worktree add <path> <base>
//
// when newBranch is empty (used by the §5.3.1 "join existing"
// branch of the decision card, where the branch already exists).
func WorktreeAdd(ctx context.Context, dir, newBranch, path, base string, git GitRunner) error {
	args := []string{"worktree", "add"}
	if newBranch != "" {
		args = append(args, "-b", newBranch)
	}
	args = append(args, path)
	if base != "" {
		args = append(args, base)
	}
	_, stderr, err := git.Run(ctx, dir, args...)
	if err != nil {
		return &WorktreeError{
			Op:     "worktree_add",
			Stderr: stderr,
			Err:    err,
		}
	}
	return nil
}

// WorktreeError wraps a failed `git worktree add` so callers can
// render the last few lines of stderr to the user (F-45 §5.3.3
// decision card). The Op field distinguishes kinds for future use.
type WorktreeError struct {
	Op     string
	Stderr string
	Err    error
}

func (e *WorktreeError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("gtw: git worktree %s: %v: %s", e.Op, e.Err, e.Stderr)
	}
	return fmt.Sprintf("gtw: git worktree %s: %v", e.Op, e.Err)
}

func (e *WorktreeError) Unwrap() error { return e.Err }
