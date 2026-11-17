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
	// paths is the number of distinct paths the worktree carries a
	// change on. A path staged and then modified again counts once.
	paths int
	// commits is the number of commits the coding agent made.
	commits int
}

// total is the number of things the agent left behind: the paths it
// touched plus the commits it made. Zero means nothing happened.
func (w workCounts) total() int {
	return w.paths + w.commits
}

// countWorktreeChanges counts the changes the worktree holds: the
// paths staged in the index, the paths changed but not staged, the
// untracked paths, and the distinct paths the worktree carries a
// change on.
//
// The porcelain status is the same source the readiness gate reads,
// but the XY columns are split here — the readiness gate only asks
// "is the tree clean", this asks "what did the agent leave behind".
func countWorktreeChanges(ctx context.Context, worktree string, deps HandlerDeps) (workCounts, error) {
	out, stderr, err := deps.Git.Run(ctx, worktree, "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return workCounts{}, gitErr("git status", err, stderr)
	}
	var w workCounts
	for _, line := range splitNonEmptyLines(out) {
		if len(line) < 3 {
			continue
		}
		x, y := line[0], line[1]
		if x == '?' && y == '?' {
			w.untracked++
			w.paths++
			continue
		}
		if x == '!' && y == '!' {
			continue
		}
		// One path, one entry: it counts once even when it is both
		// staged and modified again.
		w.paths++
		if x != ' ' {
			w.staged++
		}
		if y != ' ' {
			w.unstaged++
		}
	}
	return w, nil
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

// commitsMissingFromRemote counts the commits the remote's tip for
// the branch does not have.
//
// The range origin/<branch>..HEAD answers the question the push
// command asks: does the remote hold every local commit? A count of
// zero means yes — whether the local is level with the remote or
// behind it. A plain SHA comparison cannot tell those two apart, and
// calls a branch that is behind the remote one that "carries commits
// the remote does not have".
//
// The remote the push targets is origin — the same one
// programmaticPush pushes to.
func commitsMissingFromRemote(ctx context.Context, worktree, branch string, deps HandlerDeps) (int, error) {
	out, stderr, err := deps.Git.Run(ctx, worktree, "rev-list", "--count", "origin/"+branch+"..HEAD")
	if err != nil {
		return 0, gitErr("git rev-list --count origin/"+branch+"..HEAD", err, stderr)
	}
	return atoi(strings.TrimSpace(out))
}
