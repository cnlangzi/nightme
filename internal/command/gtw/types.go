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

// LabelMeta carries the colour and description applied when
// CreateLabel bootstraps a label that doesn't yet exist on the
// remote repo. Centralised here so AllLabels and the bootstrap
// metadata stay in lockstep — adding a new state label requires
// only one edit (the AllLabels slice plus a labelMeta entry).
//
// Colour is the 6-character hex WITHOUT a leading '#'. Description
// is English-only because the gtw command is /gtw; localisation
// would live in a separate message catalogue, not here.
type LabelMeta struct {
	Color       string
	Description string
}

var labelMeta = map[string]LabelMeta{
	LabelWIP:       {Color: "fbca04", Description: "Work in progress (claimed by /gtw fix)"},
	LabelReady:     {Color: "0e8a16", Description: "Ready for review"},
	LabelReviewing: {Color: "1d76db", Description: "Under review"},
	LabelRevise:    {Color: "e4e669", Description: "Needs revision"},
	LabelDone:      {Color: "5319e7", Description: "Completed"},
	LabelStuck:     {Color: "b60205", Description: "Stuck / blocked"},
}

// LabelMetaFor returns the colour / description registered for
// `name`. Falls back to a neutral grey + blank description when
// `name` is not a known gtw label (e.g. an external label passed
// through CreateLabel) so callers can use it as the single source
// of metadata without checking membership.
func LabelMetaFor(name string) LabelMeta {
	if m, ok := labelMeta[name]; ok {
		return m
	}
	return LabelMeta{Color: "ededed", Description: ""}
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

// IssueDispatchMode decides which prompt variant gtw hands to
// the agent. Plan = "analyze and present a plan, do not modify
// files". Execute = "implement the fix". gtw always dispatches
// exactly one prompt per /gtw fix; subsequent agent↔user
// confirmation flows through the chat, never back through gtw.
// See F-gtw-fix.md §4 for the rationale.
type IssueDispatchMode int

const (
	DispatchPlan IssueDispatchMode = iota
	DispatchExecute
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

// ReactionEvent is the inbound reaction payload. Type alias to
// services.ReactionEvent (the canonical location). Old
// `chatsession.ReactionEvent` and the prior `gtw.ReactionEvent`
// struct both had identical fields (TargetMsgID / Emoji /
// UserID / ChatID), so callers compiled against either form
// can be migrated to this alias.
//
// v1.5: gtw no longer consumes reaction events (the §5.3.3
// worktree-fail retry card was retired). The alias stays
// because the type still threads through `commandServices`
// infrastructure that nightme uses for non-gtw reaction
// flows.
type ReactionEvent = services.ReactionEvent

// F-XX removed `gtw.Sender` interface; the gtw package now
// imports chatsession directly and uses *chatsession.ChatSession
// for SelectedCwd / SetSelectedCwd / QueueUserMessage.
//
// v1.5 removed the gtw.Choice / gtw.ChoiceOption / gtw.Draft /
// gtw.DraftKind / gtw.FixDraftPayload types along with the
// worktree-fail retry card. The gtw package no longer emits
// interactive cards of its own; messages.Choice / messages.
// ChoiceOption (used by the channels package) are unrelated.
