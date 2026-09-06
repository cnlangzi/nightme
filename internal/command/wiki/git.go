package wiki

import (
	"os/exec"
	"strings"
)

// GitRunner is the minimal git surface /wiki needs:
// resolve a repository root and report working-tree state.
type GitRunner interface {
	// RepoRoot returns the toplevel of the repository
	// containing cwd. Used to normalise a ChatSession CWD
	// that lives inside a worktree.
	RepoRoot(cwd string) (string, error)
	// IsClean reports whether repoRoot's working tree has
	// no uncommitted modifications.
	IsClean(repoRoot string) (bool, error)
}

// ExecGitRunner shells out to the git binary. Production
// default — no extra deps.
type ExecGitRunner struct{}

// RepoRoot returns the toplevel of the repository containing
// cwd.
func (ExecGitRunner) RepoRoot(cwd string) (string, error) {
	out, err := runGit(cwd, "rev-parse", "--show-toplevel")
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
func (ExecGitRunner) IsClean(repoRoot string) (bool, error) {
	out, err := runGitAllowFailure(repoRoot, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

func runGit(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func runGitAllowFailure(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", &gitError{Stderr: strings.TrimSpace(string(ee.Stderr))}
		}
		return "", err
	}
	return string(out), nil
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
