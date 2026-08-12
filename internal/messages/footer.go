package messages

import "fmt"

// Footer payload types — promoted from internal/command/gtw into
// the messages package so the StatusBar struct can stay in
// this package without creating an import cycle (chatsession →
// outbound → channel → messages → gtw → chatsession would loop
// through the gtw package; moving the leaf value types here keeps
// the dependency graph acyclic).

// GitStatusSnapshot is the parsed result of a single
// `git status --porcelain --branch` invocation against a workspace.
// Pure value type (no exported state, no I/O) so it can be carried
// across package boundaries (StatusBar, runtime stamping) and
// tested without running git.
//
// F-57 adds read-only predicate methods on this type. They are
// defined here (not in the gtw package) because gtw uses a type
// alias (`type GitStatusSnapshot = messages.GitStatusSnapshot`) and
// Go forbids attaching new methods to the alias's underlying type
// from a different package. Keeping the predicates next to the
// field definitions also avoids an import cycle between messages
// and gtw: the predicates must NOT touch the gtw package or any
// transitive dep of it.
//
// Field semantics:
//
//	Branch         — current branch name. Empty when the workspace
//	                 is in detached HEAD state ("## HEAD (no branch)"
//	                 or "## (HEAD detached at <sha>)"). The Feishu
//	                 footer renders this as "⎇ ?" so users see
//	                 "branch unknown" without the underlying reason.
//	Added          — count of porcelain entries with X=='A'
//	                 (staged new files). Excludes "??" untracked
//	                 entries (those are kept separate as Untracked
//	                 and rendered as "? N" by the Feishu footer).
//	                 Renamed/copied/conflict entries do NOT count
//	                 here — see Modified / HasConflicts.
//	Deleted        — count of porcelain entries with X=='D' OR
//	                 Y=='D' (a file removed in the index OR the
//	                 working tree).
//	Modified       — count of porcelain entries with X=='M' OR
//	                 Y=='M', plus renamed (R) and copied (C).
//	                 Does NOT include conflict entries — those live
//	                 in Conflicts so the `± N` segment doesn't double-
//	                 count alongside `! N`.
//	Untracked      — count of "??" entries (files not in the
//	                 index). Rendered as a separate "? N" segment
//	                 by the Feishu footer (iTerm2-aligned) rather
//	                 than folded into Added.
//	Conflicts      — count of unmerged conflict entries (UU / AA /
//	                 DD / AU / UA / DU / UD). Tracked separately
//	                 from Modified so the footer can render a
//	                 distinct "! N" segment without double-counting.
//	                 HasConflicts is the boolean mirror of this
//	                 field (HasConflicts == (Conflicts > 0)); kept
//	                 for /gtw push and /gtw pr readiness predicates
//	                 that branch on the flag.
//	AheadOfRemote  — number of local commits the upstream is
//	                 behind. Always 0 when HasUpstream is false.
//	BehindRemote   — number of upstream commits the local branch
//	                 is behind (i.e. origin/<branch> moved forward
//	                 via rebase/force-push and local didn't catch
//	                 up). Always 0 when HasUpstream is false. F-57
//	                 added this for the readiness gate (see §2.1 /
//	                 docs/feat/F-57-gtw-push-pr-readiness.md).
//	HasUpstream    — true when the branch has an upstream tracking
//	                 ref ("## main...origin/main"). Detached HEAD
//	                 never has upstream; the Feishu footer omits
//	                 the "⇡ N" segment in that case.
//	HasConflicts   — true when Conflicts > 0. Source of truth for
//	                 the readiness gate (F-57 §3.1 / §4.1); the
//	                 Feishu footer reads Conflicts directly to
//	                 render the "! N" count.
//
// F-48 (follow-up to F-45): runtime stamps one of these on every
// OutboundMessage.StatusBar that flows to a main-chat footer
// render site. See docs/feat/F-45-session-footer.md §1.7.
type GitStatusSnapshot struct {
	Branch        string
	Added         int
	Deleted       int
	Modified      int
	Untracked     int
	Conflicts     int
	AheadOfRemote int
	BehindRemote  int
	HasUpstream   bool
	HasConflicts  bool
}

// PR is the abstract cross-platform handle for a single Pull
// Request / Merge Request. Production has two impls (GitHub /
// GitLab) and the runtime caches one PR per AgentSession via
// prcache.Registry.
//
// Field semantics:
//
//	Number — platform-native PR number (GitHub: #N; GitLab: !N).
//	URL    — web URL to the PR / MR page.
//	State  — "open" / "merged" / "closed". v1 only requests
//	         PRs in the "open" state (GitHub's `pr list --state open`,
//	         GitLab's `mr list --state opened`); the field stays in
//	         the type so a future "show merged PR too" footer variant
//	         can flip the platform filter without changing the
//	         consumer.
type PR struct {
	Number int
	URL    string
	State  string
}

// -----------------------------------------------------------------------------
// F-57 readiness predicates
//
// The three atomic predicates below are the building blocks both /gtw push
// and /gtw pr compose to make their business decisions. They are deliberately
// pure functions on the snapshot — they do NOT call into the gtw package,
// touch git, or know anything about the push/pr commands' UX. The push and
// pr layers add their own composition logic (PushBlockReason / PRBlockReason)
// on top.
//
// Keeping these predicates in the messages package (where the struct lives)
// is a deliberate architectural choice — see the type-level comment for the
// import-cycle reasoning. The methods are accessed via the type alias
// `gtw.GitStatusSnapshot = messages.GitStatusSnapshot` from the gtw layer.
// -----------------------------------------------------------------------------

// HasUpstreamBranch reports whether this snapshot's branch exists on the
// origin remote (i.e. has an upstream tracking ref). Detached HEAD never
// has upstream, even if the porcelain header still parses.
func (s *GitStatusSnapshot) HasUpstreamBranch() bool {
	return s.Branch != "" && s.HasUpstream
}

// LocalIsAtUpstreamTip reports whether the local branch tip is exactly the
// same commit as origin/<branch> — i.e. there are zero unpushed local
// commits AND zero upstream commits the local branch hasn't caught up to.
//
// HasUpstream must be true for this to be meaningful; without an upstream
// reference there is no "tip" to compare against. The function returns
// false in that case rather than panicking, which is the safer default
// for a "is everything pushed?" check.
func (s *GitStatusSnapshot) LocalIsAtUpstreamTip() bool {
	return s.HasUpstreamBranch() && s.AheadOfRemote == 0 && s.BehindRemote == 0
}

// WorkingTreeIsClean reports whether the working tree has nothing to commit:
// no modified / staged / deleted / renamed entries, no untracked files, and
// no unresolved merge/rebase conflicts.
//
// This is the senior-dev "before opening a PR" hygiene gate. It does NOT
// check upstream alignment (see LocalIsAtUpstreamTip for that).
func (s *GitStatusSnapshot) WorkingTreeIsClean() bool {
	return s.Added == 0 && s.Deleted == 0 &&
		s.Modified == 0 && s.Untracked == 0 &&
		s.Conflicts == 0
}

// HasNothingToPush reports whether /gtw push should bail out with a
// "nothing to push" informational message. Returns true only when:
//
//   - the working tree is fully committed (WorkingTreeIsClean), AND
//   - the branch exists on origin (HasUpstreamBranch), AND
//   - local is at upstream tip (AheadOfRemote == 0)
//
// Note that "no upstream at all" deliberately returns FALSE — a branch
// that has never been pushed is exactly the case /gtw push needs to handle
// (programmaticPush runs `git push -u origin <branch>` for that). Calling
// /gtw push on such a branch is the happy path, not a no-op.
func (s *GitStatusSnapshot) HasNothingToPush() bool {
	return s.WorkingTreeIsClean() &&
		s.HasUpstreamBranch() &&
		s.AheadOfRemote == 0
}

// PushBlockReason returns the single hard-refuse reason for /gtw push, or
// "" if push should proceed. Currently the only hard-refuse condition is
// unresolved merge/rebase conflicts — pushing an unresolved state would
// land a broken tip on origin and is rejected at the gate.
//
// All other "not ready" conditions (uncommitted files, no upstream, etc.)
// are NOT hard-refused here; the agent commit path or programmaticPush
// handle them. See F-57 §3.1.
func (s *GitStatusSnapshot) PushBlockReason() string {
	if s.HasConflicts {
		return "❌ worktree has unmerged paths (merge/rebase conflict)\n" +
			"hint: resolve conflicts and `git add`, OR `git rebase --abort` / `git merge --abort`"
	}
	return ""
}

// IsReadyForPR reports whether /gtw pr should proceed past the readiness
// gate. A worktree is "ready" iff:
//
//   - the branch exists on origin (HasUpstreamBranch), AND
//   - local branch tip == upstream tip (LocalIsAtUpstreamTip), AND
//   - working tree is fully committed (WorkingTreeIsClean)
//
// /gtw push's successful exit guarantees this (modulo race); see F-57 §5
// for the continuity proof.
func (s *GitStatusSnapshot) IsReadyForPR() bool {
	return s.HasUpstreamBranch() && s.LocalIsAtUpstreamTip() && s.WorkingTreeIsClean()
}

// PRBlockReason returns the single actionable reason /gtw pr is being
// refused, in priority order (hard blocks first, then cleanup nudges).
//
// Priority rationale:
//  1. Branch == "" (detached HEAD) — refuse first; there's no ref to PR
//     from. Cannot be "fixed" by /gtw push; the user must checkout.
//  2. HasConflicts — refuse; resolve manually.
//  3. !HasUpstream — this is the bug case F-57 fixes: branch was never
//     pushed to origin. Direct the user to /gtw push.
//  4. AheadOfRemote > 0 — local has unpushed commits; direct to push.
//     NOTE: if BOTH ahead > 0 AND behind > 0 (diverged — local
//     rebased on stale origin), the ahead branch wins. User must
//     push first; the resulting push will fail if remote has new
//     commits, but that's the "git push --force-with-lease" path,
//     not a /gtw pr concern.
//  5. BehindRemote > 0 — remote moved forward; user must rebase.
//  6. Added / Deleted / Modified > 0 — working tree dirty. /gtw
//     push no longer auto-commits; direct to /gtw commit first.
//     All three categories share a single hint line so /gtw pr's
//     refusal reads as one "working tree has uncommitted work"
//     statement, not three.
//  7. Untracked > 0 — git add, then commit, then push.
//
// The function returns "" when IsReadyForPR() returns true (no block).
// Callers should check IsReadyForPR first and call PRBlockReason only on
// the negative path, but the "" return here is also safe to ignore.
func (s *GitStatusSnapshot) PRBlockReason() string {
	switch {
	case s.Branch == "":
		return "❌ detached HEAD — checkout a named branch first"
	case s.HasConflicts:
		return "❌ worktree has unmerged paths (merge/rebase conflict)\n" +
			"hint: resolve conflicts and `git add`, then /gtw pr"
	case !s.HasUpstream:
		return "❌ branch has no upstream on origin\n" +
			"hint: /gtw push first to publish the branch to origin, then /gtw pr"
	case s.AheadOfRemote > 0:
		return fmt.Sprintf(
			"⚠️ %d commit(s) made locally but not pushed\n"+
				"hint: /gtw push first, then /gtw pr", s.AheadOfRemote)
	case s.BehindRemote > 0:
		return fmt.Sprintf(
			"⚠️ origin/%s is %d commit(s) ahead of your local branch\n"+
				"hint: `git pull --rebase`, then /gtw pr", s.Branch, s.BehindRemote)
	case s.Added+s.Deleted+s.Modified > 0:
		return fmt.Sprintf(
			"⚠️ %d file(s) changed but not committed\n"+
				"hint: /gtw commit first, then /gtw push, then /gtw pr",
			s.Added+s.Deleted+s.Modified)
	case s.Untracked > 0:
		return fmt.Sprintf(
			"⚠️ %d new file(s) not added to git\n"+
				"hint: git add them, then /gtw commit, then /gtw push, then /gtw pr", s.Untracked)
	}
	return ""
}