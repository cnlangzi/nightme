package gtw

import (
	"context"
	"fmt"
	"strings"
)

// headSHA returns the current HEAD's full SHA in worktree. Used
// by verifyAgentPushedAndRecover to detect "agent claimed success
// but didn't commit" — a class of failure where status is empty
// and unpushed count is 0, but no commit actually landed.
//
// Returns the SHA with surrounding whitespace trimmed (git's
// output is one line, but the test fake may not be). Errors
// propagate so the caller can surface them; verifyAgentPushedAndRecover
// treats the snapshot as "unavailable" when this fails (skips
// the HEAD-advance check rather than aborting the verification
// entirely).
func headSHA(ctx context.Context, worktree string, deps HandlerDeps) (string, error) {
	out, _, err := deps.Git.Run(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// currentBranch returns the name of the branch HEAD is on
// (equivalent to `git rev-parse --abbrev-ref HEAD`). Empty
// string is returned for detached HEAD (the caller's failure
// path should treat that as "agent switched to detached").
func currentBranch(ctx context.Context, worktree string, deps HandlerDeps) (string, error) {
	out, _, err := deps.Git.Run(ctx, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse --abbrev-ref HEAD: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "HEAD" {
		// git returns the literal "HEAD" for detached HEAD.
		// Normalise to "" so the caller can branch on
		// "branch != expected" uniformly.
		return "", nil
	}
	return branch, nil
}

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

// countUnpushed returns the count of commits on the named branch
// that have no upstream counterpart.
//
// Uses `branch@{u}..branch` (NOT `HEAD@{u}..HEAD`) so the count
// reflects the BRANCH we're pushing, not whatever HEAD happens
// to be on. If HEAD is on a different branch (e.g. user did
// `git checkout main` inside the worktree), @{u}..HEAD would
// silently inspect the wrong branch and report a misleading
// "nothing to push" / wrong unpushed count for c.Branch.
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
	out, stderr, err := deps.Git.Run(ctx, worktree, "rev-list", "--count", branch+"@{u}.."+branch)
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

// listUnpushedCommits returns the oneline summary of every
// commit on `branch` that has no upstream counterpart. Used by
// dispatchPush / dispatchPR to surface the actual commits the
// user needs to push, so the message can name them instead of
// just saying "1 unpushed commit".
func listUnpushedCommits(ctx context.Context, worktree, branch string, deps HandlerDeps) ([]string, error) {
	out, stderr, err := deps.Git.Run(ctx, worktree, "rev-list", "--oneline", branch+"@{u}.."+branch)
	if err != nil {
		// Same "no upstream" → empty list semantics as countUnpushed.
		if strings.Contains(stderr, "no upstream configured") {
			return nil, nil
		}
		return nil, fmt.Errorf("list unpushed commits: %w (stderr: %s)", err, strings.TrimSpace(stderr))
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// unpushedCommitsForDisplay is the error-safe wrapper around
// listUnpushedCommits that the user-facing diagnostics use.
// Returns:
//   - "<commit>\n<commit>\n..." when the list succeeds
//   - "(couldn't list unpushed commits: <err>)" when it fails
//     (the user knows the diagnostic is incomplete rather than
//     seeing an empty list silently)
//   - "" when there are no unpushed commits
//
// Never returns an error itself — callers can paste the result
// straight into the IM reply body without further unwrapping.
func unpushedCommitsForDisplay(ctx context.Context, worktree, branch string, deps HandlerDeps) string {
	commits, err := listUnpushedCommits(ctx, worktree, branch, deps)
	if err != nil {
		return "(couldn't list unpushed commits: " + err.Error() + ")"
	}
	return strings.Join(commits, "\n")
}

// listUncommittedFiles returns the porcelain paths of every file
// that is currently dirty in the worktree (modified, added,
// deleted, renamed, copied, untracked). Used by dispatchPush to
// tell the user exactly what's still uncommitted after the agent
// claimed to be done.
func listUncommittedFiles(ctx context.Context, worktree string, deps HandlerDeps) ([]string, error) {
	out, _, err := deps.Git.Run(ctx, worktree, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var files []string
	for _, line := range splitNonEmptyLines(out) {
		if len(line) < 4 {
			continue
		}
		// porcelain: XY <path>  (XY is 2 chars + space, then path)
		// For renames/copies the format is `XY <orig> -> <new>` —
		// take the final path.
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		files = append(files, path)
	}
	return files, nil
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
