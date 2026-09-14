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
// 10.3+). Reusing the same draft_id across calls causes the client
// to animate the draft in place rather than replace it, giving a
// ChatGPT-style streaming view of agent activity.
//
// Lifecycle: one streamer per (chat_id, thread_id), GLOBAL across
// turns. Allocated on first draft-eligible event; the streamer
// object stays alive in the index for the daemon's lifetime.
// draft_id is allocated once and reused across turns — when a
// turn ends with a real message, the server pushes the draft out
// and a fresh event with the same draft_id naturally creates a
// new draft. We deliberately do NOT reset state per turn: a
// global draft gives simpler semantics (one draft per chat, no
// turn-boundary races between late events and the next turn's
// reset).
//
// Scope: DM only (chat.type == "private"). Groups / forum topics
// never consult this streamer — they go straight to the v9 chain.
// The Send() switch in adapter.go is responsible for the routing
// decision; this struct just streams when asked.
type draftStreamer struct {
	api apiClient
	log *slog.Logger

	chatID   int64
	threadID int

	mu      sync.Mutex
	draftID int32           // 0 = unallocated; allocated on first appendEvent
	textBuf strings.Builder // accumulated body sent to sendMessageDraft
	failed  bool            // sticky latch for the current turn
}

func newDraftStreamer(api apiClient, log *slog.Logger, chatID int64, threadID int) *draftStreamer {
	return &draftStreamer{
		api:      api,
		log:      log,
		chatID:   chatID,
		threadID: threadID,
	}
}

// appendEvent REPLACES the draft text with this event and pushes
// the new body to Telegram via sendMessageDraft. Same draft_id
// across calls → client animates from prev text to new text in
// place ("Changes to drafts with the same identifier are
// animated"). REPLACE-not-append mirrors ChatGPT-style UX
// where only the latest step is visible.
//
// Returns errDraftFallback when the API call failed; the streamer
// is latched for the rest of the turn. Callers should treat this as
// "drop the event and all future events of this turn". The latch
// is cleared by resetState at the next turn boundary, so the next
// turn gets a fresh chance to use drafts.
func (d *draftStreamer) appendEvent(ctx context.Context, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.failed {
		return errDraftFallback
	}
	if d.draftID == 0 {
		d.draftID = draftIDCounter.Add(1)
	}
	// REPLACE semantics: each event replaces the previous draft
	// content. The client animates from prev text → new text in
	// place (same draft_id → "Changes to drafts with the same
	// identifier are animated"), giving a ChatGPT-style "only
	// the latest step is visible" UX. We do not accumulate a
	// history because the user wants the draft to mirror the
	// agent's current activity, not a log of past steps.
	d.textBuf.Reset()
	d.textBuf.WriteString(text)

	// No parse_mode: text is rendered as plain text. summarize_tool.go
	// produces plain emoji + text (no HTML tags), and OutThinking
	// text is raw LLM output where enabling parse_mode=HTML would
	// corrupt any literal "&" or "<" the model emits (Telegram
	// HTML mode requires escaping for those characters; "AT&T"
	// or "type <T>" would mangle). Plain-text rendering keeps
	// the draft text lossless regardless of LLM output.
	err := d.api.call(ctx, "sendMessageDraft", map[string]any{
		"chat_id":  d.chatID,
		"draft_id": d.draftID,
		"text":     d.textBuf.String(),
	}, nil)
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
