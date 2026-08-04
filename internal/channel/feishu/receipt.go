// Package feishu — F-25 MessageReceipt with the v0.3 rolling-log
// format. Each user message gets one reply in chat that grows over
// the agent's lifetime:
//
//	⏳ 等待中
//
//	💭 I'll explore the workspace...
//	🔧 codegraph_explore(/repo/api)
//	✅ codegraph_explore done
//	💬 Here's the API handler I found at line 42:
//	    <reply text — may span multiple lines>
//
//	✅ 已完成 10:11:11
//
// All events (thinking, tool call, tool result, final reply) append
// to the log. If the log grows past replyMaxBytes, oldest entries
// are evicted from the front (FIFO) and a "…(前 N 条已省略)" prefix
// is shown. The receipt's reaction emoji (⏳ / 🔄 / ✅) is append-only
// per state transition — it shows the lifecycle trail, the log shows
// the work.
package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
)

// receiptBot is the minimal Feishu surface the receipt depends
// on. *Adapter satisfies this via its existing methods. Tests pass
// a mock that records the calls. Keeping the field typed as an
// interface (instead of *Adapter) makes the receipt unit-testable
// without spinning up a real lark client.
//
// SendCard + PatchMessage power the rolling-log card strategy
// (first-send-then-in-place-PATCH, see docs/channel/feishu.md §5).
// SendMessageText remains for the cold-start synthetic reply path
// (see Adapter.receiptFor) and as an escape hatch for tests.
//
// v1.3.x (§13.10): SendCard / SendMessageText now accept a rootID
// parameter (Feishu reply-in-thread). Receipt-cold-start passes the
// user message id here so the first card is rendered as a reply
// to the user's message; PATCH cycles preserve the thread.
type receiptBot interface {
	AddReaction(ctx context.Context, msgID, emoji string) (string, error)
	UpdateMessage(ctx context.Context, messageID, text string) error
	SendMessageText(ctx context.Context, chatID, text, rootID string, replyInThread bool) (string, error)
	// SendCard posts a new interactive card and returns its message ID.
	// Used on the FIRST render of a receipt (no cardMsgID yet).
	// v1.3.x (§13.10): rootID is the user message id to thread
	// the cold-start card to. F-37: replyInThread is forwarded but
	// always false on the cold-start path (the receipt card must
	// stay visible in the main chat as the pinned answer).
	SendCard(ctx context.Context, chatID, cardJSON, rootID string, replyInThread bool) (string, error)
	// PatchMessage replaces the body of an existing message in place
	// (Feishu PATCH /im/v1/messages/{id}). Used on every render after
	// the first. The message must already be an interactive card.
	PatchMessage(ctx context.Context, messageID, cardJSON string) error
}

// replyMaxBytes bounds the size of the rolling log message. Feishu's
// card body (and rich-text / text) request body is capped at 30 KB
// (Create / PATCH share the limit per the SDK's resource.go comment
// on Patch). We stay well under that — 24 KB leaves headroom for the
// envelope + future growth — so a single receipt is never rejected
// for size. Eviction kicks in once the rendered card body crosses
// this number, dropping oldest entries from the front.
//
// The legacy text-mode limit was 4 KB (3500 in this file). The card
// surface lifts the bar by ~6×; the rendered body also includes the
// per-element JSON wrapping, so the effective entry count is
// further constrained by replyMaxElements below.
const replyMaxBytes = 24 * 1024

// replyMaxElements caps the number of Feishu card body elements.
// Feishu's body.elements array is hard-limited to 50 elements
// (per the card 2.0 docs). The receipt layout reserves:
//
//	1 element  — header (state.headerLine)
//	1 element  — evicted marker (only when r.evicted > 0)
//	N elements — entries (≤ replyMaxEntries)
//	1 element  — <hr> divider
//	1 element  — foot note (state.footLine)
//
// → 2 + replyMaxEntries + 2 = 50 ⇒ replyMaxEntries ≤ 46. We pick
// 45 to leave one slot of slack against Feishu's 50-element limit
// (and against any future per-entry element growth).
const replyMaxElements = 50

// replyMaxEntries is the cap on entries kept in the rolling log.
// Derived from replyMaxElements (50) − reserved slots (header +
// evicted + hr + footer = 4, and the header slot is itself
// conditional on headerLine being non-empty, but we budget for the
// worst case).
const replyMaxEntries = 45

// perEntryMaxBytes bounds a single log entry's payload so a giant
// 💬 reply line cannot monopolise the budget.
//
// F-37 (multi-div content split): the unit is now characters
// (runes), not bytes — truncateForLog was changed to rune counting
// in F-37 (see receipt_event.go). The const name is retained for
// source-compatibility with thinking/tool/reply/error/compaction
// callers that previously assumed a 600 B cap. For CJK / emoji
// content the effective size is now larger (600 chars ≈ 1.8 KB for
// Chinese) and totalLogBytesLocked accounts for the multi-byte
// expansion. Callers that genuinely need a byte limit should use
// a separate helper.
const perEntryMaxBytes = 600

// divTextCharLimit caps the text length of a single `div` element
// in the receipt card body. This is the Feishu hard limit on
// `div.text` (per docs/feishu.md §10) — the splitter uses this
// as maxRunes so no div ever exceeds the server's accepted size.
//
// F-37 design: receipt entries may produce multiple `div` elements
// when split, but each individual div stays under this limit.
const divTextCharLimit = 1000

// perEntryMaxRunes caps a single log entry's payload in runes
// (not bytes) so a giant final reply can be preserved as multiple
// divs instead of being truncated at 600 bytes.
//
// F-37 (multi-div content split): the splitter emits multiple
// divs per entry when text exceeds divTextCharLimit. 8000 runes
// is chosen so that:
//
//   - 中文 8000 chars × 3 B/char ≈ 24 KB — still under the 30 KB
//     card body envelope (per SDK resource.go:1381)
//   - 英文 8000 chars × 1 B/char ≈ 8 KB — well under the envelope
//   - After maxRunes splitting at divTextCharLimit=1000 runes,
//     a single entry produces up to 8 divs (Chinese) or 8 divs
//     (English). Either way, well under the 50-element limit.
//
// Beyond 8000 runes, truncateForLog still kicks in (rare tail;
// defensive). perEntryMaxBytes is a rune cap applied to other
// event kinds (see its own comment for the unit history).
const perEntryMaxRunes = 8000

// logEvictedMarker is the marker prepended when FIFO eviction has
// trimmed entries from the front of the log.
const logEvictedMarker = "…(前 %d 条已省略)"

// LogEntry is one row of the rolling activity log. The renderer
// stores entries in FIFO order; when total bytes exceed
// replyMaxBytes, the oldest are dropped and the count surfaces via
// logEvictedMarker.
type LogEntry struct {
	// Time is when the event arrived. Used only for sorting /
	// debugging; the rendered message shows eventKind+text, not
	// per-entry timestamps (Feishu's ⏳ header carries the live
	// clock already).
	Time time.Time

	// Icon is the unicode emoji that prefixes the line in chat
	// ("💭", "🔧", "✅", "💬", "❌"). Together with Text it forms
	// one rendered line.
	Icon string

	// Text is the entry body, already truncated:
	//   - thinking / tool / reply / error / compaction: perEntryMaxBytes (600 B)
	//   - result (final reply): perEntryMaxRunes (8000 chars), F-37 multi-div
	//     then splits this across multiple card divs when it exceeds
	//     divTextCharLimit (1000 chars per div).
	Text string

	// Kind is one of "thinking" | "tool_start" | "tool_end" |
	// "reply" | "error". Used by tests; not rendered.
	Kind string
}

// MessageReceipt is the per-user-message rolling-log display (F-25
// spec §6, v0.3 update). One receipt owns ONE Feishu reply message
// (the replyMsgID) and ONE reaction emoji on the user message. The
// reply message is updated in place via im.message.update as the log
// grows.
//
// Two visual tracks update in sync:
//
//  1. The reply text — a multi-line rolling log (header + entries).
//     Always ONE message per user message; the message grows over
//     the agent's lifetime. When it exceeds replyMaxBytes, oldest
//     entries are dropped from the front.
//  2. A single reaction emoji on the user message — ⏳ / 🔄 / ✅.
//     Swapped on lifecycle transitions (F-25 dual-track).
//
// Visual result on the user message:
//
//	🔄 ← single reaction emoji, swapped on every state change
//	💭 I'll explore the workspace...
//	🔧 codegraph_explore(/repo/api)
//	✅ codegraph_explore done
//	💬 Here's what I found…  ← all appended to one reply message
//	✅ 已完成 10:11:11
type MessageReceipt struct {
	chatID     string
	userMsgID  string
	replyMsgID string
	bot        receiptBot
	logger     *slog.Logger

	mu          sync.Mutex
	state       ReceiptState
	completedAt time.Time

	// entries is the rolling-log buffer (FIFO). Append grows it;
	// renderLocked evicts from the front when either the byte budget
	// (replyMaxBytes) or the element budget (replyMaxElements) is
	// exceeded.
	entries []LogEntry

	// evicted tracks how many entries were dropped from the front
	// across the receipt's lifetime. Surfaced via logEvictedMarker.
	evicted int

	// tasks is the latest Claude task snapshot (F-38) for this
	// turn. The bridge always sends the full snapshot, so we
	// copy it verbatim on every event. The slice is rendered as
	// a single markdown element below the answer entries and
	// above the footer divider; it is not part of the rolling
	// LogEntry history and is not subject to entry eviction.
	tasks []agent.TaskItem

	// v1.3 (F-31): currentReaction removed. Reaction idempotency
	// tracking moved to Adapter-level messageStates map (per
	// userMsgID, not per receipt). MessageReceipt is now purely
	// about the card body; reaction lifecycle is owned by
	// MessageState FSM.

	// cardMsgID is the Feishu message id of the rolling-log card
	// once it has been created. Empty before the first render.
	// After the first SendCard it is set; subsequent renders
	// PatchMessage against this id rather than posting new
	// messages. See docs/channel/feishu.md §5.2 / §5.3 for the
	// first-send-then-PATCH strategy.
	//
	// Kept separate from replyMsgID (which now points at the same
	// card) so the receipt's "what is the surface" intent stays
	// explicit. If a future migration changes the surface type
	// (e.g. to a thread reply), replyMsgID's meaning changes too;
	// cardMsgID stays anchored to "the card we PATCH".
	cardMsgID string

	// Feishu limits message updates to roughly five per second.
	// Skip duplicate bodies and pace real PATCH requests.
	lastBody      string
	lastBodyPatch time.Time

	// --- v1.1 foot-note metadata ---
	//
	// Populated by Append on EventInit (agentName + workspace) and
	// EventUsage (input/output token counts). The buildReceiptCard
	// helper renders the foot note as
	//
	//   "Agent · <agentName> | cwd · <workspace> | tokens · <count>"
	//
	// Any segment whose source field is empty is omitted (no
	// "Agent · ·" double-dot). The whole foot note is omitted
	// when every segment is empty (e.g. before EventInit arrives).
	// See docs/channel/feishu.md §9.3 for the contract.

	agentName    string // from agent.EventInit.AgentName
	workspace    string // from agent.EventInit.Workspace (cwd)
	branch       string // from agent.EventInit.Branch (git branch)
	inputTokens  int    // accumulated from agent.EventUsage
	outputTokens int    // accumulated from agent.EventUsage
}

// ReceiptState is the lifecycle state of a MessageReceipt.
//
// StateWaiting / StateExecuting / StateCompleted predate the v1.1
// four-state cross-channel enum (channel.ReceiptState). They remain
// in use because the existing adapter-internal paths
// (SendUserMessage / MarkExecuting / SetExecuting / SetCompleted /
// Append) flow through them; the new v1.1 path
// (channel.CreateReceipt → channel.UpdateReceipt) translates the
// cross-channel ReceiptState into one of these via applyState
// (see bottom of this file).
type ReceiptState int

const (
	// StateWaiting means the message has been received and is in
	// the buffer or pending dispatch to Claude.
	StateWaiting ReceiptState = iota

	// StateExecuting means Claude is processing this message (either
	// directly or after a buffer flush).
	StateExecuting

	// StateCompleted means Claude's result event has been received.
	// The receipt is terminal and no further updates happen.
	StateCompleted

	// StateError means dispatch or processing failed (v1.1). The
	// receipt is terminal and the user should retry.
	StateError
)

// String renders the state as a short human label. Mostly useful for
// test diagnostics and structured logs.
func (s ReceiptState) String() string {
	switch s {
	case StateWaiting:
		return "waiting"
	case StateExecuting:
		return "executing"
	case StateCompleted:
		return "completed"
	case StateError:
		return "error"
	}
	return "unknown"
}

// headerLine returns the single-line header that sits at the top of
// the reply message. It carries the lifecycle state.
//
// Executing state intentionally returns "": the previous
// "🔄 ⏳ N · HH:MM:SS" line was driven by an auto-tick (Heartbeat)
// that we removed in #40, and the ticking clock was misleading
// once no events were flowing (it implied activity that did not
// exist). The card body itself — log entries / task list / footer
// — already conveys "we are working" once the first entry lands,
// and the ⏳ / ✅ emoji reactions on the user message (managed by
// MessageState FSM) carry the lifecycle markers.
func (s ReceiptState) headerLine(r *MessageReceipt) string {
	switch s {
	case StateWaiting:
		return "⏳ 等待中"
	case StateExecuting:
		return ""
	case StateCompleted:
		if r == nil || r.completedAt.IsZero() {
			return "✅ 已完成"
		}
		return fmt.Sprintf("✅ 已完成 %s",
			r.completedAt.Format("15:04:05"))
	}
	return ""
}

// footLine returns the labelled summary that sits at the bottom
// of the receipt card (after a <hr> divider). It is rendered by
// buildReceiptCard as a plain markdown element (no color tag —
// the user requested the default card-renderer color).
//
// Format — two-line layout. The first line groups the three
// "task-scoped" fields (Agent, GIT, Tokens) joined by " | ";
// the second line carries the workspace path alone so a long
// cwd doesn't push the metadata off the card.
//
//	Agent: <agentName> | GIT: <branch> | Tokens: <in>/<out>
//	Workspace: <workspace>
//
// Missing fields drop their segment from line 1 entirely; if
// only the workspace is present, the result is a single
// "Workspace: ..." line. If nothing is present, "" is
// returned and buildReceiptCard omits the <hr> + foot note.
//
// Labels are mixed-case (Agent / GIT / Tokens / Workspace) per
// the user's most recent explicit request. The TOKENS
// segment uses uppercase K/M for the input side and
// lowercase k/m for the output side (a small visual
// convention so users can scan "32K/101" and tell at a
// glance which side is input vs output). Each side is
// omitted when its count is zero, so a turn that has not
// yet reported usage renders as "TOKENS: 20K" (input only)
// or "TOKENS: 1k" (output only) rather than "TOKENS: 0/0"
// or "TOKENS: 20K/0".
//
// NOTE: the per-line labels use ": " (colon + space) per
// the user's explicit request — this is the standard "key:
// value" Markdown pattern. Feishu's lark_md renderer MAY
// interpret a line that *starts* with "key: value" as a
// definition list and hoist the value to the top of the body
// (OpenClaw issue #59360). The risk is contained to the foot
// note's own contents; the user accepted the trade-off for
// visual clarity. See the original PR discussion in
// docs/channel/feishu.md §9.3 for the trade-off analysis.
//
// Returns "" if the receipt is nil OR every source field is
// empty / zero. buildReceiptCard uses the empty return to omit
// the <hr> + footer section entirely (no divider when there's
// nothing to show).
func (s ReceiptState) footLine(r *MessageReceipt) string {
	if r == nil {
		return ""
	}
	// Line 1: task-scoped fields joined by " | ".
	line1 := []string{}
	if r.agentName != "" {
		line1 = append(line1, "Agent: "+r.agentName)
	}
	if r.branch != "" {
		line1 = append(line1, "GIT: "+r.branch)
	}
	if r.inputTokens > 0 || r.outputTokens > 0 {
		// Render "<inputK>/<outputk>" with each side
		// independently suppressed when zero.
		var left, right string
		if r.inputTokens > 0 {
			left = compactNumberLoud(r.inputTokens)
		}
		if r.outputTokens > 0 {
			right = compactNumber(r.outputTokens)
		}
		switch {
		case left != "" && right != "":
			line1 = append(line1, "Tokens: "+left+"/"+right)
		case left != "":
			line1 = append(line1, "Tokens: "+left)
		case right != "":
			line1 = append(line1, "Tokens: "+right)
		}
	}
	// Line 2: workspace (full path) alone.
	var line2 string
	if r.workspace != "" {
		line2 = "Workspace: " + r.workspace
	}
	// Compose: omit the <br/> separator when one side is
	// empty so a single-line render doesn't have a
	// dangling line break.
	joined1 := strings.Join(line1, " | ")
	switch {
	case joined1 != "" && line2 != "":
		return joined1 + "<br/>" + line2
	case joined1 != "":
		return joined1
	case line2 != "":
		return line2
	}
	return ""
}

// compactNumber formats a non-negative token count with a
// lowercase unit suffix. The decimal place is dropped when the
// result is a whole number (so 1,000 → "1k" not "1.0k", matching
// the example in the user's footer request "20K/1k"). Used by
// footLine for the output-tokens side of the
// "<inputK>/<outputk>" segment.
func compactNumber(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		// 1,000 → "1k", 1,200 → "1.2k", 9,999 → "10k".
		// n%1000==0 catches the exact thousands; n>=10000
		// (handled by the next branch) catches everything
		// else that rounds to a whole k.
		if n%1000 == 0 {
			return fmt.Sprintf("%dk", n/1000)
		}
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		if n%1_000_000 == 0 {
			return fmt.Sprintf("%dM", n/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// compactNumberLoud is the same formatter as compactNumber but
// with an uppercase K/M suffix. Used by footLine for the
// input-tokens side so the user can scan "20K/1k" and tell at a
// glance which side is input vs output.
func compactNumberLoud(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		if n%1000 == 0 {
			return fmt.Sprintf("%dK", n/1000)
		}
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dK", n/1000)
	default:
		if n%1_000_000 == 0 {
			return fmt.Sprintf("%dM", n/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// Emoji (deprecated in v1.3 F-31) was the feishu-side reaction
// emoji mapping for receipt-card lifecycle states. The function is
// removed: reaction handling moved to MessageState FSM (see
// mapStateToFeishuEmoji in adapter.go). Receipt FSM now only
// drives the card body, not reactions.
//
// The identifiers that used to live here (OneSecond / OnIt / DONE /
// THUMBSUP) match the v1.3 MessageState → emoji mapping verbatim,
// but the trigger states (Received / Forwarded / Done) are owned
// by ChatSession, not MessageReceipt.

// NewMessageReceipt creates a receipt and emits the initial state
// (Waiting). The reply text message and the ⏳ reaction are posted
// before NewMessageReceipt returns. The reply starts as just the
// header line; entries are appended as events arrive via Append.
func NewMessageReceipt(ctx context.Context, bot *Adapter, chatID, userMsgID string) (*MessageReceipt, error) {
	r := &MessageReceipt{
		chatID:    chatID,
		userMsgID: userMsgID,
		bot:       bot,
		logger:    slog.Default(),
		state:     StateWaiting,
	}

	// v1.3 (F-31): initial reaction is NO LONGER added here.
	// MessageState FSM owns user-message reaction lifecycle; receipt
	// only handles the reply message (card body). ChatSession.emitMessageState
	// will fire MessageState(Received) → Gateway.OnMessageState →
	// Adapter.Send → AddReaction("OneSecond") on userMsgID.
	//
	// Post the reply with just the header line. Subsequent events
	// Append entries and re-render this message in place. Capture
	// the returned message ID for edits.
	//
	// v1.3.x (§13.10): pass userMsgID as rootID so the cold-start
	// text reply is rendered as a reply to the user's message. The
	// receipt path that lives under Adapter.receiptFor uses SendCard
	// directly with the same threading; this code path (synthetic
	// text fallback) was the only place not yet threaded.
	//
	// F-37: replyInThread=false — the cold-start text bubble is the
	// pinned answer preview and must stay visible in main chat.
	msgID, err := bot.SendMessageText(ctx, chatID, r.state.headerLine(r), userMsgID, false)
	if err != nil {
		r.logger.Warn("feishu receipt: initial reply failed", "err", err)
		return r, fmt.Errorf("create receipt: %w", err)
	}
	r.mu.Lock()
	r.replyMsgID = msgID
	r.mu.Unlock()

	return r, nil
}

// NewMessageReceiptForReply wraps an already-posted reply message
// (the caller is responsible for sending the initial text and
// adding the ⏳ reaction). The returned receipt is registered in
// the adapter's indexes so subsequent Append calls update the
// same reply. Use this when the adapter owns the full lifecycle
// (Stage 3): the gateway's fallback calls adapter.SendUserMessage
// which posts the reply and constructs the receipt in one place.
//
// bot is the adapter (or any receiptBot implementation) used by
// renderLocked to ship reply text (SendCard on first render,
// PatchMessage on subsequent). Stage 3 callers pass `a`; passing
// nil will panic on the first Append, since renderLocked calls
// r.bot.SendCard / PatchMessage directly.
//
// v1.3 (F-31): renderLocked no longer calls AddReaction. Reaction
// lifecycle is owned by MessageState FSM and routed through
// Adapter.Send dispatcher (mapStateToFeishuEmoji).
// adapter's Send switch. See CHANGELOG v0.3 Stage 3 + the
// `recover renderLocked panic` follow-up.)
func NewMessageReceiptForReply(chatID, userMsgID, replyMsgID string, bot receiptBot) *MessageReceipt {
	return &MessageReceipt{
		chatID:     chatID,
		userMsgID:  userMsgID,
		replyMsgID: replyMsgID,
		bot:        bot,
		logger:     slog.Default(),
		state:      StateWaiting,
	}
}

// NewMessageReceiptForCard wraps an already-posted interactive card
// (the caller posted the cold-start card via SendCard and is
// passing back the message id). The returned receipt is seeded
// with cardMsgID = messageID so the FIRST Append skips the SendCard
// step and goes straight to PATCH in place. The text-mode
// NewMessageReceiptForReply constructor leaves cardMsgID empty and
// triggers a SendCard on first render — that path is the right
// choice when the gateway fell back to a text reply; this one is
// the right choice when the adapter's receiptFor posted a card
// directly (the recommended cold-start path, see
// docs/channel/feishu.md §5.2 + buildColdStartCard in adapter.go).
func NewMessageReceiptForCard(chatID, userMsgID, cardMessageID string, bot receiptBot) *MessageReceipt {
	return &MessageReceipt{
		chatID:     chatID,
		userMsgID:  userMsgID,
		replyMsgID: cardMessageID,
		cardMsgID:  cardMessageID,
		bot:        bot,
		logger:     slog.Default(),
		state:      StateWaiting,
	}
}

// SetExecuting transitions Waiting → Executing. First-call only;
// subsequent SetExecuting calls are idempotent no-ops. The card
// header in Executing state is empty (see headerLine) so no header
// rewrite is needed; renderLocked still fires so the reaction FSM
// can pick up the state change in any downstream consumers.
func (r *MessageReceipt) SetExecuting(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateCompleted {
		return nil
	}
	if r.state == StateExecuting {
		return nil
	}
	r.state = StateExecuting
	return r.renderLocked(ctx)
}

// SetTaskList replaces the per-turn task checklist (F-38) with a
// fresh snapshot from the bridge. The slice is copied so subsequent
// caller mutations to the underlying array cannot affect the
// receipt. Empty lists (len(Items)==0) are accepted and clear
// the checklist.
//
// Like Append, a SetTaskList after the receipt is completed is
// dropped silently. The first SetTaskList on a Waiting receipt
// promotes it to Executing.
func (r *MessageReceipt) SetTaskList(ctx context.Context, list *agent.TaskListEvent) error {
	if r == nil {
		return nil
	}
	if list == nil {
		return errors.New("feishu receipt: SetTaskList called with nil TaskListEvent")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateCompleted {
		return nil
	}
	items := list.Items
	if len(items) == 0 {
		r.tasks = nil
	} else {
		copyItems := make([]agent.TaskItem, len(items))
		copy(copyItems, items)
		r.tasks = copyItems
	}
	if r.state == StateWaiting {
		r.state = StateExecuting
	}
	return r.renderLocked(ctx)
}

// SetCompleted transitions Executing → Completed. The header flips to
// ✅ and the rolling-log streaming text is cleared so the card collapses
// to header + footer + task list — the answer text lives in the
// independent reply (F-39 reverse-section proof). No new entry appended.
//
// Called from EventDone / EventError in Append; NOT from OutResult (which
// was the F-39 mistake — early-terminal caused OutUsage / OutInit /
// TaskList to be silently dropped because Append's StateCompleted guard
// returned early).
func (r *MessageReceipt) SetCompleted(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateCompleted {
		return nil // idempotent
	}
	r.state = StateCompleted
	r.completedAt = time.Now()
	r.entries = nil // collapse rolling-log; only metadata (header/footer/task list) survives
	return r.renderLocked(ctx)
}

// State returns the current state. Useful for tests + diagnostics.
func (r *MessageReceipt) State() ReceiptState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// IsCompleted reports whether the receipt has reached a terminal
// state (Completed or Error). F-40: late-arriving OutReply events
// (after the agent turn ended) check this to decide between fold-
// into-receipt and bail out to a stand-alone reply via
// sendReplyAsMessage. See docs/feat/F-40-outreply-overflow.md §1.5.
func (r *MessageReceipt) IsCompleted() bool {
	return r.State() == StateCompleted || r.State() == StateError
}

// EntryCount returns the number of rolling-log entries currently
// held in the receipt. F-40: Adapter.Send(OutReply) calls this
// from outside the Append critical section to decide whether the
// receipt is full (entries >= replyMaxEntries) and the next reply
// should be diverted to a stand-alone reply instead of folded in.
// See docs/feat/F-40-outreply-overflow.md §1.2 isOverflowingReceipt.
func (r *MessageReceipt) EntryCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Append ingests one agent event and re-renders the reply message.
// All event kinds (thinking, tool_start, tool_end, text, done, error)
// flow through here — the renderer never calls SendMessage
// directly for these any more. Returns the rendered error (or nil)
// so callers can log per-event failures.
func (r *MessageReceipt) Append(ctx context.Context, ev agent.AgentEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == StateCompleted {
		// Late event after completion — drop silently. Tool
		// echoes (Claude Code's stream-json user-role events)
		// can arrive after the result event; they're noise from
		// the user's perspective.
		return nil
	}

	// Terminal events are handled first — they don't carry an
	// entry (the header itself signals completion), and we want
	// to short-circuit out before eventToEntry's "skip"
	// branches swallow them.
	switch ev.Kind {
	case agent.EventDone:
		// SetCompleted inlined: Append holds r.mu, calling SetCompleted
		// would self-deadlock (sync.Mutex is not reentrant).
		if r.state != StateCompleted {
			r.state = StateCompleted
			r.completedAt = time.Now()
			r.entries = nil // collapse rolling-log on terminal
			if err := r.renderLocked(ctx); err != nil {
				return err
			}
		}
		return nil
	case agent.EventError:
		entry, _ := eventToEntry(ev, time.Now())
		if entry.Text != "" || entry.Icon != "" {
			r.appendEntryLocked(entry)
		}
		r.state = StateCompleted
		r.completedAt = time.Now()
		return r.renderLocked(ctx)
	case agent.EventInit:
		// Stash agent identity + workspace + branch for the
		// foot note. Stamps happen even if Init arrives after
		// the first event (unusual — system/init normally
		// precedes any assistant output) so a later PATCH
		// picks up the foot note as soon as the metadata is
		// known.
		if ev.Init != nil {
			if ev.Init.AgentName != "" {
				r.agentName = ev.Init.AgentName
			}
			if ev.Init.Workspace != "" {
				r.workspace = ev.Init.Workspace
			}
			if ev.Init.Branch != "" {
				r.branch = ev.Init.Branch
			}
		}
		return r.renderLocked(ctx)
	case agent.EventUsage:
		// Accumulate token counts across turns so the foot note
		// reflects the running total rather than only the most
		// recent usage event. The foot note redraws on every
		// usage event so the user can see usage grow.
		if ev.Usage != nil {
			r.inputTokens += ev.Usage.InputTokens
			r.outputTokens += ev.Usage.OutputTokens
		}
		return r.renderLocked(ctx)
	}

	entry, ok := eventToEntry(ev, time.Now())
	if !ok {
		// F-34: eventToEntry returns (_, false) for kinds the
		// receipt card no longer carries (thinking / tool_start /
		// tool_end / compaction / permission). The adapter
		// routes those to Feishu thread replies. Nothing in
		// the card body needs to change, so no PATCH is
		// issued — the receipt card stays as it was and the
		// detail surfaces in the thread.
		_ = entry
		return nil
	}

	if entry.Text != "" || entry.Icon != "" {
		r.appendEntryLocked(entry)
	}

	// State transition: the first non-empty entry promotes the
	// receipt from Waiting → Executing.
	if r.state == StateWaiting && (entry.Text != "" || entry.Icon != "") {
		r.state = StateExecuting
	}

	return r.renderLocked(ctx)
}

// appendEntryLocked pushes entry onto r.entries and evicts the
// oldest from the front until both the byte budget (replyMaxBytes)
// and the entry budget (replyMaxEntries) are satisfied. The byte
// budget protects against Feishu's 30 KB card body cap; the entry
// budget protects against the 50-element hard limit (see the
// derivation on replyMaxEntries). Caller must hold r.mu.
// lastEntryLocked returns the most recently appended LogEntry,
// or nil when the buffer is empty. Used by eventToEntry's
// de-duplication pass: the final assistant text is emitted
// twice (streamed EventText + EventResult's text field), and
// skipping the duplicate keeps the rolling log clean. Caller
// MUST hold r.mu.
func (r *MessageReceipt) lastEntryLocked() *LogEntry {
	if len(r.entries) == 0 {
		return nil
	}
	return &r.entries[len(r.entries)-1]
}

func (r *MessageReceipt) appendEntryLocked(entry LogEntry) {
	if entry.Text == "" && entry.Icon == "" {
		return
	}
	r.entries = append(r.entries, entry)
	r.evictOverflowLocked()
}

// evictOverflowLocked drops oldest entries until the rendered body
// fits under BOTH budgets (whichever is tighter). Marks each drop in
// r.evicted so the rendered header can show "…(前 N 条已省略)".
//
// Caller must hold r.mu.
func (r *MessageReceipt) evictOverflowLocked() {
	for (totalLogBytesLocked(r) > replyMaxBytes || len(r.entries) > replyMaxEntries) && len(r.entries) > 1 {
		r.entries = r.entries[1:]
		r.evicted++
	}
}

// totalLogBytesLocked returns an approximation of the rendered
// card-body byte size (header + evicted marker + entries). The
// estimate is intentionally simple — it sums the user-visible
// text length (icon + text) per entry plus a per-element JSON
// wrapping overhead. The exact JSON depends on key ordering and
// the lark_md markdown grammar; this estimate is a conservative
// upper bound used only to decide when to evict, not to size the
// final payload. eviction triggers early enough that the actual
// PATCH always fits Feishu's 30 KB cap.
//
// F-37: chunk count is computed from rune count (the splitter's
// unit), not byte count, so CJK / emoji content is not
// over-counted. Thinking and tool entries wrap in a
// collapsible_panel element whose JSON adds ~250 B of header /
// border / icon keys on top of the inner markdown elements.
//
// Caller must hold r.mu.
func totalLogBytesLocked(r *MessageReceipt) int {
	const perElementOverhead = 96 // {"tag":"div","text":{"tag":"lark_md","content":""}} ≈ 50-100 bytes
	const perPanelOverhead = 250  // collapsible_panel header / border / icon / padding JSON
	total := 0
	if hl := r.state.headerLine(r); hl != "" {
		total += len(hl) + perElementOverhead
	}
	if r.evicted > 0 {
		total += len(fmt.Sprintf(logEvictedMarker, r.evicted)) + perElementOverhead
	}
	for _, e := range r.entries {
		// F-37: count runes (matches splitMarkdownForDivs's unit)
		// so Chinese content is not over-estimated.
		entryText := e.Icon + " " + e.Text
		entryRunes := utf8.RuneCountInString(entryText)
		chunks := 1
		if entryRunes > divTextCharLimit {
			chunks = (entryRunes + divTextCharLimit - 1) / divTextCharLimit
		}
		total += len(entryText) + chunks*perElementOverhead
		// Thinking / tool entries wrap in a collapsible_panel;
		// the panel JSON is independent of the inner content.
		if e.Kind == "thinking" || e.Kind == "tool" {
			total += perPanelOverhead
		}
	}
	// Foot note (when present) — <hr> + plain markdown.
	if note := r.state.footLine(r); note != "" {
		total += len(note) + 2*perElementOverhead
	}
	return total
}

// renderLocked pushes the current receipt state to Feishu. Caller
// must hold r.mu.
//
// Two side-effects fire on every render:
//
//  1. Reaction on the USER message (append-only). The receipt
//     keeps a single emoji per state on the user message and
//     never deletes — same-state renders skip a duplicate add.
//     See F-25 dual-track for the why.
//  2. The card body itself, via first-send-then-PATCH:
//     - If r.cardMsgID == "" → first render. Call SendCard to
//     create the interactive card; capture the message id.
//     - Else → call PatchMessage to replace the body of the
//     existing card in place. The whole card is replaced; the
//     server doesn't accept diffs. See docs/channel/feishu.md
//     §3.4 / §6.4.
//
// The card is built by buildReceiptCard (in adapter.go) which lays
// out: header div → optional evicted marker → entries → <hr> →
// optional foot note in <text_tag color='neutral'>. Empty entries
// (e.g. terminal state transitions with no log line) still trigger
// a render so the header timestamp refreshes.
//
// Failure modes:
//   - SendCard / PatchMessage failure: logged + returned to the
//     caller (which is Append, ApplyState, etc). The card surface
//     may be stale until the next render; we do NOT auto-create
//     a new card on PATCH failure (avoids duplicate surfaces).
//   - Reaction failure: logged only, never blocks the body send.
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
	// v1.3 (F-31): reaction handling moved out of MessageReceipt.
	// MessageState FSM owns user-message reaction lifecycle; this
	// receipt only renders the card body. ChatSession emits
	// MessageState events that flow through Gateway.OnMessageState
	// → Adapter.Send → AddReaction on userMsgID.

	// 1. Build the card body. buildReceiptCard is in adapter.go.
	body, err := buildReceiptCard(r)
	if err != nil {
		r.logger.Warn("feishu receipt: build card failed",
			"err", err, "state", r.state, "entries", len(r.entries))
		return fmt.Errorf("feishu receipt: build card: %w", err)
	}

	if r.cardMsgID == "" {
		// First send: create the card. Capture the message id
		// for subsequent PATCH calls.
		//
		// v1.3.x (§13.10): pass userMsgID as rootID so the cold-start
		// card is rendered as a reply to the user's message. Once
		// the card exists, PatchMessage preserves the thread across
		// subsequent in-place edits.
		//
		// F-37: replyInThread=false — the cold-start card IS the
		// main visible answer; thread-only would leave the main chat
		// empty until the receipt PATCHes happen.
		msgID, sendErr := r.bot.SendCard(ctx, r.chatID, body, r.userMsgID, false)
		if sendErr != nil {
			r.logger.Warn("feishu receipt: create card failed",
				"err", sendErr, "state", r.state, "entries", len(r.entries))
			return fmt.Errorf("feishu receipt: create card: %w", sendErr)
		}
		r.cardMsgID = msgID
		r.replyMsgID = msgID // keep alias in sync
		r.lastBody = body
		r.lastBodyPatch = time.Now()
		return nil
	}

	// Skip PATCHes when the body is identical to the previous render
	// (common when rapid events don't actually change the card) and
	// pace the real PATCHes below Feishu's message-update rate limit.
	if body == r.lastBody {
		return nil
	}
	const minPatchInterval = 300 * time.Millisecond
	if wait := minPatchInterval - time.Since(r.lastBodyPatch); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}

	// Subsequent: PATCH the existing card in place. The whole
	// body is replaced; the server doesn't accept diffs.
	if patchErr := r.bot.PatchMessage(ctx, r.cardMsgID, body); patchErr != nil {
		r.logger.Warn("feishu receipt: patch card failed",
			"err", patchErr, "state", r.state, "card_msg_id", r.cardMsgID,
			"entries", len(r.entries))
		return fmt.Errorf("feishu receipt: patch card: %w", patchErr)
	}
	r.lastBody = body
	r.lastBodyPatch = time.Now()
	return nil
}

// appendReactionLocked was removed in v1.3 (F-31). Reaction
// lifecycle is now owned by MessageState FSM, not receipt FSM.
// Adapter.Send handles OutMessageState events directly via
// mapStateToFeishuEmoji + AddReaction; see adapter.go §6.6.

// --- v1.1 unified state machine ---
//
