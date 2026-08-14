package messages

import (
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
)

// GitStatus is the workspace / git / PR context attached to every
// outbound message that flows from the runtime to a Channel.
//
// F-CLAUDE-PRINT-002: this is the consolidation of the legacy
// StatusBar wrapper. OutboundMessage.GitStatus *GitStatus is
// the per-chatsession snapshot rebuilt fresh on every read by
// ChatSession.GitStatus(ctx); the runtime event hook stamps it
// onto every outbound via the Emitter's GitStatusLookup closure.
// Channel adapters read this directly via formatStatusBarLines →
// formatGitLine.
//
// Sourced from chatsession: each ChatSession.GitStatus call
// invokes deps.CollectGit against the current workspace and
// deps.LookupPR against prcache.Cache (the only persistent
// layer; PR caching stays in prcache because gh/glab API
// round-trips are expensive). The runtime doesn't recompute;
// the chatsession is the single owner. nil / empty when the
// chat has no workspace or no AgentSession yet — Channel
// adapters treat nil as "no workspace line".
type GitStatus struct {
	Workspace   string
	Snapshot    *GitStatusSnapshot
	PullRequest *PR
}

// GitStatusSnapshot is the parsed result of a single
// `git status --porcelain --branch` invocation against a workspace.
// Pure value type (no exported state, no I/O) so it can be carried
// across package boundaries and tested without running git.
//
// F-57 adds read-only predicate methods on this type. They are
// defined here (not in the gtw package) because gtw uses a type
// alias (`type GitStatusSnapshot = messages.GitStatusSnapshot`) and
// Go forbids attaching new methods to the alias's underlying type
// from a different package. Keeping the predicates next to the
// field definitions also avoids an import cycle between messages
// and gtw: the predicates must NOT touch the gtw package or any
// transitive dep of it.
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
//  7. Untracked > 0 — git add, then commit, then push, then pr.
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
			"\n" +
			"💡 hint: resolve conflicts and `git add`, then /gtw pr"
	case !s.HasUpstream:
		return "❌ branch has no upstream on origin\n" +
			"\n" +
			"💡 hint: /gtw push first to publish the branch to origin, then /gtw pr"
	case s.AheadOfRemote > 0:
		return fmt.Sprintf(
			"⚠️ %d commit(s) made locally but not pushed\n"+
				"\n"+
				"💡 hint: /gtw push first, then /gtw pr", s.AheadOfRemote)
	case s.BehindRemote > 0:
		return fmt.Sprintf(
			"⚠️ origin/%s is %d commit(s) ahead of your local branch\n"+
				"\n"+
				"💡 hint: `git pull --rebase`, then /gtw pr", s.Branch, s.BehindRemote)
	case s.Added+s.Deleted+s.Modified > 0:
		return fmt.Sprintf(
			"⚠️ %d file(s) changed but not committed\n"+
				"\n"+
				"💡 hint: /gtw commit first, then /gtw push, then /gtw pr",
			s.Added+s.Deleted+s.Modified)
	case s.Untracked > 0:
		return fmt.Sprintf(
			"⚠️ %d new file(s) not added to git\n"+
				"\n"+
				"💡 hint: git add them, then /gtw commit, then /gtw push, then /gtw pr", s.Untracked)
	}
	return ""
}

// ToolInfo is the typed payload for OutboundMessage.Tool,
// representing a tool call (start or end). It captures the
// generic concepts that any tool has — name, args, output, error
// — without prescribing how each bridge represents them. Fields:
//
//	Name    — the tool's registered name (e.g. "Read", "Bash").
//	          Set on both Start and End.
//	Args    — the tool's input, in whatever representation the
//	          bridge chose. Set on both Start and End. Gateway
//	          does NOT parse this string; channels that want
//	          type-aware rendering (e.g. summarising tool output)
//	          parse it themselves.
//	Output  — the tool's result text. Only set on End; empty on
//	          Start.
//	Err     — the tool's error (if any). Only set on End; nil on
//	          Start.
//
// ToolInfo deliberately avoids naming fields after any specific
// bridge's schema (no `file_path`, `command`, `content`, etc.) —
// those are tool-specific details that the channel layer
// (with its own per-tool heuristics) handles.
type ToolInfo struct {
	Name   string
	Args   string
	Output string
	Err    error
}

// UsageInfo is the typed payload for OutUsage. See agent.UsageInfo
// for field semantics. Re-exported as a type alias here so existing
// gateway code (translate.go:158) keeps the same symbol name; the
// canonical definition lives in internal/agent (F-45 §2.1).
//
// (F-45): the comment block that used to live here was removed
// when the type moved to agent.UsageInfo. Old "InputTokens is the
// total input tokens ... (prompt + cache reads + tool input)" was
// misleading — InputTokens is the non-cached input count, NOT the
// sum with cache reads. Cache hits live in CacheReadInputTokens.
type UsageInfo = agent.UsageInfo

// Card is an interactive permission card or any other card that
// requires the user's choice.
//
// F-46: kind + choices + action encoding (see docs/feat/F-46-
// interactive-cards.md). The legacy Options field still works for
// callers that just want a flat list of button labels — build-
// InteractiveCard renders Options as primary buttons when Choices
// is empty.
type Card struct {
	Title    string
	Body     string
	Options  []string
	RequestID string
	Kind     CardKind
	Choices  []CardChoice
	Action   string
	Disabled bool
	ChosenChoiceEmoji string
	HeaderColor string
}

type CardKind int

const (
	CardKindPermission CardKind = iota
	CardKindDecision
	CardKindPreview
)

type CardChoice struct {
	Emoji  string
	Label  string
	Action string
}

type MessageStatePayload struct {
	State      agent.MessageState
	MessageID  string
	ReactionID string
	Emoji      string
}
