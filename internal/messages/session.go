package messages

import (
	"github.com/cnlangzi/nightme/internal/agent"
)

// SessionContext is the runtime-stamped AgentSession snapshot
// delivered alongside main-chat OutboundMessages for footer
// rendering. Populated by the runtime's newEventHandler closure
// (not by bridges) and read by Channel adapters to compose the
// footer line on each reply / result / task receipt.
//
// Field semantics:
//
//	Agent           — registry name of the agent that produced this
//	                  event (e.g. "claude", "codex"). Sourced from
//	                  AgentSession.Agent (immutable, no lock).
//	Model           — model the agent selected (e.g.
//	                  "claude-opus-4-5-20250929"). Sourced from
//	                  AgentSession.Model which the runtime caches
//	                  on first EventAgentReady. Empty before EventAgentReady
//	                  lands; footer omits the segment when "".
//	Usage           — per-turn snapshot from the bridge event that
//	                  produced this OutboundMessage (a pointer to
//	                  agent.UsageInfo, copied off out.Usage by
//	                  sessionContextInto). nil when the bridge
//	                  event did not carry usage (OutReply chunks
//	                  during streaming, etc.); the footer omits
//	                  Line 2 entirely in that case. The runtime
//	                  is a passive pass-through — it does NOT
//	                  aggregate across turns, so this snapshot is
//	                  always the single turn's bridge-reported
//	                  value, not a running total.
type SessionContext struct {
	Agent string
	Model string
	// SessionID is the agent's own session id captured on the
	// last run (e.g. Claude Code's `system/init.session_id`,
	// ACP's synthesized uuid when the bridge does not expose
	// one). Sourced from AgentSession.SessionID() (RLock). Empty
	// when the agent has no resume semantics or has not yet
	// emitted its init event; the footer omits the segment when
	// "".
	//
	// F-56 (F-45 follow-up): the first footer line
	// ("🤖: <agent> · <model> · <sessionid>") uses this as the
	// trailing identity segment. See docs/feat/F-45-session-footer.md
	// §1.10.
	SessionID string
	// Workspace is the absolute path of the AgentSession's
	// working directory at the time this OutboundMessage was
	// emitted. Sourced from AgentSession.Cwd (immutable post-
	// construction, no lock). Empty before the AgentSession has
	// been bound; the footer omits the workspace segment when "".
	//
	// F-48 (F-45 follow-up): the third footer line ("📁 code/nightme
	// · ⎇ main · …") uses this plus GitStatus to render the
	// git-tracking segment. See docs/feat/F-45-session-footer.md
	// §1.7.
	Workspace string
	// GitStatus is the per-stamp git status snapshot captured by
	// the runtime via gtw.CollectStatus. nil when the workspace
	// is not a git repo or the git invocation failed — the footer
	// omits the entire git segment in that case.
	//
	// Recomputed on every main-chat stamp (no caching) so the
	// footer reflects the latest worktree state without an
	// invalidation hook. See docs/feat/F-45-session-footer.md §1.7.
	GitStatus *GitStatusSnapshot

	// PullRequest is the open PR / MR associated with the
	// current head branch, resolved asynchronously by the
	// runtime via prcache.Registry. nil when:
	//
	//   - the workspace has no `origin` remote
	//   - the git platform cannot be detected (no URL hint
	//     match AND Stage B probe failed)
	//   - the most-recent platform call failed (auth,
	//     network, rate limit)
	//   - no open PR exists for the head branch
	//   - the cache has never been refreshed yet (first
	//     stamp, before the background goroutine has
	//     completed)
	//
	// The footer render path treats nil as "omit the trailing
	// `#N` PR segment on the workspace line" — PR lookup
	// failures and "no PR yet" look identical to the user,
	// which is the right trade-off for a chat-side decoration.
	// The only observable difference is at debug log level.
	//
	// Reads are synchronous (the cache is a struct field); the
	// underlying refresh goroutine does its own `gh pr list` /
	// `glab mr list` round-trip and writes back asynchronously.
	// Invalidation when /gtw pr creates a new PR happens via
	// prcache.Registry.Invalidate (which calls Cache.Invalidate
	// on the relevant AgentSession's cache), triggered by
	// dispatchPR after a successful CreatePR.
	PullRequest *PR

	// Usage is the per-turn snapshot from the bridge event that
	// produced this OutboundMessage — bridges populate it on
	// EventAgentResult / EventAgentDone. The runtime is a passive
	// pass-through; AgentSession does NOT aggregate across turns.
	// Channel footer reads Usage for the Line 2 segments (in / out
	// · X% · $cost). nil when the bridge event didn't carry
	// usage (e.g. OutReply chunks during streaming, which have no
	// usage field). See docs/feat/F-45-session-footer.md §1.6.
	//
	// ContextWindowPct on UsageInfo is the bridge-computed
	// per-turn context-fill percentage (0–100), via the Doc 1
	// formula. Channels read it verbatim as the "X%" segment;
	// 0 means "not reported" and the footer omits X% rather than
	// showing 0%. See docs/feat/F-45-session-footer.md §1.5.
	Usage *agent.UsageInfo
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

// Card is an interactive permission card or any other card that
// requires the user's choice.
//
// F-46: kind + choices + action encoding (see docs/feat/F-46-
// interactive-cards.md). The legacy Options field still works for
// callers that just want a flat list of button labels — build-
// InteractiveCard renders Options as primary buttons when Choices
// is empty.
type Card struct {
	// Title is the short headline (e.g., "Permission needed").
	Title string
	// Body is the question or instructions.
	Body string
	// Options enumerates the user-selectable choices. The first
	// option is the default / safe choice. The Gateway maps
	// the user's selection back via SendPermission(choice).
	Options []string
	// RequestID is the correlation token.
	RequestID string

	// F-46 fields.
	// Kind drives header decoration: CardKindPermission gets a
	// 🔐 prefix and the default blue template; CardKindDecision
	// renders the raw title with no prefix.
	Kind CardKind
	// Choices is the F-46 structured form of Options. Each choice
	// emits one button; the action string is encoded into the
	// button's `value` field with the F-46 {"action":..., "request_id":...}
	// envelope so handleCardAction can route it back into the
	// gtw pipeline via the act:/gtw/<scenario> prefix.
	Choices []CardChoice
	// Action is a single-button shortcut: when set, the card emits
	// one primary button with this action string (used for simple
	// "confirm" cards where Options/Choices would be overkill).
	Action string
	// Disabled disables every button on the card. PATCH-rendered
	// cards use this to grey out the original choices once the
	// user has picked one.
	Disabled bool
	// ChosenChoiceEmoji is the emoji of the button the user
	// picked. When set together with Disabled, the chosen button
	// is rendered with a "✅ 已<original-label>" label so the
	// user sees the click result inline in the card (the toast
	// position is controlled by Feishu and not always visible).
	ChosenChoiceEmoji string
	// HeaderColor overrides the default colour template
	// (blue / red / green / grey / etc.). Empty string = pick
	// from Kind.
	HeaderColor string
}

// CardKind tags the semantic shape of a Card. Drives header
// decoration and the 🔐 prefix policy.
type CardKind int

const (
	// CardKindPermission is the original permission card. Header
	// is prefixed with 🔐 and uses the blue template. v1 only
	// ships this kind for /gtw permission flows.
	CardKindPermission CardKind = iota
	// CardKindDecision is a gtw decision card (§5.3.1 / §5.3.3).
	// No 🔐 prefix; header is the title verbatim; buttons are
	// rendered as an equal-width column_set.
	CardKindDecision
	// CardKindPreview is /gtw test card — non-interactive preview
	// only; no buttons, no actions.
	CardKindPreview
)

// CardChoice is one button on a F-46 decision card. The action
// string follows the cc-connect convention: `act:/gtw/<scenario>`
// for action dispatch (handled in F-46 main work; for the
// prototype the action is encoded into the button value so a
// future handleCardAction can read it back).
type CardChoice struct {
	Emoji  string // optional leading emoji (e.g. "🆕"); rendered as part of the button text
	Label  string // visible button text
	Action string // value sent back via card.action.trigger (e.g. "act:/gtw/branch-newv2")
}

// MessageStatePayload is the OutboundMessage payload for
// OutMessageState / OutMessageStateRemoved kinds (F-31). It is
// the typed transport for the same data that v0.2 carried in
// Meta["message_id"] / ["state"] / ["reaction_id"]; channels
// read from this typed field directly. Replaces the legacy
// Reaction struct + implicit Meta keys (removed in §1.4
// cleanup).
type MessageStatePayload struct {
	// State is the abstract MessageState value (received /
	// forwarded / done / error).
	State agent.MessageState
	// MessageID is the channel-native id of the message being
	// reacted on (typically the user message that triggered the
	// assistant turn). Required for both OutMessageState (target
	// of AddReaction) and OutMessageStateRemoved (target of
	// DeleteReaction).
	MessageID string
	// ReactionID is the channel-native reaction id returned by a
	// prior AddReaction call. Required for OutMessageStateRemoved
	// so the channel can target the right reaction row (Feishu
	// has no UpdateReaction API). Empty for OutMessageState (the
	// reaction has not been created yet at that point).
	ReactionID string
	// Emoji is an optional channel-native emoji override. Most
	// channels ignore this and map State → emoji via their own
	// table (e.g. Feishu: StateReceived → "OneSecond").
	Emoji string
}

// UsageInfo is the typed payload for OutUsage and the
// SessionContext.CumulativeUsage field. See agent.UsageInfo for
// field semantics. Re-exported as a type alias here so existing
// gateway code (translate.go:158) keeps the same symbol name; the
// canonical definition lives in internal/agent (F-45 §2.1).
//
// (F-45): the comment block that used to live here was removed
// when the type moved to agent.UsageInfo. Old "InputTokens is the
// total input tokens ... (prompt + cache reads + tool input)" was
// misleading — InputTokens is the non-cached input count, NOT the
// sum with cache reads. Cache hits live in CacheReadInputTokens.
type UsageInfo = agent.UsageInfo