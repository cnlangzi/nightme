package wiki

import (
	"context"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// gitTimeout bounds every git call /wiki makes. A hung git (a
// credential prompt, an unreachable remote) must not park the
// command dispatcher.
const gitTimeout = 30 * time.Second

// GitRunner is the minimal git surface /wiki needs:
// resolve a repository root and report working-tree state.
type GitRunner interface {
	// RepoRoot returns the toplevel of the repository
	// containing cwd. Used to normalise a ChatSession CWD
	// that lives inside a worktree.
	RepoRoot(ctx context.Context, cwd string) (string, error)
	// IsClean reports whether repoRoot's working tree has
	// no uncommitted modifications.
	IsClean(ctx context.Context, repoRoot string) (bool, error)
}

// ExecGitRunner shells out to the git binary. Production
// default — no extra deps.
type ExecGitRunner struct{}

// RepoRoot returns the toplevel of the repository containing
// cwd.
func (ExecGitRunner) RepoRoot(ctx context.Context, cwd string) (string, error) {
	out, err := runGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", wrapGitErr("not a git repo (or git unavailable)", err)
	}
	if out == "" {
		return "", wrapGitErr("not a git repo (or git unavailable)", err)
	}
	return out, nil
}

// IsClean reports whether repoRoot's working tree has no
// uncommitted modifications.
func (ExecGitRunner) IsClean(ctx context.Context, repoRoot string) (bool, error) {
	out, err := runGit(ctx, repoRoot, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// runGit runs a git command in cwd and returns its trimmed stdout.
//
// The command goes through the shared process helper (internal/proc)
// so it inherits the SIGTERM → grace → SIGKILL cleanup, the Windows
// .cmd shim handling and the hidden-window behaviour — the same
// plumbing every other git call in nightme uses.
//
// `--no-optional-locks` skips git's optional index refresh. The
// status call only reads the working tree, and a refresh would take
// the index lock on every /wiki run.
//
// On failure it returns a *gitError carrying git's stderr, so the
// caller sees git's own diagnostic instead of a bare exit status.
func runGit(ctx context.Context, cwd string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := proc.New(c, "git", gitArgs(cwd, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			// git never ran (not on PATH, …) — the exec error is
			// the only diagnostic there is.
			msg = err.Error()
		}
		return "", &gitError{Stderr: msg}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitArgs builds the argv of a git invocation in cwd.
//
// `--no-optional-locks` comes first so git skips the optional index
// refresh for every command /wiki runs.
func gitArgs(cwd string, args ...string) []string {
	return append([]string{"--no-optional-locks", "-C", cwd}, args...)
}

type gitError struct {
	Stderr string
}

func (e *gitError) Error() string { return e.Stderr }

func wrapGitErr(msg string, err error) error {
	if err == nil {
		return nil
	}
	if ge, ok := err.(*gitError); ok && ge.Stderr != "" {
		return &gitError{Stderr: msg + ": " + ge.Stderr}
	}
	return &gitError{Stderr: msg}
}
