package review

// The git facts the review judges itself by.
//
// The review needs to know whether the findings it handed to the
// coding agent were acted on. The answer is in the workspace: a new
// commit, or a change the working tree did not carry before.

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// gitTimeout bounds every git call /review makes. A hung git (a
// credential prompt, an unreachable remote) must not park the
// command dispatcher.
const gitTimeout = 30 * time.Second

// workspaceState is what the workspace held at one moment: the head
// it was on and the changes its working tree carried.
type workspaceState struct {
	// head is the commit HEAD pointed at.
	head string
	// status is the porcelain status of the working tree. Empty
	// when the tree is clean.
	status string
	// known reports whether the state could be read at all. A
	// workspace that is not a git repository has no state to judge
	// against.
	known bool
}

// readWorkspaceState reads the head and the working tree's changes.
//
// The state is only known when both reads succeeded: a half-read
// state would compare a head against nothing.
func readWorkspaceState(ctx context.Context, dir string) (workspaceState, error) {
	head, err := runGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return workspaceState{}, err
	}
	status, err := runGit(ctx, dir, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return workspaceState{}, err
	}
	return workspaceState{head: head, status: status, known: true}, nil
}

// readBaseline reads the workspace's state for the review's verdict.
//
// The read is bounded: it happens right before the findings go out,
// and a slow git must not hold the findings back. A workspace that
// cannot be read yields an unknown state — the verdict then leaves
// the review's own verdict standing.
func readBaseline(ctx context.Context, dir string) workspaceState {
	ctx, cancel := context.WithTimeout(ctx, baselineTimeout)
	defer cancel()
	state, err := readWorkspaceState(ctx, dir)
	if err != nil {
		slog.Default().Debug("/review: the workspace could not be read",
			"workspace", dir,
			"err", err)
		return workspaceState{}
	}
	return state
}

// runGit runs a git command in dir and returns its trimmed stdout.
//
// The command goes through the shared process helper (internal/proc)
// so it inherits the SIGTERM → grace → SIGKILL cleanup and the
// Windows .cmd shim handling — the same plumbing every other git
// call in nightme uses.
//
// `--no-optional-locks` skips git's optional index refresh: the
// status call only reads the working tree, and a refresh would take
// the index lock on every /review run.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := proc.New(c, "git", append([]string{"--no-optional-locks", "-C", dir}, args...)...)
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
		return "", &gitError{stderr: msg, err: err}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitError is a failed git call carrying git's own diagnostic.
//
// The underlying error is kept so the caller can still tell what
// went wrong (git missing, permission denied) when git printed
// nothing useful of its own.
type gitError struct {
	stderr string
	err    error
}

func (e *gitError) Error() string { return e.stderr }

// Unwrap exposes the underlying exec error.
func (e *gitError) Unwrap() error { return e.err }
