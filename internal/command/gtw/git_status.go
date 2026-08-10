package gtw

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// GitStatusSnapshot is the parsed result of a single
// `git status --porcelain --branch` invocation against a workspace.
// It is intentionally a pure value type (no methods) so it can be
// carried across package boundaries (gateway.SessionContext,
// runtime stamping) and tested without running git.
//
// Field semantics:
//
//	Branch         — current branch name. Empty when the workspace
//	                 is in detached HEAD state ("## HEAD (no branch)"
//	                 or "## (HEAD detached at <sha>)"). The Feishu
//	                 footer renders this as "⎇ ?" so users see
//	                 "branch unknown" without the underlying reason.
//	Uncommitted    — count of porcelain entries that are NOT
//	                 untracked or ignored. Includes modified (M),
//	                 added (A), deleted (D), renamed (R), copied
//	                 (C), and unmerged conflict entries (UU, AA,
//	                 etc.). Excludes "!!" ignored lines.
//	Untracked      — count of "??" entries (files not in the index).
//	AheadOfRemote  — number of local commits the upstream is
//	                 behind. Always 0 when HasUpstream is false.
//	HasUpstream    — true when the branch has an upstream tracking
//	                 ref ("## main...origin/main"). Detached HEAD
//	                 never has upstream; the Feishu footer omits
//	                 the "⇡ N" segment in that case.
//
// F-48 (follow-up to F-45): runtime stamps one of these on every
// OutboundMessage.SessionContext that flows to a main-chat footer
// render site. See docs/feat/F-45-session-footer.md §1.7.
type GitStatusSnapshot struct {
	Branch        string
	Uncommitted   int
	Untracked     int
	AheadOfRemote int
	HasUpstream   bool
}

// CollectStatus runs `git -C <dir> status --porcelain --branch`
// and parses the output into a GitStatusSnapshot.
//
// Returns (nil, nil) when:
//   - dir is not inside a git working tree
//   - the porcelain output is empty / malformed
//   - the underlying git invocation fails for any reason
//
// The footer render path treats (nil, nil) as "no git segment".
// We intentionally do NOT propagate errors upward — the footer
// is decorative metadata, not a correctness-critical signal, and
// blocking an outbound message because `git status` is slow or
// broken would be the wrong trade-off (chat UX > git visibility).
//
// `git` is the runner abstraction; production passes
// `gtw.ExecGitRunner{}`, tests pass a fakeGit with canned
// porcelain output. This avoids any package-level mutable state
// or test-only hooks in production code paths.
func CollectStatus(ctx context.Context, dir string, git GitRunner) (*GitStatusSnapshot, error) {
	out, stderr, err := git.Run(ctx, dir, "status", "--porcelain", "--branch",
		"--untracked-files=normal")
	if err != nil {
		// Non-zero exit: not-a-git-repo, IO error, etc. We swallow
		// stderr here on purpose — the footer render path doesn't
		// surface it. A separate warn-level log line in the
		// runtime caller is fine; not this layer's concern.
		_ = stderr
		return nil, nil
	}
	snap := parsePorcelainBranchStatus(out)
	if snap == nil {
		return nil, nil
	}
	return snap, nil
}

// parsePorcelainBranchStatus parses the exact output format of
// `git status --porcelain --branch`. The first non-empty line is
// always the branch header ("## <info>"); subsequent lines are
// status entries (2 chars + space + filename).
//
// Returns nil when the input is empty / lacks a branch header —
// that's the "not in a repo" or "empty repo with no commits" case.
//
// Format references:
//
//	## main                                           (local only)
//	## main...origin/main                             (with upstream)
//	## main...origin/main [ahead 3]                   (3 local unpushed)
//	## main...origin/main [ahead 3, behind 1]         (diverged)
//	## (HEAD detached at 1234abc)                     (detached at sha)
//	## (HEAD detached at origin/main)                 (detached on remote)
//	## HEAD (no branch)                               (initial commit / orphan)
func parsePorcelainBranchStatus(out string) *GitStatusSnapshot {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		return nil
	}
	snap := &GitStatusSnapshot{}

	// Parse branch header (first line).
	header := strings.TrimPrefix(lines[0], "## ")
	switch {
	case header == "HEAD (no branch)":
		// Fresh repo with zero commits — git refuses to name a
		// branch. Leave Branch empty (footer renders "⎇ ?") and
		// HasUpstream false.
		snap.HasUpstream = false
	case strings.HasPrefix(header, "(HEAD detached at"):
		// Detached HEAD on a commit / remote ref — no branch name
		// we can show. Branch stays empty; footer renders "⎇ ?".
		snap.HasUpstream = false
	default:
		// Active branch line: "name[...upstream[ [ahead N][, behind M]]]".
		name, hasUpstream, ahead := parseBranchHeader(header)
		snap.Branch = name
		snap.HasUpstream = hasUpstream
		snap.AheadOfRemote = ahead
	}

	// Parse status entries (remaining lines).
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if len(line) < 3 {
			// Malformed entry — skip defensively rather than
			// panic on index out of range.
			continue
		}
		switch {
		case line[0] == '?' && line[1] == '?':
			// Untracked file.
			snap.Untracked++
		case line[0] == '!' && line[1] == '!':
			// Ignored file — not surfaced; only emitted with
			// --ignored which we don't pass.
			continue
		default:
			// Everything else counts as "uncommitted": modified
			// (M), staged add/delete/rename/copy (A/D/R/C),
			// and unmerged conflict entries (UU, AA, DD, etc.).
			snap.Uncommitted++
		}
	}
	return snap
}

// parseBranchHeader extracts (localBranchName, hasUpstream,
// aheadCount) from a `##` header line body. Examples:
//
//	"main"                              -> ("main", false, 0)
//	"main...origin/main"                -> ("main", true,  0)
//	"main...origin/main [ahead 3]"      -> ("main", true,  3)
//	"feat/x...origin/feat/x [ahead 3, behind 1]" -> ("feat/x", true, 3)
//
// Returns (name, false, 0) when the header doesn't match the
// expected shape (defensive — git could theoretically add new
// forms).
func parseBranchHeader(header string) (name string, hasUpstream bool, ahead int) {
	// Strip the optional " [ahead N[, behind M]]" suffix.
	headerMain := header
	if idx := strings.Index(headerMain, " ["); idx >= 0 {
		suffix := headerMain[idx+2:]
		if end := strings.Index(suffix, "]"); end >= 0 {
			suffix = suffix[:end]
		}
		// Inside the brackets: "ahead N" / "behind M" / "ahead N, behind M".
		for _, part := range strings.Split(suffix, ", ") {
			part = strings.TrimSpace(part)
			if rest, ok := strings.CutPrefix(part, "ahead "); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
					ahead = n
				}
			}
			// "behind M" intentionally not surfaced — the user
			// asked only for unpushed (ahead), not behind. If we
			// ever surface "behind", add a similar parsing branch
			// here and a new field on GitStatusSnapshot.
		}
		headerMain = headerMain[:idx]
	}

	// Strip the optional "...upstream" suffix.
	if idx := strings.Index(headerMain, "..."); idx >= 0 {
		hasUpstream = true
		headerMain = headerMain[:idx]
	}

	name = strings.TrimSpace(headerMain)
	return name, hasUpstream, ahead
}

// CollectPR resolves the workspace's remote + provider and asks
// for the open PR / MR associated with the given head branch.
// Returns (nil, nil) when:
//
//   - `head` is empty (detached HEAD — no branch to look up)
//   - the workspace has no `origin` remote
//   - the git platform cannot be detected (URL hint ambiguous
//     AND Stage B probe failed)
//   - the platform call fails (auth, network, rate limit,
//     5xx) — the caller treats this exactly like "no PR yet"
//     rather than bubbling an error to the IM
//   - no open PR exists for `head`
//
// The function does NOT itself run `git status` to discover
// `head`; the caller (AgentSession.refreshPRRef) supplies the
// already-parsed branch. Splitting the responsibility avoids
// the stamp path paying for two `git status --porcelain
// --branch` invocations per refresh (CollectStatus in the
// stamp path + CollectPR via the cache goroutine).
//
// `deps.Detect` follows the same fallback rule the rest of
// the gtw package uses: nil → package-level Detect (URL hint
// + API probe). `deps.Prober` nil → ExecHTTPProber{Timeout:
// 3s}. Both dependencies are unused when `deps.Detect` is
// non-nil (the test-only fakeDetect path) — Detect's Stage A
// hint + Stage B probe are the only consumers of Prober.
func CollectPR(ctx context.Context, dir, head string, deps HandlerDeps) (*PR, error) {
	if head == "" {
		// Detached HEAD / fresh repo without a commit — the
		// provider can't match a "head branch" against PR
		// queries. Skip the network round-trip entirely.
		return nil, nil
	}
	remoteURL, err := RemoteOriginURL(ctx, dir, deps.Git)
	if err != nil || remoteURL == "" {
		// Not a git repo, or remote `origin` not configured.
		// Same fallback as detectP R's caller in dispatchPR:
		// footer simply omits Line 4 in this case.
		return nil, nil
	}
	detect := deps.Detect
	if detect == nil {
		detect = Detect
	}
	prober := deps.Prober
	if prober == nil {
		prober = &ExecHTTPProber{Timeout: 3 * time.Second}
	}
	prov, err := detect(ctx, remoteURL, prober)
	if err != nil || prov == nil {
		// Detection failed (invalid URL / unsupported host /
		// probe timeout). Fail-soft: footer omits Line 4 and
		// the next refresh will retry.
		return nil, nil
	}
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		// Remote URL isn't parseable into owner/repo — usually
		// a self-hosted shape ParseRepoOwner doesn't handle
		// yet. Don't escalate; just skip this refresh window.
		return nil, nil
	}
	return prov.GetPR(ctx, owner, repo, head)
}