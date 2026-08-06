package chatsession

import "time"

// GTWState enumerates the gtw lifecycle stages. Mirrored from
// internal/gtw; lives here (rather than in the gtw package) so
// ChatSession can hold *GTWContext without importing gtw, which
// would create a cycle: gtw → chatsession (for Sender) → gtw.
//
// v1 only writes "fixing" / "ready" / "canceled"; the rest are
// reserved for F-46+.
type GTWState string

const (
	GTWStateFixing   GTWState = "fixing"
	GTWStatePushing  GTWState = "pushing"
	GTWStateReady    GTWState = "ready"
	GTWStateCanceled GTWState = "canceled"
)

// GTWContext is the per-chat snapshot of the in-flight /gtw fix.
// nil when the chat has no active fix. All fields are set together
// under ChatSession.mu; external readers should treat the returned
// pointer as immutable (the gtw package replaces the pointer
// atomically, never mutates fields in place).
//
// F-45 §3.4.
type GTWContext struct {
	Issue     int
	Branch    string
	Worktree  string // absolute path; empty when the fix flow hasn't reached §5.2.④
	State     GTWState
	UpdatedAt time.Time
}

// GTWDraftKind tags a pending user-confirmation card. The set is
// closed; future flows (commit / pr) extend it.
type GTWDraftKind string

const (
	GTWDraftFixBranchExists GTWDraftKind = "fix.branch-exists" // §5.3.1
	GTWDraftFixLabelTaken   GTWDraftKind = "fix.label-taken"   // §5.3.2
	GTWDraftFixWorktreeFail GTWDraftKind = "fix.worktree-fail" // §5.3.3
)

// GTWFixDraftPayload is the typed payload for a GTWDraftFix* entry.
// F-45 §3.4 / §5.3. The fields are the rollback-relevant subset of
// the original /gtw fix context.
type GTWFixDraftPayload struct {
	IssueID  int
	Title    string
	Branch   string
	Slug     string
	Repo     string // "owner/repo" (single-slash form)
	Platform string // "github" | "gitlab"
	// GitError is the last 10 lines of stderr from the failed
	// `git worktree add` (only for GTWDraftFixWorktreeFail).
	GitError string
	// AlreadyClaimedBy is the user ID holding nightme/wip on
	// the issue (only for GTWDraftFixLabelTaken; v1 never emits).
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
type CardChoice struct {
	Emoji  string
	Label  string
	Action string
}

// GTWDraft is one pending user-confirmation card indexed by the
// bot reply's userMsgID. Reactions on that message id route to
// the matching draft via ChatSession.HandleAction.
type GTWDraft struct {
	Kind    GTWDraftKind
	Payload GTWFixDraftPayload
	// CreatedAt is currently unused by the routing logic but is
	// useful for diagnostics (e.g. "draft sat for 30s without
	// reaction → expunge").
	CreatedAt time.Time

	// F-46: bot-side message id of the rendered decision card.
	// Populated by the dispatcher after SendCard returns; consumed
	// by the action handler's follow-up PATCH (see
	// gtw.executeXxxAction). Empty when the dispatcher never sent
	// a card (e.g. legacy text-only fallbacks).
	BotMessageID string
	// F-46: original card render data so the action handler can
	// rebuild the card with `Disabled: true` and a result note
	// without going back to the dispatcher.
	CardTitle     string
	CardBody      string
	CardChoices   []CardChoice
	CardRequestID string
}
