package messages

import (
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
	// UpstreamGone is true when the branch has an upstream configured
	// (HasUpstream=true, so `git status` prints "## b...origin/b [gone]")
	// but refs/remotes/origin/<branch> no longer exists locally — the
	// remote branch was deleted (and pruned), a `git push -u` was
	// rejected but still wrote branch.<name>.merge, or a manual
	// `git branch --set-upstream-to=origin/<b>` ran without a fetch.
	// git cannot compute AheadOfRemote in this state, so it reports
	// [gone] with ahead=0; see HasNothingToPush for why this flag must
	// short-circuit that predicate to false.
	UpstreamGone bool
	HasConflicts bool
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
// pr layers add their own composition logic (PushBlockReason)
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
//   - local is at upstream tip (AheadOfRemote == 0), AND
//   - the upstream is NOT gone (issue #235: a "ghost upstream" —
//     branch.<name>.merge configured but refs/remotes/origin/<name>
//     missing — makes `git status --porcelain --branch` emit
//     `## branch...origin/branch [gone]`, which parseBranchHeader reads
//     as HasUpstream=true, ahead=0. Without the UpstreamGone guard this
//     predicate would wrongly bail with "nothing to push" and strand
//     genuinely-unpushed commits; the push must instead proceed so
//     `git push -u origin <branch>` re-publishes the branch and
//     materialises the tracking ref.)
//
// Note that "no upstream at all" deliberately returns FALSE — a branch
// that has never been pushed is exactly the case /gtw push needs to handle
// (programmaticPush runs `git push -u origin <branch>` for that). Calling
// /gtw push on such a branch is the happy path, not a no-op.
func (s *GitStatusSnapshot) HasNothingToPush() bool {
	return s.WorkingTreeIsClean() &&
		s.HasUpstreamBranch() &&
		s.AheadOfRemote == 0 &&
		!s.UpstreamGone
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

// Choice is an interactive prompt that requires the user's pick
// (permission, clarifying question, or gtw decision). Channel may
// render it as a native card; this type is not a platform schema.
//
// Kind is semantic only. Visual chrome (title emoji, header
// colour, button layout, in-card paging) belongs in the Channel.
type Choice struct {
	RequestID string
	Kind      ChoiceKind
	Title     string
	Body      string
	Options   []ChoiceOption
	// Questions is AskUserQuestion content. Empty for approvals
	// and gtw decisions. Channel owns any in-progress paging.
	Questions []ChoiceQuestion
	// Settled means the prompt no longer accepts a pick.
	// SelectedID is the chosen option ID; empty on dashboard settle.
	Settled    bool
	SelectedID string
}

// ChoiceQuestion is one AskUserQuestion item. Options are the
// selectable answers for that item.
type ChoiceQuestion struct {
	ID       string
	Header   string
	Question string
	Options  []ChoiceOption
}

type ChoiceKind int

const (
	ChoiceKindPermission ChoiceKind = iota
	ChoiceKindQuestion
	ChoiceKindDecision
)

// ChoiceOption is one selectable answer. ID is what comes back on
// pick (permission: the option label; gtw: act:/gtw/...). Emoji is
// an optional identity (gtw reaction key), not button chrome.
type ChoiceOption struct {
	ID    string
	Label string
	Emoji string
}

// ChoiceOptionsFromLabels maps a flat label list onto Options
// whose ID equals Label (permission / question answers).
func ChoiceOptionsFromLabels(labels []string) []ChoiceOption {
	if len(labels) == 0 {
		return nil
	}
	out := make([]ChoiceOption, len(labels))
	for i, l := range labels {
		out[i] = ChoiceOption{ID: l, Label: l}
	}
	return out
}

// MessageStatePayload carries the data an adapter needs to render a
// per-message state reaction on top of the originating inbound user
// message. The adapter decides HOW to render each state (feishu uses
// its predefined emoji_type set; telegram uses unicode codepoints);
// the runtime only forwards the abstract State value.
//
// ReactionID is reserved for platforms that return an opaque handle
// when adding a reaction (so the adapter can address it on removal).
// Telegram's setMessageReaction replaces the entire reaction set and
// does not return per-emoji IDs, so ReactionID stays "" for telegram.
type MessageStatePayload struct {
	State      agent.MessageState
	MessageID  string
	ReactionID string
}
