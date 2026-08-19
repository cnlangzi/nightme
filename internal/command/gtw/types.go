// Package gtw implements the nightme team-workflow command family
// (F-45: `/gtw fix <id>` and the underlying state machine).
//
// F-51 relocated gtw from `internal/gtw/` to
// `internal/command/gtw/`. The package is now part of the slash
// command layer; the chatsession package no longer knows about
// gtw's types or state. State lives in `gtw.Manager` (manager.go),
// keyed by chatID.
//
// Scope (v1):
//
//	/gtw fix <issue-id>           claim, label, create worktree, dispatch to agent
//	/gtw fix --name <branch>      create local worktree (no issue / no agent)
//
// Design constraints (carried from F-45):
//
//   - Zero new per-repo / per-user files: state lives in gtw.Manager
//     memory (states / drafts) and on the provider (GitHub / GitLab labels).
//   - Zero new OutboundKind: all output is plain text (the caller wraps
//     it into whatever OutboundKind the channel wants).
//   - The reaction-routing entry point is `Manager.HandleReaction`,
//     invoked from `services.ReactionRouter` — NOT from
//     ChatSession.HandleAction (the F-51 refactor removed that path).
//   - Credentials are borrowed from `gh auth token` / `glab auth status`.
//     nightme never persists its own tokens.
//
// Provider abstraction (GitProvider): see provider.go.
//
// The gtw package is gateway-agnostic. It directly imports the
// chatsession package for *ChatSession access (SelectedCwd,
// SetSelectedCwd, QueueUserMessage) — F-XX removed the Sender
// interface indirection that the previous F-51 design used.
//
// gtw never reads or stores *ChatSession on its own: slash
// commands receive cs from the dispatcher parameter; reactions
// receive cs from the runtime-layer wrapper. See manager.go for
// the wiring contract.
package gtw

import (
	"time"

	"github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Label* constants are the platform-side state-machine labels that
// /gtw uses. Hard-coded on purpose (F-45 §4.2): users do not configure
// the prefix; teams that need a different namespace fork the source.
//
// Every label carries the `nightme/` namespace so the gtw state is
// visually distinct from human-managed labels on the same issue.
const (
	LabelPrefix    = "nightme"
	LabelWIP       = "nightme/wip"
	LabelReady     = "nightme/ready"
	LabelReviewing = "nightme/reviewing"
	LabelRevise    = "nightme/revise"
	LabelDone      = "nightme/done"
	LabelStuck     = "nightme/stuck"
)

// AllLabels is the full set of state labels, in display order.
var AllLabels = []string{
	LabelWIP, LabelReady, LabelReviewing,
	LabelRevise, LabelDone, LabelStuck,
}

// State is the gtw lifecycle stage cached on the platform's
// label.
//
// v1 only writes "fixing" / "ready" / "canceled"; the rest are
// reserved for F-46+.
//
// F-51: defined natively in this package (was chatsession.GTWState
// pre-F-51). The state value strings are unchanged.
type State string

const (
	StateFixing   State = "fixing"
	StatePushing  State = "pushing"
	StateReady    State = "ready"
	StateCanceled State = "canceled"
)

// Mode tags which /gtw fix entry produced the in-flight snapshot.
//
//   - ModeRemote: `/gtw fix <id>` (default). Pulls issue from
//     GitHub/GitLab, dispatches the issue body to the active
//     AgentSession after worktree creation.
//   - ModeLocal:  `/gtw fix --name <branch>` (F-XX). No issue, no
//     label, no agent dispatch. Just creates a local worktree.
//
// Persisted in Context.Mode so rebuild after daemon restart can
// reconstruct the right state. Empty / unknown values are treated
// as ModeRemote (legacy entries predate this field).
type Mode string

const (
	ModeRemote Mode = "remote"
	ModeLocal  Mode = "local"
)

// Context is the per-chat snapshot of the in-flight /gtw fix.
// The zero value (with State == "") signals "no active fix".
//
// F-51: defined natively in this package (was chatsession.GTWContext
// pre-F-51). Field shape unchanged except for the Mode field
// added in F-XX. F-XX (close): RepoRoot / Repo / Provider added
// so /gtw close can switch CWD back + run git worktree remove
// from the right directory, and so /gtw push + /gtw pr have the
// remote info at hand without re-fetching on each dispatch.
type Context struct {
	Mode     Mode
	Issue    int // -1 for ModeLocal (no remote issue); > 0 for ModeRemote
	Branch   string
	Worktree string // absolute path; empty when the fix flow hasn't reached §5.2.④

	// RepoRoot is the main repository root (NOT the worktree).
	// Captured at /gtw fix time so /gtw close can (a) run
	// `git worktree remove` from inside the main repo — git
	// refuses to run that command from a worktree — and (b)
	// SetSelectedCwd back here after teardown. Empty when the
	// fix flow hasn't reached §5.2.④.
	RepoRoot string

	// Repo is the "owner/repo" form (single-slash, no scheme),
	// parsed from the origin remote URL at fix time. Empty for
	// ModeLocal. Consumed by /gtw push and /gtw pr to choose
	// between `gh` and `glab` without re-detecting each call.
	Repo string

	// Provider is "github" / "gitlab" / "" — picked by Detect at
	// fix time. Empty for ModeLocal. /gtw push and /gtw pr use
	// it to skip re-detection on each dispatch.
	Provider string

	State     State
	UpdatedAt time.Time
}

// DraftKind tags a pending user-confirmation choice. The set is
// closed; future flows (commit / pr) extend it.
type DraftKind string

const (
	DraftFixBranchExists DraftKind = "fix.branch-exists" // §5.3.1
	DraftFixLabelTaken   DraftKind = "fix.label-taken"   // §5.3.2
	DraftFixWorktreeFail DraftKind = "fix.worktree-fail" // §5.3.3
)

// FixDraftPayload is the typed payload for a DraftFix* entry.
// F-45 §3.4 / §5.3 + F-50 rename. The fields are the rollback-
// relevant subset of the original /gtw fix context.
type FixDraftPayload struct {
	IssueID int
	Title   string
	Branch  string
	Slug    string
	Repo    string // "owner/repo" (single-slash form)
	// Provider is the provider identity (ProviderGitHub /
	// ProviderGitLab). Used by the rollback path in action.go
	// to construct a fresh GitProvider for label removal.
	Provider string
	// Worktree is the directory gh/glab label-rollback calls
	// should spawn from. Set to the main repo root (always a
	// valid git dir) when the draft is emitted — the reaction
	// handler passes it through to NewProvider so `gh issue edit`
	// forks git from a directory that exists, even if the
	// daemon's own CWD has been stale'd since startup. See
	// internal/command/gtw/exec.go for the CWD contract this
	// defends against.
	Worktree string
	// GitError is the last 10 lines of stderr from the failed
	// `git worktree add` (only for DraftFixWorktreeFail).
	GitError string
	// AlreadyClaimedBy is the user ID holding nightme/wip on
	// the issue (only for DraftFixLabelTaken; v1 never emits).
	AlreadyClaimedBy string
	// LabelAdded is true iff nightme/wip was applied before the
	// draft was emitted. Rollback uses this to decide whether to
	// remove the label.
	LabelAdded bool
	// ChatID is the chat the /gtw fix was sent to. Required for
	// rendering follow-up replies. The handleFix path stores it
	// explicitly so reaction handlers don't have to look it up.
	ChatID string
}

// ChoiceOption is one button on a decision prompt. F-46 → action
// handler includes the original Options on the PATCH so the
// rebuilt prompt keeps the same layout (settled, selected id).
//
// F-51: defined natively in this package (was chatsession.ChoiceOption
// pre-F-51). Wire shape is messages.ChoiceOption.
type ChoiceOption = messages.ChoiceOption

// Draft is one pending user-confirmation choice indexed by
// Choice.RequestID. Channel inbound copies that RequestID onto
// ReactionEvent; Manager.HandleReaction looks up by it.
//
// F-51: defined natively in this package (was chatsession.GTWDraft
// pre-F-51). Stored in gtw.Manager.drafts[chatID][requestID].
type Draft struct {
	Kind    DraftKind
	Payload FixDraftPayload
	// CreatedAt is currently unused by the routing logic but is
	// useful for diagnostics (e.g. "draft sat for 30s without
	// reaction → expunge").
	CreatedAt time.Time

	// ChoicePosted is true when Send(OutChoice) succeeded. The
	// action handler PATCHes via Choice.RequestID when true, and
	// falls back to a plain-text follow-up when false (channel
	// choice path unavailable).
	ChoicePosted bool
	// Original choice render data so the action handler can
	// rebuild the prompt as Settled with a SelectedID
	// without going back to the dispatcher.
	ChoiceTitle     string
	ChoiceBody      string
	ChoiceOptions   []ChoiceOption
	ChoiceRequestID string
}

// ReactionEvent is the inbound reaction payload. Type alias to
// services.ReactionEvent (the canonical location). Old
// `chatsession.ReactionEvent` and the prior `gtw.ReactionEvent`
// struct both had identical fields (TargetMsgID / Emoji /
// UserID / ChatID), so callers compiled against either form
// can be migrated to this alias.
type ReactionEvent = services.ReactionEvent

// Choice represents the original decision prompt stored on a draft.
// Carries enough information for the action handler to rebuild
// it as Settled with a SelectedID (see executeXxxAction →
// deps.Send → Kind=OutChoicePatch path).
type Choice struct {
	Title     string
	Body      string
	Options   []ChoiceOption
	RequestID string
}

// F-XX removed `gtw.Sender` interface; the gtw package now
// imports chatsession directly and uses *chatsession.ChatSession
// for SelectedCwd / SetSelectedCwd / QueueUserMessage.
