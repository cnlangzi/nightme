package gtw

import (
	"context"
	"fmt"
	"strings"
)

// headSHA returns the current HEAD's full SHA in worktree.
// Used by /gtw commit (the agent-commit flow):
//
//   - dispatchCommit: captures headBefore so verifyAgentCommitted
//     can detect "agent claimed success but didn't commit" (a
//     class of failure where status is empty and unpushed count
//     is 0, but no commit actually landed).
//
//   - verifyAgentCommitted: reads headAfter to compare against
//     headBefore. Errors here surface as a diagnostic in the
//     reply.
//
// Note: /gtw push (dispatchPush) used to capture headBefore too,
// per F-56 §B3 — but after the F-XX commit/push split it dropped
// that step because headBefore..origin/<branch> collapses to
// empty after a successful push (origin/<branch> == local tip
// == headBefore). The success card now uses originBefore instead
// (see originBranchSHA + dispatchPush).
//
// Returns the SHA with surrounding whitespace trimmed (git's
// output is one line, but the test fake may not be).
func headSHA(ctx context.Context, worktree string, deps HandlerDeps) (string, error) {
	out, _, err := deps.Git.Run(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// originBranchSHA returns the SHA of origin/<branch>, or "" when
// the remote-tracking ref does not exist (i.e. the branch has
// never been pushed — `git push -u origin <branch>` will create
// it on this push).
//
// Used by dispatchPush to capture the "before" tip of origin
// before pushing, so the success-card rev range can list the
// exact commits that just landed. Without this, the range would
// be `headBefore..origin/<branch>` — but after a successful push
// origin/<branch> == headBefore, so the range collapses to empty
// and the card reports "pushed 0 commit(s)".
//
// Errors other than "unknown ref" propagate so the caller can
// distinguish "first push" (== "") from "git is broken" (= err).
func originBranchSHA(ctx context.Context, worktree, branch string, deps HandlerDeps) (string, error) {
	out, stderr, err := deps.Git.Run(ctx, worktree,
		"rev-parse", "--verify", "origin/"+branch)
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	if strings.Contains(stderr, "unknown revision or path not in the working tree") ||
		strings.Contains(stderr, "Not a valid ref") {
		return "", nil
	}
	return "", fmt.Errorf("rev-parse origin/%s: %w (stderr: %s)",
		branch, err, strings.TrimSpace(stderr))
}

// firstPushRevRange picks a `git log` rev range that lists
// exactly the commits a first-push landed — not the entire local
// branch history.
//
// Setup: origin/<branch> did not exist before the push, so the
// `originBefore..origin/<branch>` shape used in the regular path
// is unavailable. The naive fallback `git log <branch>` includes
// every reachable ancestor (main's whole history, when the branch
// was forked from main) and so wildly over-counts.
//
// Better: anchor against origin/<default>. For a feature branch
// pushed from a repo where origin/main already exists, the push
// itself only uploads commits unique to that branch — exactly
// `origin/main..origin/<feature>`.
//
// Two sub-cases fall through to `c.Branch`:
//   - DefaultBranch lookup failed (brand-new repo, no origin
//     remote, no origin/HEAD): the only thing on origin is what
//     we just pushed, so the whole branch IS the upload.
//   - c.Branch IS the default (pushing main itself for the
//     first time): origin/main didn't exist before, but we just
//     populated it with everything reachable from local main.
//     No other ref exists to subtract against.
//
// Output is the bare arg string passed to `git log`. Errors from
// the lookup are logged (returned as c.Branch) rather than
// surfaced — at this point the push has already succeeded and a
// best-effort card beats refusing to render one.
func firstPushRevRange(ctx context.Context, c Context, deps HandlerDeps) string {
	defaultBranch, err := DefaultBranch(ctx, c.RepoRoot, deps.Git)
	if err != nil || defaultBranch == "" || defaultBranch == c.Branch {
		return c.Branch
	}
	return "origin/" + defaultBranch + "..origin/" + c.Branch
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

// programmaticPushWithRetry runs `git push -u origin <branch>` with
// one retry on transient failure. Per F-56 §4.3, this is the
// single source of truth for "did the push land": countUnpushed
// (NOT the push command's exit code — git can exit 0 with commits
// silently not landing on the remote, or exit non-zero with the
// branch already up to date).
//
// Returns nil on success (unpushed == 0 after at most one retry),
// or an error whose Error() is a complete IM-friendly message
// including the unpushed commit list — caller can paste it
// straight into the reply.
func programmaticPushWithRetry(ctx context.Context, deps HandlerDeps, c Context) error {
	// Attempt 1.
	out1, _ := programmaticPush(ctx, deps, c)
	if ok, err := unpushedIsZero(ctx, deps, c); err != nil {
		return fmt.Errorf("verify after push: %w", err)
	} else if ok {
		return nil
	}

	// Attempt 2 (retry on transient failure).
	out2, _ := programmaticPush(ctx, deps, c)
	if ok, err := unpushedIsZero(ctx, deps, c); err != nil {
		return fmt.Errorf("re-count after retry: %w", err)
	} else if ok {
		return nil
	}

	// Both attempts verified-failed. Build the diagnostic.
	unpushed, _ := countUnpushed(ctx, c.Worktree, c.Branch, deps)
	return fmt.Errorf(
		"❌ %d commit(s) on %s still don't appear on origin/%s after retry\n"+
			"first attempt output: %s\n"+
			"retry output: %s\n"+
			"hint: check `git push -v %s` — likely network or remote protection rule.\n\n"+
			"unpushed commits:\n%s",
		unpushed, c.Branch, c.Branch,
		strings.TrimSpace(out1), strings.TrimSpace(out2), c.Branch,
		unpushedCommitsForDisplay(ctx, c.Worktree, c.Branch, deps),
	)
}

// unpushedIsZero is the countUnpushed → bool convenience used by
// programmaticPushWithRetry's verify loop. Errors propagate so the
// caller can distinguish "no commits to push" (true) from
// "couldn't read state" (false + err).
func unpushedIsZero(ctx context.Context, deps HandlerDeps, c Context) (bool, error) {
	n, err := countUnpushed(ctx, c.Worktree, c.Branch, deps)
	if err != nil {
		return false, err
	}
	return n == 0, nil
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
// dispatchPush's Branch 1 (clean + 0 unpushed) treats that as
// "nothing to push" and exits with the no-op IM card. Branch 2
// (dirty) still works: the agent's commits get the upstream
// established when nightme runs programmaticPushWithRetry.
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

// gitLogRange returns the `<sha> <subject>` oneline of every
// commit in `revRange` (e.g. "headBefore..HEAD" for commit, or
// "originBefore..origin/<branch>" for push — see dispatchPush
// for why push switched off `headBefore..origin/<branch>` which
// collapses to empty after a successful push). Used by
// replyCommitSuccessCard and replyPushSuccessCard to build the
// IM card directly from git state — never from agent prose.
//
// Output is the raw `git log --oneline` text, trimmed. Caller
// can split on "\n" if it wants structured access.
//
// An empty range (no commits) returns "" + nil. Errors other
// than "bad revision" propagate.
func gitLogRange(ctx context.Context, worktree, revRange string, deps HandlerDeps) (string, error) {
	out, stderr, err := deps.Git.Run(ctx, worktree, "log", "--oneline", revRange)
	if err != nil {
		// Empty rev range (no commits) often surfaces as a non-fatal
		// error from `git log`; treat that as "no output, no error".
		// Caller can render an empty list.
		if strings.Contains(stderr, "does not have any commits") ||
			strings.Contains(stderr, "unknown revision") {
			return "", nil
		}
		return "", fmt.Errorf("git log %s: %w (stderr: %s)", revRange, err, strings.TrimSpace(stderr))
	}
	return strings.TrimRight(out, "\n"), nil
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
