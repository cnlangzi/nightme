package gtw

import (
	"context"
	"fmt"
	"strings"
)

// programmaticPush runs `git push -u origin <branch>`. The -u flag
// sets upstream tracking so subsequent plain `git push` works
// without specifying the remote.
//
// v1 hard-codes "origin"; future: read from the worktree's
// configured remote (the first entry under [remote ...] in
// .git/config).
func programmaticPush(ctx context.Context, deps HandlerDeps, c Context) (string, error) {
	stdout, stderr, err := deps.Git.Run(ctx, c.Worktree,
		"push", "-u", "origin", c.Branch)
	if err != nil {
		return stdout + stderr, fmt.Errorf("%w", err)
	}
	return stdout + stderr, nil
}

// countUnpushed returns the count of commits on the local branch
// that have no upstream counterpart.
//
// When @{u} is unset (first push of a fresh branch) returns 0 —
// the dispatcher's "clean + unpushed" branch treats that as
// "nothing to push"; the "dirty" branch will have the agent run
// `git push -u origin` to establish upstream.
//
// When @{u} is set but rev-list errors for some other reason
// (permission denied, corrupt worktree, etc.) we return the
// error so the dispatcher can surface it — silently treating
// real failures as "nothing to push" would hide unpushed commits
// from the user.
func countUnpushed(ctx context.Context, worktree, branch string, deps HandlerDeps) (int, error) {
	_ = branch // branch not directly queried — @{u} resolves against HEAD's upstream
	out, stderr, err := deps.Git.Run(ctx, worktree, "rev-list", "--count", "@{u}..HEAD")
	if err == nil {
		n, perr := atoi(strings.TrimSpace(out))
		if perr != nil {
			return 0, perr
		}
		return n, nil
	}
	// git emits "fatal: no upstream configured for branch '<name>'"
	// on stderr when @{u} is unset; that's our "0 unpushed" signal.
	// Any other stderr is a real git error and must propagate.
	if strings.Contains(stderr, "no upstream configured") {
		return 0, nil
	}
	return 0, fmt.Errorf("count unpushed commits: %w (stderr: %s)", err, strings.TrimSpace(stderr))
}

// detectConflicts parses a `git status --porcelain` output and
// returns true if any unmerged paths are present (mid-rebase /
// mid-merge). The caller is expected to have already done the
// git status call — this is just the parsing half of the check.
func detectConflicts(statusOut string) bool {
	for _, line := range splitNonEmptyLines(statusOut) {
		if len(line) < 2 {
			continue
		}
		xy := line[:2]
		if xy[0] != ' ' && xy[1] != ' ' && xy != "??" &&
			(xy[0] == 'U' || xy[1] == 'U' ||
				xy[0] == 'A' || xy[1] == 'A' ||
				xy[0] == 'D' || xy[1] == 'D') {
			return true
		}
	}
	return false
}

// indentLines prefixes every line of s with prefix. Used for
// multi-line stdout (git push progress, agent reply text) in the
// success card so the indentation reads as a code block.
func indentLines(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// atoi is a tiny strconv.Atoi wrapper that keeps imports tight.
// Accepts non-negative integers only — fine for `git rev-list
// --count`, which never produces negatives.
func atoi(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("atoi: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
