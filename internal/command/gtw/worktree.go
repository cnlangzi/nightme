package gtw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// no origin is configured. Consumed by gtw.runFixRemote to drive
// Provider detection (URL hint + optional API probe for self-hosted
// GitHub Enterprise / GitLab). Local mode (runFixLocal) does not
// call this — local worktrees have no remote issue.
//
// The canonical Provider-abstraction design is in
// docs/feat/F-50-git-provider.md.
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

// DefaultBranch discovers the repository's default branch name
// (e.g. "main", "master", "develop") via the symbolic-ref
// `refs/remotes/origin/HEAD` that GitHub / GitLab / most
// remotes set on first clone. Returns the bare branch name
// (no `origin/` prefix). Errors when the ref is missing —
// caller surfaces the "no upstream" hint to the user.
//
// Repos cloned before the host set this ref, or fresh local
// repos without an `origin`, will hit this error path. The
// caller is expected to surface a friendly message; we do
// NOT fall back to "main" by name because that would silently
// pick the wrong branch on `master`-default repos.
func DefaultBranch(ctx context.Context, dir string, git GitRunner) (string, error) {
	out, _, err := git.Run(ctx, dir,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf(
			"discover default branch via refs/remotes/origin/HEAD: %w "+
				"(no origin remote? run `git remote add origin <url>` and `git fetch origin`)",
			err)
	}
	ref := strings.TrimSpace(out)
	branch := strings.TrimPrefix(ref, "origin/")
	if branch == "" || branch == ref {
		return "", fmt.Errorf("unrecognised default branch ref %q", ref)
	}
	return branch, nil
}

// RefreshDefaultBranch brings repoRoot up to date with the
// upstream default branch before /gtw fix creates a new
// worktree. The sequence:
//
//  1. Discover the default branch (errors if no origin).
//  2. Refuse if the main repo has uncommitted changes —
//     silently stashing / discarding would be hostile.
//  3. `git checkout <default>`.
//  4. `git pull --ff-only` — refuses non-fast-forward
//     updates so we never silently create merge commits on
//     the user's main branch.
//
// Returns the resulting HEAD SHA so the caller can label
// the success card ("based on origin/main@<sha>") and verify
// the refresh actually happened.
//
// /gtw fix calls this on the FRESH branch only — re-entry
// paths (branch already attached at the target worktree path)
// skip it because the user's working tree is already in the
// worktree, not the main repo, and there's nothing useful to
// refresh.
func RefreshDefaultBranch(ctx context.Context, repoRoot string, deps HandlerDeps) (string, error) {
	branch, err := DefaultBranch(ctx, repoRoot, deps.Git)
	if err != nil {
		return "", err
	}

	// Step 2: refuse on dirty main.
	statusOut, _, err := deps.Git.Run(ctx, repoRoot, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git status %s: %w", repoRoot, err)
	}
	if statusOut != "" {
		return "", fmt.Errorf(
			"❌ main repo %s has uncommitted changes — commit or stash before /gtw fix:\n%s",
			repoRoot, statusOut)
	}

	// Step 3: checkout.
	if _, _, err := deps.Git.Run(ctx, repoRoot, "checkout", branch); err != nil {
		return "", fmt.Errorf("git checkout %s in %s: %w", branch, repoRoot, err)
	}

	// Step 4: pull --rebase. We pass explicit `<remote>
// <branch>` so the command works on local branches that
// have no upstream-tracking config. We use `--rebase` rather
// than the default merge-style pull, OR `--ff-only`, for a
// concrete UX reason:
//
//   - `--ff-only` rejects when the user's local main has
//     un-pushed commits — every experiment the user did
//     since their last pull blocks /gtw fix until they
//     manually merge upstream. UX-hostile.
//   - `--rebase` (default-style merge) creates a merge
//     commit on the user's main — surprising for a tool
//     they thought was just "create a worktree".
//   - `--rebase` is the modern recommended default for
//     upstream-tracking branches (per git docs and most
//     major project READMEs). It replays the user's local
//     commits on top of upstream, exactly matching the
//     "I want my work to look like I started from the
//     latest upstream" intent that drives /gtw fix.
//
// Mid-rebase conflicts leave the repo in a half-rebased
// state; we surface the conflict (and the abort command)
// verbatim so the user can `git rebase --abort` and retry
// after resolving manually.
	if _, stderr, err := deps.Git.Run(ctx, repoRoot,
		"pull", "--rebase", "origin", branch); err != nil {
		return "", fmt.Errorf(
			"git pull --rebase origin %s in %s: %w "+
				"[stderr: %s] "+
				"(rebase conflict? resolve manually, then `git rebase --abort` to clean up before retrying /gtw fix)",
			branch, repoRoot, err, stderr)
	}

	// Step 5: report the new HEAD.
	headOut, _, err := deps.Git.Run(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD in %s: %w", repoRoot, err)
	}
	return strings.TrimSpace(headOut), nil
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
	// Walk the porcelain output once, tracking the most recent
	// "worktree <path>" line so we can return the path whose
	// "branch refs/heads/<branch>" line follows. Porcelain is
	// line-oriented (one record per worktree, fields separated
	// by newlines) so strings.Split is the right tool.
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

// WorktreeRemove runs `git worktree remove [<--force>] <path>` from
// inside `dir` (which MUST be the main repo root — git refuses to
// run `worktree remove` from inside a worktree itself, returning
// "fatal: cannot remove the current working directory"). Used by
// /gtw close (F-XX §14.5) to tear down the worktree that /gtw fix
// created.
//
// force=true maps to `git worktree remove --force <path>`, which
// skips the "worktree contains modified or untracked files" safety
// net. v1 of /gtw close never sets force=true (RunClose errors
// out on dirty worktrees and lets the user decide); the flag is
// exposed so future flows (e.g. an explicit /gtw close --force)
// can opt in.
//
// On failure the returned *WorktreeError carries the git stderr
// tail so the caller can render a useful "why did it fail" reply.
func WorktreeRemove(ctx context.Context, dir, path string, force bool, git GitRunner) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, stderr, err := git.Run(ctx, dir, args...)
	if err != nil {
		return &WorktreeError{
			Op:     "worktree_remove",
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

// PreflightWorktreeCreate runs four cheap checks before
// `git worktree add`, so the user sees a clean error instead of
// `git`'s internal "fatal: ..." strings. Returns nil when all
// checks pass.
//
// F-XX: new for the gtw mode-split refactor.
//
// Checks (in order):
//
//  1. If `branch` is already attached to the target worktree
//     path (a re-entry after daemon recovery, or a same-path
//     repeat), return nil. The worktree directory exists
//     intentionally in this case; the caller treats it as
//     "already set up" rather than "create a new one".
//  2. Branch not already attached to a DIFFERENT worktree via
//     `git worktree list --porcelain` (catches `fatal: <branch>
//     is already checked out at <other>`).
//  3. Target path not already present on the filesystem
//     (catches `fatal: <path> already exists`).
//  4. Parent directory of the worktree path exists and is
//     writable by us (catches `fatal: cannot create directory
//     at <path>: Permission denied`).
//
// Pre-fix this code path deferred all checks to `WorktreeAdd`,
// relying on its opaque `*WorktreeError` for surface. Users had
// to read 10 lines of stderr to figure out which case they'd
// hit; preflight surfaces a one-line friendly error.
//
// The same-path short-circuit (check 1) means the caller MUST
// already have decided the branch is the right one — i.e. the
// caller has done BranchExists(...) and seen true. The runFix*
// callers honour that ordering; new callers should not skip
// the BranchExists check before invoking preflight.
func PreflightWorktreeCreate(ctx context.Context, repoRoot, branch, worktreePath string, git GitRunner) error {
	attachedPath, err := WorktreeListPath(ctx, repoRoot, branch, git)
	if err != nil {
		return fmt.Errorf("git worktree list: %w", err)
	}
	if attachedPath != "" && filepath.Clean(attachedPath) == filepath.Clean(worktreePath) {
		// Branch is attached at exactly the target path —
		// daemon-recovery / repeat-run case. Allow it.
		return nil
	}
	if attachedPath != "" {
		return fmt.Errorf(
			"❌ branch %q already checked out at %q — join that worktree, remove it, or pick a different branch",
			branch, attachedPath,
		)
	}

	if _, err := os.Stat(worktreePath); err == nil {
		return fmt.Errorf(
			"❌ worktree path %q already exists on filesystem — clean up before retrying",
			worktreePath,
		)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat worktree path %q: %w", worktreePath, err)
	}

	parent := filepath.Dir(worktreePath)
	parentInfo, parentStatErr := os.Stat(parent)
	switch {
	case parentStatErr == nil:
		// Parent exists. Must be a directory (not a symlink to
		// a file, not a file itself).
		if !parentInfo.IsDir() {
			return fmt.Errorf(
				"❌ parent path %q exists but is not a directory",
				parent,
			)
		}
	case os.IsNotExist(parentStatErr):
		// Parent does NOT exist. `git worktree add` will create
		// the necessary parent directories on our behalf, so a
		// missing parent is fine — as long as the GRANDPARENT
		// (the directory that would contain the to-be-created
		// parent) is writable. Probe writability there.
		grandparent := filepath.Dir(parent)
		probeDir, err := os.MkdirTemp(grandparent, ".gtw-grandparent-probe-*")
		if err != nil {
			return fmt.Errorf(
				"❌ cannot create worktree parent %q (grandparent %q not writable: %w)",
				parent, grandparent, err,
			)
		}
		_ = os.RemoveAll(probeDir)
		return nil
	default:
		return fmt.Errorf("stat parent %q: %w", parent, parentStatErr)
	}

	// Parent exists. Probe writability directly inside it.
	probeDir, err := os.MkdirTemp(parent, ".gtw-probe-*")
	if err != nil {
		return fmt.Errorf(
			"❌ parent directory %q is not writable: %w",
			parent, err,
		)
	}
	_ = os.RemoveAll(probeDir)
	return nil
}
