package gtw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/pathutil"
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

// Run implements GitRunner by delegating to runCmd (see exec.go) —
// the exec / Dir / capture plumbing lives in one place, so this
// path can never drift from the CLI runner's. dir may be empty
// (run in the caller's cwd); otherwise it is the working
// directory of the spawned process.
func (ExecGitRunner) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	if len(args) == 0 {
		return "", "", errors.New("gtw: git: empty argv")
	}
	return runCmd(ctx, dir, "git", args...)
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

// RemoteBranchExists reports whether branch exists on the
// origin remote. Equivalent to
// git ls-remote --heads origin <branch> -- empty stdout means
// the branch is not on origin; any non-empty line (one per ref)
// means it is.
//
// F-237: git status --porcelain --branch trusts local config
// (branch.<name>.{remote,merge}) and the cached
// refs/remotes/origin/<branch> SHA. A branch that was pushed
// then deleted server-side -- or pulled into a sibling worktree
// from a stale clone -- leaves a cached tracking ref whose
// ## branch...origin/branch porcelain header makes
// CollectReadiness believe the branch is on origin when it
// actually is not. RemoteBranchExists is the probe that catches
// that lie (see CollectReadinessForDispatch / verifyUpstreamOnOrigin).
//
// Returns (false, nil) on:
//   - empty git ls-remote output (branch truly absent on origin)
//   - exit-zero with no refs (no upstream branch matching <branch>)
//
// Returns (false, err) only when git itself failed (no origin
// remote, network down, etc.). Callers should treat this as
// "cannot verify" rather than "definitely absent" -- see the
// graceful-fallback logic in verifyUpstreamOnOrigin.
//
// The output of git ls-remote --heads origin <branch> is
// <sha>	refs/heads/<branch> per matching ref; we do not
// parse the SHA, only check for non-empty output. The CLI
// strips trailing newlines so a single matching ref produces
// a single non-empty line.
// lsRemoteKnownErrors maps a known git ls-remote stderr fragment
// to a friendly hint string. Each fragment is the literal
// substring we match on; the hint is appended after the trimmed
// stderr so the original message is still visible.
//
// "could not read Username" — git's auth-prompt failure on an
// HTTPS remote without cached credentials.
// "unable to access" — DNS / network failure to origin.
// "does not appear to be a git repository" — origin URL points
// somewhere that isn't a git server.
//
// Unmatched stderr falls through to the generic wrap path —
// dispatchPR surfaces the raw stderr verbatim, NEVER
// reinterpreted as one of the known fragments. The user's
// product principle: known errors get friendly hints; unknown
// errors get the truth.
var lsRemoteKnownErrors = []struct {
	fragment string
	hint     string
}{
	{"could not read Username", "hint: configure git credentials (e.g. `gh auth login` or `git config credential.helper`)"},
	{"unable to access", "hint: check network connectivity and the origin remote URL (`git remote -v`)"},
	{"does not appear to be a git repository", "hint: verify `git remote -v` — origin should point at the same host as the PR target"},
}

// RemoteBranchExists reports whether `branch` is present on the
// origin remote's refs/heads namespace via
// `git ls-remote --heads origin <branch>`. The CLI strips trailing
// newlines so a single matching ref produces a single non-empty
// line.
//
// Error contract:
//   - known stderr fragments (auth / network / not-a-repo) →
//     wrapped error includes the original stderr PLUS a friendly
//     hint pointing at the next step;
//   - unknown stderr → wrapped verbatim, NO hint attached;
//   - empty stderr → wrapped from the underlying exec error.
//
// Used by /gtw pr's first readiness gate ("origin/<branch>
// ref exists"). The PR dispatcher surfaces the error directly;
// we deliberately do NOT silently fall back to "no upstream"
// on transient errors — that would mislead the user into
// running /gtw push when the real problem is the network.
func RemoteBranchExists(ctx context.Context, dir, branch string, git GitRunner) (bool, error) {
	out, stderr, err := git.Run(ctx, dir, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		trimmed := strings.TrimSpace(stderr)
		for _, k := range lsRemoteKnownErrors {
			if strings.Contains(trimmed, k.fragment) {
				return false, fmt.Errorf("git ls-remote --heads origin %s: %s\n%s",
					branch, trimmed, k.hint)
			}
		}
		// Unknown stderr — preserve verbatim, no hint. The user
		// can read the original git message and decide.
		if trimmed != "" {
			return false, fmt.Errorf("git ls-remote --heads origin %s: %s", branch, trimmed)
		}
		return false, fmt.Errorf("git ls-remote --heads origin %s: %w", branch, err)
	}
	return strings.TrimSpace(out) != "", nil
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

// RefreshDefaultBranch brings repoRoot (which MUST be the
// primary checkout, not a linked worktree) up to date with
// the upstream default branch. Called by /gtw fix as a
// pre-worktree refresh and by /gtw sync as a user-invoked
// command. The sequence:
//
//  1. Discover the default branch (errors if no origin).
//  2. Refuse if the primary checkout has uncommitted changes —
//     silently stashing / discarding would be hostile.
//  3. Verify repoRoot is the primary checkout (not a linked
//     worktree). `git checkout <default>` from a linked
//     worktree fails because the default branch is checked
//     out elsewhere; worse, git can silently switch the
//     worktree itself off the user's feature branch.
//  4. `git checkout <default>`.
//  5. `git pull --rebase origin <default>`.
//
// Returns (newHead, pullOut, err):
//   - newHead: the post-pull HEAD SHA, for callers that want
//     to label a success card ("based on origin/main@<sha>")
//     and verify the refresh moved the branch.
//   - pullOut: the raw `git pull --rebase` stdout ("Already
//     up to date.", "Updating X..Y\nFast-forward\n…", etc.).
//     /gtw sync passes this through to the user; /gtw fix
//     discards it in favour of its own success card layout.
func RefreshDefaultBranch(ctx context.Context, repoRoot string, deps HandlerDeps) (string, string, error) {
	branch, err := DefaultBranch(ctx, repoRoot, deps.Git)
	if err != nil {
		return "", "", err
	}

	// Step 2: refuse on dirty main.
	statusOut, _, err := deps.Git.Run(ctx, repoRoot, "status", "--porcelain")
	if err != nil {
		return "", "", fmt.Errorf("git status %s: %w", repoRoot, err)
	}
	if statusOut != "" {
		return "", "", fmt.Errorf(
			"❌ main repo %s has uncommitted changes — commit or stash first:\n%s",
			repoRoot, statusOut)
	}

	// Step 3: enforce primary-checkout precondition. `git
	// rev-parse --git-dir` returns the per-worktree .git
	// location; `--git-common-dir` returns the shared .git
	// directory. They match only in the primary checkout.
	// Resolving both relative to repoRoot lets us compare
	// paths the same way regardless of how git printed them.
	gitDirRaw, _, err := deps.Git.Run(ctx, repoRoot, "rev-parse", "--git-dir")
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse --git-dir in %s: %w", repoRoot, err)
	}
	commonDirRaw, _, err := deps.Git.Run(ctx, repoRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse --git-common-dir in %s: %w", repoRoot, err)
	}
	gitDir := pathutil.Clean(pathutil.Join(repoRoot, strings.TrimSpace(gitDirRaw)))
	commonDir := pathutil.Clean(pathutil.Join(repoRoot, strings.TrimSpace(commonDirRaw)))
	if gitDir != commonDir {
		return "", "", fmt.Errorf(
			"❌ %s is a linked worktree; default-branch refresh must run from the primary checkout. /cwd into the main repo first.",
			repoRoot)
	}

	// Step 4: checkout.
	if _, _, err := deps.Git.Run(ctx, repoRoot, "checkout", branch); err != nil {
		return "", "", fmt.Errorf("git checkout %s in %s: %w", branch, repoRoot, err)
	}

	// Step 5: pull --rebase. We pass explicit `<remote>
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
	pullOut, stderr, err := deps.Git.Run(ctx, repoRoot,
		"pull", "--rebase", "origin", branch)
	if err != nil {
		return "", pullOut, fmt.Errorf(
			"git pull --rebase origin %s in %s: %w "+
				"[stderr: %s] "+
				"(rebase conflict? resolve manually, then `git rebase --abort` to clean up before retrying)",
			branch, repoRoot, err, stderr)
	}

	// Step 5: report the new HEAD.
	headOut, _, err := deps.Git.Run(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", pullOut, fmt.Errorf("git rev-parse HEAD in %s: %w", repoRoot, err)
	}
	return strings.TrimSpace(headOut), pullOut, nil
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
// F-PATHUTIL-001 §5.2: the worktree path is Normalized for git
// before being added to argv. On Windows, a yml-derived
// `c.Worktree` of "F:/foo" (forward slashes from git's
// rev-parse output) would otherwise reach `git worktree add` as
// "F:/foo" and trigger the same ERROR_INVALID_PARAMETER class of
// errors that WorktreeRemove used to hit.
//
// An empty path is a hard error here (not silently forwarded to
// git): it indicates an upstream caller passed a malformed value,
// and the user-visible git error would be opaque. Wrap the
// NormalizeForGit error in the WorktreeError shape so the
// /gtw fix/close path produces a clear error reply instead
// of "fatal: ".
func WorktreeAdd(ctx context.Context, dir, newBranch, path, base string, git GitRunner) error {
	args := []string{"worktree", "add"}
	if newBranch != "" {
		args = append(args, "-b", newBranch)
	}
	np, err := pathutil.NormalizeForGit(path)
	if err != nil {
		return &WorktreeError{
			Op:  "worktree_add",
			Err: fmt.Errorf("normalize worktree path %q: %w", path, err),
		}
	}
	path = np
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
// net. /gtw close passes its parsed --force flag straight through
// (RunClose's step 4); this skips the worktree dirty check (step 3)
// too, so the user can opt into destroying uncommitted edits in one
// flag.
//
// F-PATHUTIL-001 §5.2 (regression for the gtw-close "Invalid
// argument" bug on Windows): the worktree path is Normalized for
// git before being added to argv. Without this, a yml whose
// `Worktree` is "F:/foo" (forward slashes from git's
// `rev-parse --show-toplevel` output) is passed verbatim to git,
// which forwards it to RemoveDirectoryW as-is and fails with
// ERROR_INVALID_PARAMETER. NormalizeForGit forces backslash
// form on Windows.
//
// Empty-path is a hard error: same rationale as WorktreeAdd —
// the yml field is mandatory (ReadGTWYml validates Worktree !=
// "") so reaching here with "" is an upstream bug, and we'd
// rather say so than forward "git worktree remove ''" and get
// a cryptic "fatal: " from git.
//
// On failure the returned *WorktreeError carries the git stderr
// tail so the caller can render a useful "why did it fail" reply.
func WorktreeRemove(ctx context.Context, dir, path string, force bool, git GitRunner) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	np, err := pathutil.NormalizeForGit(path)
	if err != nil {
		return &WorktreeError{
			Op:  "worktree_remove",
			Err: fmt.Errorf("normalize worktree path %q: %w", path, err),
		}
	}
	path = np
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
	// Resolve symlinks on both sides so macOS (/var vs /private/var)
	// doesn't make same-path compare fail. EvalSymlinks is a
	// no-op on Linux for non-symlinked paths.
	canonicalAttached := attachedPath
	canonicalTarget := worktreePath
	if a, err := filepath.EvalSymlinks(attachedPath); err == nil {
		canonicalAttached = a
	}
	if t, err := filepath.EvalSymlinks(worktreePath); err == nil {
		canonicalTarget = t
	}
	if attachedPath != "" && canonicalAttached == canonicalTarget {
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

	parent := pathutil.Dir(worktreePath)
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
		grandparent := pathutil.Dir(parent)
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
