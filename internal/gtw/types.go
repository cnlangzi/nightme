package gtw

import (
	"context"

	"github.com/cnlangzi/nightme/internal/chatsession"
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

// Re-exports of the chatsession types so the gtw package can
// present a self-contained API to its callers. The underlying
// definitions live in internal/chatsession/gtw_state.go.
type (
	// State is the gtw lifecycle stage cached on the platform's
	// label. See chatsession.GTWState for the canonical enum.
	State = chatsession.GTWState
	// Context is the per-chat /gtw snapshot. See chatsession.GTWContext.
	Context = chatsession.GTWContext
	// DraftKind is the typed identity of one pending user card.
	DraftKind = chatsession.GTWDraftKind
	// Draft is one pending user-confirmation card.
	Draft = chatsession.GTWDraft
	// FixDraftPayload is the typed payload of a Fix* draft.
	FixDraftPayload = chatsession.GTWFixDraftPayload
)

const (
	StateFixing   = chatsession.GTWStateFixing
	StatePushing  = chatsession.GTWStatePushing
	StateReady    = chatsession.GTWStateReady
	StateCanceled = chatsession.GTWStateCanceled
)

const (
	DraftFixBranchExists = chatsession.GTWDraftFixBranchExists
	DraftFixLabelTaken   = chatsession.GTWDraftFixLabelTaken
	DraftFixWorktreeFail = chatsession.GTWDraftFixWorktreeFail
)

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
}

// SendFunc is the IM-side send callback. The ctx is the caller's
// request context (typically the one passed to RunFix / HandleAction
// from the slash-command dispatcher). Adapters use it for
// cancellation + rate limiting; tests can pass a closure that
// appends to a slice for assertions.
type SendFunc func(ctx context.Context, m OutMsg) error

// ReactionEvent is the inbound reaction payload. Mirrors the F-45
// §3.2 design. Channels translate their native reaction-created
// event into this shape and publish it on the inbound stream.
type ReactionEvent struct {
	// TargetMsgID is the bot's message id that the user reacted to.
	// Used to look up the corresponding gtwDraft.
	TargetMsgID string
	// Emoji is the raw reaction emoji ("✅", "🆕", "🔗", "❌", "🔄", "🤝").
	Emoji string
	// UserID is the user who reacted. Used for "force-take" attribution
	// (§5.3.2: a comment in the commit trail is left by the user who
	// hits 🤝).
	UserID string
	// ChatID is the chat the reaction originated in. Required for
	// rendering follow-up replies; the gtwDraft payload does not
	// store it (F-45 §3.4 keeps the payload narrow).
	ChatID string
}
