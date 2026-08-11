package messages

import (
	"github.com/cnlangzi/nightme/internal/agent"
)

// StatusBar is the runtime-stamped metadata envelope attached to
// every outbound message that flows from the runtime to a
// Channel. It groups the per-message decoration data into three
// sub-bars; the runtime populates each one based on what's
// available for the current chat, and the Channel decides how
// to render each sub-bar (Feishu renders a multi-line footer;
// other Channels may render inline annotations, a sidebar tag,
// or nothing at all — they read the same typed payload).
//
// Sub-bar semantics:
//
//	GitBar   — workspace + git + PR context. ALWAYS populated
//	           when the chat has a selected workspace
//	           (cs.SelectedCwd != ""), even if no AgentSession
//	           is selected. This is the "every message carries
//	           git context" rule: a chat user should always
//	           see what worktree they're talking about, even
//	           for /gtw replies or pre-spawn placeholders.
//	           The runtime collects git status fresh on every
//	           stamp (no caching) so the Channel sees the
//	           latest worktree state. PullRequest is nil when
//	           no AS is selected (PR lookup is per-AS).
//
//	AgentBar — agent identity (Agent / Model / SessionID).
//	           Populated only when a chat has a selected
//	           AgentSession — without an AS there is no
//	           identity to surface. Channels render Line 1
//	           of the footer when AgentBar != nil.
//
//	UsageBar — per-turn usage snapshot (in/out tokens,
//	           context window %, cost). Populated from
//	           out.Usage when the bridge event carries it
//	           (typically on OutResult / EventAgentResult).
//	           nil for streaming OutReply chunks without
//	           usage. Channels render Line 2 when UsageBar
//	           != nil.
//
// The runtime is the single owner of StatusBar — bridges never
// populate this directly. Pre-rename this was called
// SessionContext and carried flat fields (Agent, Model,
// Workspace, GitStatus, PullRequest, Usage). Renamed because
// "SessionContext" was semantically inaccurate — it is not
// only about the AgentSession (Workspace / GitStatus are
// workspace fields, Usage is a per-turn field) — and because
// the field-by-field naming collided with Go's
// context.Context idiom. See docs/feat/F-45-session-footer.md
// §1.3 for the pre-rename architecture rationale.
type StatusBar struct {
	// GitBar carries workspace + git + PR context. Always
	// populated when the chat has a selected workspace. nil
	// only when the chat has neither an AgentSession nor a
	// SelectedCwd — i.e. the chat is unusable for any work.
	GitBar *GitStatusBar
	// AgentBar carries agent identity (Agent / Model /
	// SessionID). nil when no AgentSession is selected — the
	// "AgentBar / TokensBar 没有 AgentSession 则忽略" rule:
	// without an AS there's no agent identity to surface.
	AgentBar *AgentStatusBar
	// UsageBar carries per-turn usage snapshot. nil when the
	// bridge event did not carry usage (OutReply chunks
	// during streaming, etc.); the renderer omits Line 2
	// entirely in that case.
	UsageBar *UsageStatusBar
}

// GitStatusBar is the workspace / git / PR sub-bar of StatusBar.
// Always populated when the chat has a workspace (cs.SelectedCwd
// != ""), even without an AgentSession — the "git status is
// always there" rule.
//
// Field semantics:
//
//	Workspace   — absolute path of the AgentSession's working
//	              directory at stamp time. Sourced from
//	              AgentSession.Cwd when an AS exists
//	              (immutable post-construction, no lock); falls
//	              back to cs.SelectedCwd() when no AS is
//	              selected — same string in normal operation.
//	              Empty before any of those is set; the
//	              renderer omits the workspace segment when
//	              "".
//
//	GitStatus   — per-stamp git status snapshot captured via
//	              gtw.CollectReadiness with a 3s deadline
//	              (review fix: a hung git would otherwise
//	              block the entire outbound-message pipeline).
//	              nil when the workspace is not a git repo or
//	              git invocation failed/timed out — Channels
//	              that render the git line (Line 3 of the
//	              Feishu footer) omit the entire line in that
//	              case. Recomputed on every main-chat stamp
//	              (no caching) so the Channel sees the latest
//	              worktree state. See
//	              docs/feat/F-45-session-footer.md §1.7.
//
//	PullRequest — open PR/MR associated with the head branch,
//	              resolved via prcache.Registry per-AS. nil
//	              when: no AS is selected (PR lookup is
//	              tied to the AS's cache); the workspace has
//	              no `origin` remote; the git platform cannot
//	              be detected; the most-recent platform call
//	              failed; no open PR exists; or the cache has
//	              never been refreshed. The renderer treats
//	              nil as "omit the trailing `#N` PR segment
//	              on the workspace line".
type GitStatusBar struct {
	Workspace   string
	GitStatus   *GitStatusSnapshot
	PullRequest *PR
}

// AgentStatusBar is the agent-identity sub-bar of StatusBar.
// Populated only when a chat has a selected AgentSession —
// without an AS there is no agent identity to surface.
//
// Field semantics:
//
//	Agent     — registry name of the agent that produced this
//	            event (e.g. "claude", "codex"). Sourced from
//	            AgentSession.Agent (immutable, no lock).
//	            Empty before the AS is bound; the renderer
//	            omits the segment when "".
//
//	Model     — model the agent selected (e.g.
//	            "claude-opus-4-5-20250929"). Sourced from
//	            AgentSession.Model which the runtime caches
//	            on first EventAgentReady. Empty before
//	            EventAgentReady lands; renderer omits when "".
//
//	SessionID — the agent's own session id captured on the
//	            last run (Claude Code's `system/init.session_id`,
//	            ACP's synthesized uuid, codex's thread.id).
//	            Sourced from AgentSession.SessionID() (RLock).
//	            Empty when the agent has no resume semantics
//	            or has not yet emitted its init event;
//	            renderer omits when "".
//
// F-56 (preserved): the first footer line
// ("🤖: <agent> · <model> · <sessionid>") uses these as
// trailing identity segments. Each segment is omitted
// independently when empty; an AgentBar with only SessionID
// set renders as "🤖: · <sid>".
type AgentStatusBar struct {
	Agent     string
	Model     string
	SessionID string
}

// UsageStatusBar is the per-turn usage sub-bar of StatusBar.
// Populated from the bridge event's Usage payload (typically
// co-located on OutResult / EventAgentResult). nil for
// streaming OutReply chunks without usage.
//
// *UsageInfo is embedded so callers access fields directly
// without an extra indirection:
//
//	sb.UsageBar.InputTokens
//	sb.UsageBar.ContextWindowPct
//	sb.UsageBar.CostUSD
//
// The runtime is a passive pass-through — it does NOT
// aggregate Usage across turns; this snapshot is always the
// single turn's bridge-reported value. See
// docs/feat/F-45-session-footer.md §1.6.
type UsageStatusBar struct {
	*UsageInfo
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
// StatusBar.CumulativeUsage field. See agent.UsageInfo for
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