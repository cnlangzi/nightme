package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
)

// errDraftFallback signals the adapter to skip the draft path and fall
// through to the v9 chain. Returned by draftStreamer.appendEvent when
// the sendMessageDraft API call fails (typically Bot API < 10.3, or
// the call is unsupported on the server side). After the first
// failure the streamer is latched for the current turn and all
// subsequent calls return this same error without touching the API.
var errDraftFallback = errors.New("telegram: sendMessageDraft unavailable, falling back to v9 chain")

// draftIDCounter is a process-global monotonic source for draft_id
// values. draft_id only needs to be unique within the chat+thread
// scope from Telegram's perspective (server keys drafts by
// chat+thread+draft_id); using a global counter avoids any chance of
// accidental collision and produces stable, debuggable ids across
// the process.
var draftIDCounter atomic.Int32

// draftStreamer accumulates OutThinking / OutToolStart / OutToolEnd
// events into a single animated draft via sendMessageDraft (Bot API
// 10.3+). Reusing the same draft_id across calls within a turn
// causes the client to animate the draft in place rather than
// replace it, giving a ChatGPT-style streaming view of agent
// activity.
//
// Lifecycle: one streamer per (chat_id, thread_id, user_msg_id) —
// per-turn scope. Allocated on first draft-eligible event; the
// streamer object lives until endProcess evicts it from the index
// (called from Send's OutResult branch and OnPromptEnded). Each
// turn gets a fresh draft_id; cross-turn reuse of the streamer
// object would conflate back-to-back user prompts and is explicitly
// not supported (see docs/channel/telegram.md §11.12.11.3 for the
// per-prompt isolation contract, mirrored from group_draft.go).
//
// Per-turn eviction matters because:
//   - draft_id is the server-side key into the draft surface; two
//     turns sharing draft_id would render as one animated draft.
//   - The failure latch is per-turn: a Bot API < 10.3 bot should
//     keep dropping events for the failing turn, but a later turn
//     (with a fresh streamer) should still be allowed to try.
//   - Memory: a process that runs many turns must not accumulate
//     dead streamers indefinitely.
//
// Scope: DM only (chat.type == "private"). Groups / forum topics
// never consult this streamer — they go straight to the v9 chain
// via group_draft.go. The Send() switch in adapter.go is
// responsible for the routing decision; this struct just streams
// when asked.
type draftStreamer struct {
	api apiClient
	log *slog.Logger

	chatID    int64
	threadID  int
	userMsgID int // turn anchor; the streamer belongs to exactly one user message

	mu      sync.Mutex
	draftID int32           // 0 = unallocated; allocated on first appendEvent
	textBuf strings.Builder // accumulated body sent to sendMessageDraft
	failed  bool            // sticky latch for the current turn
}

func newDraftStreamer(api apiClient, log *slog.Logger, chatID int64, threadID int, userMsgID int) *draftStreamer {
	return &draftStreamer{
		api:       api,
		log:       log,
		chatID:    chatID,
		threadID:  threadID,
		userMsgID: userMsgID,
	}
}

// appendEventWithThread is appendEvent with an explicit
// message_thread_id parameter (for forum topic routing). When
// topicID > 0 the API call carries message_thread_id so the draft
// is scoped to the correct forum topic (per Telegram API spec).
//
// When topicID == 0 (DM or non-forum group), behaves identically
// to appendEvent.
func (d *draftStreamer) appendEventWithThread(ctx context.Context, text string, replace bool, topicID int) error {
	return d.appendEventInternal(ctx, text, replace, topicID)
}

// appendEvent pushes text to the draft via sendMessageDraft. The
// replace flag controls how text is composed into the draft body:
//
//   - replace=true:  Reset textBuf first, then write text. Use for
//     events that display ALONE — one event per draft visual
//     surface. Each new event's body replaces the prior draft
//     body in full (OutThinking, OutToolStart).
//   - replace=false: Append text after a "\n\n" separator. Use
//     for events that STACK onto an existing draft body to form a
//     combined visual — OutToolEnd stacks onto its matching
//     OutToolStart to render the full "🔧 call / ✅ result" pair
//     as one draft body.
//
// User 2026-09-15 model: the draft visually shows ONE event at a
// time. replace=true events REPLACE the prior body (equivalent to
// "each event does its own reset"); replace=false events extend
// the prior body so a tool's start + end render as one composite
// display. Across turns no manual reset is needed: when a real
// message (OutResult / OutReply) lands, Telegram disposes the
// draft; the next turn's first event allocates a fresh draft
// naturally (same draft_id, but old body is gone). This is why a
// globally unique draft per (chat, thread) is sufficient.
//
// Returns errDraftFallback when the API call failed; the streamer
// is latched for the rest of the chat's turns until resetState
// (admin / test utility) is called.
func (d *draftStreamer) appendEvent(ctx context.Context, text string, replace bool) error {
	return d.appendEventInternal(ctx, text, replace, 0)
}

// appendEventInternal is the shared implementation; topicID > 0
// causes the API call to include message_thread_id so the draft
// is scoped to the correct forum topic.
func (d *draftStreamer) appendEventInternal(ctx context.Context, text string, replace bool, topicID int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.failed {
		return errDraftFallback
	}
	if d.draftID == 0 {
		d.draftID = draftIDCounter.Add(1)
	}
	if replace {
		d.textBuf.Reset()
	} else if d.textBuf.Len() > 0 {
		d.textBuf.WriteString("\n\n")
	}
	d.textBuf.WriteString(text)

	// No parse_mode: text is rendered as plain text. summarize_tool.go
	// produces plain emoji + text (no HTML tags), and OutThinking
	// text is raw LLM output where enabling parse_mode=HTML would
	// corrupt any literal "&" or "<" the model emits (Telegram
	// HTML mode requires escaping for those characters; "AT&T"
	// or "type <T>" would mangle). Plain-text rendering keeps
	// the draft text lossless regardless of LLM output.
	params := map[string]any{
		"chat_id":  d.chatID,
		"draft_id": d.draftID,
		"text":     d.textBuf.String(),
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
	}
	err := d.api.call(ctx, "sendMessageDraft", params, nil)
	if err != nil {
		d.failed = true
		d.log.Warn(
			"telegram: sendMessageDraft failed; OutTool/OutThink fall back to v9 chain for this turn",
			"chat_id", d.chatID,
			"thread_id", d.threadID,
			"err", err,
		)
		return errDraftFallback
	}
	return nil
}

// resetState clears the streamer state for a new turn. The streamer
// object stays in the index; subsequent events allocate a fresh
// draft_id and start a new animated draft. The failure latch is
// cleared so new turns retry the API (a transient failure in turn N
// shouldn't poison turn N+1).
func (d *draftStreamer) resetState() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.draftID = 0
	d.textBuf.Reset()
	d.failed = false
}

// resetProcess ends the current agent process: clears draftID and
// textBuf so the NEXT process (next turn's first event) starts
// with a fresh draft. Differs from resetState in that the failure
// latch is PRESERVED — a transient sendMessageDraft failure in this
// process shouldn't be wiped just because the process ended; the
// next process inherits the latch (so a Bot API < 10.3 bot keeps
// dropping events until daemon restart).
//
// Called from:
//   - Send case OutResult (real message sent → process ended)
//   - OnPromptEnded (safety net for turns without OutResult)
func (d *draftStreamer) resetProcess() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.draftID = 0
	d.textBuf.Reset()
	// failed intentionally preserved.
}
