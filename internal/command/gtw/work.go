package gtw

import (
	"context"
	"strings"
)

// The work a coding agent produced in a worktree: what the tree
// holds and what the branch carries.
//
// Two commands need it. `/gtw fix` judges its own outcome with it —
// the agent produced something, or it produced nothing. `/gtw commit`
// needs the commit half to tell "the work was already committed"
// from "there is nothing to commit".

// workCounts is what a worktree holds right now.
type workCounts struct {
	// staged is the number of paths with a change in the index.
	staged int
	// unstaged is the number of paths with a change in the working
	// tree that the index does not carry.
	unstaged int
	// untracked is the number of paths git does not track.
	untracked int
	// commits is the number of commits the coding agent made.
	commits int
}

// total is the number of kinds of change the worktree carries.
// Zero means nothing happened: the agent left the worktree as it
// found it.
func (w workCounts) total() int {
	return w.staged + w.unstaged + w.untracked + w.commits
}

// countWorktreeChanges counts the changes the worktree holds: the
// paths staged in the index, the paths changed but not staged, and
// the untracked paths.
//
// The porcelain status is the same source the readiness gate reads,
// but the XY columns are split here — the readiness gate only asks
// "is the tree clean", this asks "what did the agent leave behind".
func countWorktreeChanges(ctx context.Context, worktree string, deps HandlerDeps) (staged, unstaged, untracked int, err error) {
	out, stderr, err := deps.Git.Run(ctx, worktree, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return 0, 0, 0, gitErr("git status", err, stderr)
	}
	for _, line := range splitNonEmptyLines(out) {
		if len(line) < 3 {
			continue
		}
		x, y := line[0], line[1]
		if x == '?' && y == '?' {
			untracked++
			continue
		}
		if x != ' ' {
			staged++
		}
		if y != ' ' {
			unstaged++
		}
	}
	return staged, unstaged, untracked, nil
}

// agentCommits returns the commits between base and HEAD that the
// coding agent made — the oneline log of each, newest first.
//
// The commits gtw itself lands on the branch (the .gitignore commit
// the fix flow makes) are left out: the question this answers is
// "did the agent produce anything", and gtw's own commit would
// answer it in the agent's place.
//
// An empty base means "no baseline" — there is nothing to compare
// against, so there are no commits to report.
func agentCommits(ctx context.Context, worktree, base string, deps HandlerDeps) ([]string, error) {
	if base == "" {
		return nil, nil
	}
	// The author email rides along so gtw's own commits can be told
	// apart from the agent's; the tab separates the two fields (an
	// email never carries one).
	out, stderr, err := deps.Git.Run(ctx, worktree, "log", "--format=%ae\t%h %s", base+"..HEAD")
	if err != nil {
		// An unborn ref has no commit to log: an empty range, not a
		// broken one.
		if strings.Contains(stderr, "does not have any commits") {
			return nil, nil
		}
		return nil, gitErr("git log "+base+"..HEAD", err, stderr)
	}
	var commits []string
	for _, line := range splitNonEmptyLines(out) {
		email, rest, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if email == gitToolIdentityEmail {
			continue
		}
		commits = append(commits, rest)
	}
	return commits, nil
}

// remoteHoldsLocalHead reports whether the remote's tip for the
// branch is the local head — i.e. every local commit is on the
// remote.
//
// The remote's tip is read from the remote-tracking ref the push
// updates, so a push that just landed is seen as "the remote holds
// the local head" without a network round-trip.
func remoteHoldsLocalHead(ctx context.Context, worktree, branch string, deps HandlerDeps) (bool, error) {
	local, err := headSHA(ctx, worktree, deps)
	if err != nil {
		return false, err
	}
	remote, err := originBranchSHA(ctx, worktree, branch, deps)
	if err != nil {
		return false, err
	}
	return remote != "" && local == remote, nil
}
