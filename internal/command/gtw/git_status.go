package gtw

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// GitStatusSnapshot is the parsed result of a single
// `git status --porcelain --branch` invocation against a workspace.
// It is intentionally a pure value type (no methods) so it can be
// carried across package boundaries (gateway.StatusBar,
// runtime stamping) and tested without running git.
//
// Field semantics:
//
//	Branch         — current branch name. Empty when the workspace
//	                 is in detached HEAD state ("## HEAD (no branch)"
//	                 or "## (HEAD detached at <sha>)"). The Feishu
//	                 footer renders this as "⎇ ?" so users see
//	                 "branch unknown" without the underlying reason.
//	Added          — count of porcelain entries with X=='A'
//	                 (staged new files). "??" untracked entries
//	                 are NOT counted here — see Untracked.
//	Deleted        — count of porcelain entries with X=='D' OR
//	                 Y=='D'.
//	Modified       — count of porcelain entries with X=='M' OR
//	                 Y=='M' (plus R / C). Does NOT include conflict
//	                 entries — those live in Conflicts so the
//	                 footer can render a distinct "! N" segment without
//	                 double-counting.
//	Untracked      — count of "??" entries (files not in the index).
//	Conflicts      — count of unmerged conflict entries (UU /
//	                 AA / DD / AU / UA / DU / UD; the full X,Y ∈
//	                 {U,A,D} matrix is 9 codes — see
//	                 isConflictXY below). Tracked separately from
//	                 Modified so the footer can render a distinct
//	                 "! N" segment without double-counting. HasConflicts
//	                 is the boolean mirror (HasConflicts == Conflicts > 0)
//	                 and remains the source of truth for the F-57 /gtw
//	                 push and /gtw pr readiness gates.
//	AheadOfRemote  — number of local commits the upstream is
//	                 behind. Always 0 when HasUpstream is false.
//	HasUpstream    — true when the branch has an upstream tracking
//	                 ref ("## main...origin/main"). Detached HEAD
//	                 never has upstream; the Feishu footer omits
//	                 the "⇡ N" segment in that case.
//
// F-48 (follow-up to F-45): runtime stamps one of these on every
// OutboundMessage.StatusBar that flows to a main-chat footer
// render site. See docs/feat/F-45-session-footer.md §1.7.
//
// The canonical definition lives in internal/messages so the wire
// types package does not need to import the gtw package (avoids
// a gtw → messages → chatsession → gtw cycle). Existing gtw
// callers keep working via this alias.
type GitStatusSnapshot = messages.GitStatusSnapshot

// CollectReadiness runs `git -C <dir> status --porcelain --branch`
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
// CollectReadiness runs `git -C <dir> status --porcelain --branch`
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
//
// F-57: this is the single source of truth for the /gtw commit,
// /gtw push, and /gtw pr readiness gates. All three commands call
// it at entry; see docs/feat/F-57-gtw-push-pr-readiness.md §2.2.
// so push and commit gate on the same git truth.
//
// Note (F-237): the porcelain header trusts local config and
// cached remote-tracking refs, so HasUpstream=true here does NOT
// guarantee the branch is on origin — it just means local git
// believes it is. Dispatch entry points that gate on upstream
// truth (e.g. /gtw pr) should call CollectReadinessForDispatch
// instead so a fresh `git ls-remote` probe can catch the stale
// cached-ref case. The runtime footer path keeps using this
// function to avoid an extra network round-trip on every stamp.
func CollectReadiness(ctx context.Context, dir string, git GitRunner) (*GitStatusSnapshot, error) {
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

// CollectReadinessForDispatch is CollectReadiness plus a
// fresh `git ls-remote --heads origin <branch>` probe that
// catches the F-237 stale-cached-upstream bug.
//
// Bug shape: when branch.<name>.{remote,merge} is set in local
// git config AND refs/remotes/origin/<branch> is present as a
// cached remote-tracking ref BUT the branch was actually
// deleted server-side (or pulled into a sibling worktree from
// a stale clone), `git status --porcelain --branch` reports
// `## branch...origin/branch` with ahead=0/behind=0. The
// readiness gate's !HasUpstream check falls through, /gtw pr
// reaches gh, and gh explodes with a 4-line GraphQL diagnostic
// because the head ref isn't a branch on origin.
//
// Fix shape: after parsing the porcelain header, when
// HasUpstream=true we run a cheap `git ls-remote --heads origin
// <branch>` and treat empty output as "the cached ref lies —
// behave as if HasUpstream=false". AheadOfRemote / BehindRemote
// are zeroed in lockstep because they were derived against the
// cached SHA and are now meaningless (the branch isn't on
// origin to be ahead/behind of).
//
// Failure mode: a network blip / missing origin remote makes
// `git ls-remote` exit non-zero. We log nothing (no logger in
// scope) and leave the porcelain truth alone rather than flip
// a passing gate to failing on a transient outage — the
// dispatchPR defense-in-depth in pr.go catches the actual gh
// rejection if the cached ref really was stale and we got past
// the gate on a fluke.
//
// Callers: dispatchPR, dispatchPush, dispatchCommit. NOT the
// runtime footer path (ChatSession.GitStatus) — that one runs
// on every outbound stamp with a 3s budget and would pay for
// a network call per render. The footer doesn't gate on
// HasUpstream truth, so the porcelain approximation is fine.
func CollectReadinessForDispatch(ctx context.Context, dir string, git GitRunner) (*GitStatusSnapshot, error) {
	snap, err := CollectReadiness(ctx, dir, git)
	if err != nil || snap == nil {
		return snap, err
	}
	verifyUpstreamOnOrigin(ctx, dir, snap, git)
	return snap, nil
}

// verifyUpstreamOnOrigin mutates snap in place when the cached
// upstream ref is stale. No-op when the snapshot is nil,
// detached-HEAD, or already HasUpstream=false (no upstream to
// verify). On `git ls-remote` error the snapshot is left
// unchanged — see CollectReadinessForDispatch for the rationale.
func verifyUpstreamOnOrigin(ctx context.Context, dir string, snap *GitStatusSnapshot, git GitRunner) {
	if snap == nil || snap.Branch == "" || !snap.HasUpstream {
		// Detached HEAD (snap.Branch=="") or porcelain already
		// said no upstream — nothing to verify, and nothing to
		// flip.
		return
	}
	exists, err := RemoteBranchExists(ctx, dir, snap.Branch, git)
	if err != nil {
		// Cannot reach origin / no origin remote / etc.
		// Graceful fallback: keep the porcelain truth. The
		// dispatchPR stderr mapper in pr.go is the safety net
		// for the rare case where the cached ref really is
		// stale and we just couldn't tell.
		return
	}
	if !exists {
		// Cached upstream lied. Zero out the fields whose values
		// were derived against the cached SHA — they're now
		// meaningless and would otherwise let the gate fall
		// through to "ahead=0 / behind=0 / clean" → ready.
		snap.HasUpstream = false
		snap.AheadOfRemote = 0
		snap.BehindRemote = 0
	}
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
		// branch. Leave Branch empty (footer renders "⎇ ") and
		// HasUpstream false.
		snap.HasUpstream = false
	case strings.HasPrefix(header, "(HEAD detached at"):
		// Detached HEAD on a commit / remote ref — no branch name
		// we can show. Branch stays empty; footer renders "⎇ ".
		snap.HasUpstream = false
	default:
		// Active branch line: "name[...upstream[ [ahead N][, behind M]]]".
		name, hasUpstream, ahead, behind := parseBranchHeader(header)
		snap.Branch = name
		snap.HasUpstream = hasUpstream
		snap.AheadOfRemote = ahead
		snap.BehindRemote = behind
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
			// Untracked file — kept separate from Added so the
			// Feishu footer can render "? N" as its own segment
			// (iTerm2-aligned git status bar). Prior to the
			// status-bar split, "??" was lumped into Uncommitted;
			// the redesign exposes it as a distinct count without
			// folding it into "added".
			snap.Untracked++
		case line[0] == '!' && line[1] == '!':
			// Ignored file — not surfaced; only emitted with
			// --ignored which we don't pass.
			continue
		default:
			// F-57: distinguish conflict entries (UU / UA / UD / AU / AA / AD / DU / DA / DD)
			// from ordinary uncommitted edits. push and pr both hard-refuse
			// when HasConflicts; the readiness gate uses the flag instead of
			// re-detecting in dispatchPush/PR.
			//
			// Status-bar split: categorise the remaining porcelain
			// entries into Added / Deleted / Modified / Conflicts
			// so the Feishu footer can render `+ N / − N / ± N /
			// ! N` segments without double-counting. Renamed (R)
			// and copied (C) collapse into Modified — iTerm2 and
			// friends do the same, and git itself treats them as
			// variants of "modified". Conflict entries bump
			// Conflicts (NOT Modified) so the footer shows two
			// distinct, non-overlapping counts.
			if isConflictXY(line) {
				snap.HasConflicts = true
				snap.Conflicts++
				continue
			}
			x, y := line[0], line[1]
			switch {
			case x == 'A':
				snap.Added++
			case x == 'D' || y == 'D':
				snap.Deleted++
			default:
				// M / R / C — anything that means "the file
				// changed but wasn't added/deleted" lands here.
				// This deliberately groups R and C with M so the
				// footer can present a single "± N" count.
				snap.Modified++
			}
		}
	}
	return snap
}

// isConflictXY reports whether a porcelain status line's first two
// characters indicate an unresolved merge/rebase conflict.
//
// Git's full conflict matrix:
//
//	XY where X != ' ' and Y != ' ' and XY != "??" → conflict
//
// Specifically: any position is U, A, or D (with the same set on the
// other side or another conflict marker). The complete matrix is
// 3×3 = 9 codes — UU / UA / UD / AU / AA / AD / DU / DA / DD —
// all unmerged merge/rebase states. isConflictXY is the SINGLE
// source of truth for this classification in the gtw package;
// other readiness predicates (PushBlockReason / PRBlockReason)
// branch on the resulting HasConflicts flag rather than re-running
// the detection. (Pre-F-57 there was a sibling push.go helper
// that this function mirrored; F-57 collapsed the two call sites
// onto the snapshot's HasConflicts field, so isConflictXY now
// stands alone.)
func isConflictXY(line string) bool {
	if len(line) < 2 {
		return false
	}
	x, y := line[0], line[1]
	if x == ' ' || y == ' ' {
		return false
	}
	// Single source of truth for the conflict matrix: both X and
	// Y must be U, A, or D. '??' (untracked) and any other
	// porcelain marker fall out automatically — the for-loop is
	// the predicate.
	for _, c := range []byte{x, y} {
		if c != 'U' && c != 'A' && c != 'D' {
			return false
		}
	}
	return true
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
func parseBranchHeader(header string) (name string, hasUpstream bool, ahead, behind int) {
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
			switch {
			case strings.HasPrefix(part, "ahead "):
				if n, err := strconv.Atoi(strings.TrimSpace(part[len("ahead "):])); err == nil {
					ahead = n
				}
			case strings.HasPrefix(part, "behind "):
				// F-57: surfaced so the readiness gate can detect
				// "remote moved forward, local is stale" — a
				// senior-dev should pull --rebase before opening
				// a PR from a stale branch.
				if n, err := strconv.Atoi(strings.TrimSpace(part[len("behind "):])); err == nil {
					behind = n
				}
			}
		}
		headerMain = headerMain[:idx]
	}

	// Strip the optional "...upstream" suffix.
	if idx := strings.Index(headerMain, "..."); idx >= 0 {
		hasUpstream = true
		headerMain = headerMain[:idx]
	}

	name = strings.TrimSpace(headerMain)
	return name, hasUpstream, ahead, behind
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
// --branch` invocations per refresh (CollectReadiness in the
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
		// Same fallback as detectPR's caller in dispatchPR:
		// the workspace footer simply omits the PR-number
		// tail in this case.
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
	prov, err := detect(ctx, remoteURL, prober, dir)
	if err != nil || prov == nil {
		// Detection failed (invalid URL / unsupported host /
		// probe timeout). Fail-soft: the workspace footer
		// omits the PR-number tail and the next refresh will
		// retry.
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
