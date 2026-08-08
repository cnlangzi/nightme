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
// chatsession package for *ChatSession access (ActiveCwd,
// SetActiveCwd, QueueUserMessage) — F-XX removed the Sender
// interface indirection that the previous F-51 design used.
//
// `cmd/nightme/run.go` wires per-chat *ChatSession lookup into
// `Manager` via SetGetChatSession at startup.
package gtw

import (
	"context"
	"time"

	"github.com/cnlangzi/nightme/internal/command/services"
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
// from the right directory, and so future /gtw push has the
// remote info at hand without re-fetching.
type Context struct {
	Mode     Mode
	Issue    int    // -1 for ModeLocal (no remote issue); > 0 for ModeRemote
	Branch   string
	Worktree string // absolute path; empty when the fix flow hasn't reached §5.2.④

	// RepoRoot is the main repository root (NOT the worktree).
	// Captured at /gtw fix time so /gtw close can (a) run
	// `git worktree remove` from inside the main repo — git
	// refuses to run that command from a worktree — and (b)
	// SetActiveCwd back here after teardown. Empty when the
	// fix flow hasn't reached §5.2.④.
	RepoRoot string

	// Repo is the "owner/repo" form (single-slash, no scheme),
	// parsed from the origin remote URL at fix time. Empty for
	// ModeLocal. Reserved for /gtw push (F-XX) and /gtw pr.
	Repo string

	// Provider is "github" / "gitlab" / "" — picked by Detect at
	// fix time. Empty for ModeLocal. /gtw push / /gtw pr use it
	// to choose between `gh` and `glab`.
	Provider string

	State     State
	UpdatedAt time.Time
}

// DraftKind tags a pending user-confirmation card. The set is
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
	IssueID  int
	Title    string
	Branch   string
	Slug     string
	Repo     string // "owner/repo" (single-slash form)
	// Provider is the provider identity (ProviderGitHub /
	// ProviderGitLab). Used by the rollback path in action.go
	// to construct a fresh GitProvider for label removal.
	Provider string
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

// CardChoice is one button on a decision card. F-46 → action
// handler includes the original Choices on the PATCH so the
// rebuilt card keeps the same layout (every button disabled).
//
// F-51: defined natively in this package (was chatsession.CardChoice
// pre-F-51). Identical field shape.
type CardChoice struct {
	Emoji  string
	Label  string
	Action string
}

// Draft is one pending user-confirmation card indexed by the
// bot reply's userMsgID. Reactions on that message id route to
// the matching draft via Manager.HandleReaction.
//
// F-51: defined natively in this package (was chatsession.GTWDraft
// pre-F-51). Stored in gtw.Manager.drafts[chatID][userMsgID].
type Draft struct {
	Kind    DraftKind
	Payload FixDraftPayload
	// CreatedAt is currently unused by the routing logic but is
	// useful for diagnostics (e.g. "draft sat for 30s without
	// reaction → expunge").
	CreatedAt time.Time

	// F-46: bot-side message id of the rendered decision card.
	// Populated by the dispatcher after SendCard returns; consumed
	// by the action handler's follow-up PATCH. Empty when the
	// dispatcher never sent a card (e.g. legacy text-only
	// fallbacks).
	BotMessageID string
	// F-46: original card render data so the action handler can
	// rebuild the card with `Disabled: true` and a result note
	// without going back to the dispatcher.
	CardTitle     string
	CardBody      string
	CardChoices   []CardChoice
	CardRequestID string
}

// ReactionKind tags a single user-emoji response on a gtw draft card.
// Each draft kind has a fixed set of accepted reaction kinds; other
// emojis are no-ops.
type ReactionKind string

const (
	// ReactionConfirm: ✅ — accept the current draft, advance the flow.
	ReactionConfirm ReactionKind = "✅"
	// ReactionEdit: ✏️ — "let me change something" (v1: no-op; reserved).
	ReactionEdit ReactionKind = "✏️"
	// ReactionCancel: ❌ — abort the current gtw step; rollback side
	// effects where possible.
	ReactionCancel ReactionKind = "❌"
	// ReactionNewV2: 🆕 — branch already exists; create a -v2 variant.
	ReactionNewV2 ReactionKind = "🆕"
	// ReactionJoin: 🔗 — branch already exists; reuse the existing one
	// without creating a new worktree.
	ReactionJoin ReactionKind = "🔗"
	// ReactionForce: 🤝 — somebody else holds the label; force-take it.
	ReactionForce ReactionKind = "🤝"
	// ReactionRetry: 🔄 — last step failed; re-run.
	ReactionRetry ReactionKind = "🔄"
)

// ReactionEvent is the inbound reaction payload. Type alias to
// services.ReactionEvent (the canonical location). Old
// `chatsession.ReactionEvent` and the prior `gtw.ReactionEvent`
// struct both had identical fields (TargetMsgID / Emoji /
// UserID / ChatID), so callers compiled against either form
// can be migrated to this alias.
type ReactionEvent = services.ReactionEvent

// OutMsg is the gtw-package view of an outbound IM message. Kept
// tiny on purpose — the gateway layer wraps this into its own
// OutboundMessage / Kind taxonomy. Using a small struct here lets
// the gtw package stay decoupled from internal/gateway.
type OutMsg struct {
	ChatID string
	Text   string
	// ReplyTo is the channel-native userMsgID to thread under.
	// Empty for top-level cards. Mirrors gateway.OutboundMessage.ReplyTo.
	ReplyTo string

	// F-46: when PatchBotMsgID is set the gateway emits a PATCH
	// (OutboundKind.OutCardPatch) targeting the bot message at that
	// ID, replacing its body with a disabled version of the
	// original decision card. The fields below carry the rebuild
	// context: title + body + choices mirror the original card so
	// the PATCH keeps the same shape, plus a one-line result note
	// appended to the body via PatchResult.
	PatchBotMsgID     string
	PatchChosenEmoji  string
	PatchResult       string
	CardTitle         string
	CardBody          string
	CardChoices       []CardChoice
	CardRequestID     string
	// ChosenChoiceEmoji opts the matching button into the
	// "✅ 已<label>" inline state when the PATCH is rendered
	// (mirrors ChosenChoiceEmoji on gateway.Card). When empty,
	// every button is disabled with its original label.
	ChosenChoiceEmoji string
}

// SendFunc is the IM-side send callback. The ctx is the caller's
// request context (typically the one passed to RunFix / HandleAction
// from the slash-command dispatcher). Adapters use it for
// cancellation + rate limiting; tests can pass a closure that
// appends to a slice for assertions.
type SendFunc func(ctx context.Context, m OutMsg) error

// Card represents the original decision card stored on a draft.
// Carries enough information for the action handler to rebuild
// the card with Disabled=true and a result note (see
// executeXxxAction → deps.Send → PatchBotMsgID path).
type Card struct {
	Title     string
	Body      string
	Choices   []CardChoice
	RequestID string
}

// OutCardMsg is the gtw-package view of an outbound card send.
// Carries the card data and the chat + reply-target; the adapter
// translates to the channel's native card wire format and returns
// the bot-side message id assigned by the channel.
type OutCardMsg struct {
	ChatID  string
	ReplyTo string
	Card    Card
}

// SendCardFunc is the IM-side card send callback. Returns the
// bot-side message id assigned by the channel so the dispatcher
// can store it on the draft for later PATCH.
type SendCardFunc func(ctx context.Context, m OutCardMsg) (botMessageID string, err error)

// F-XX removed `gtw.Sender` interface; the gtw package now
// imports chatsession directly and uses *chatsession.ChatSession
// for ActiveCwd / SetActiveCwd / QueueUserMessage.
