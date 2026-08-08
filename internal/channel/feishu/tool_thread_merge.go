// F-38 §3.1.3: tool-event thread-reply merge.
//
// Background. F-thread-route sends every OutToolStart and
// OutToolEnd as its own thread reply under the user message
// (● Tool(args), then ⎿  result). A hot agent that calls 10 tools
// in a turn therefore produces 20 thread replies — visually noisy
// and rate-limit-expensive (each Create hits the per-chat 5 QPS
// bucket independently).
//
// F-38 §3.1.3 changes the rendering contract: each tool pair is
// ONE thread reply, formed by PATCHing the start message with the
// start body + the result body concatenated. The user sees:
//
//	● Bash(go build ./... 2>&1)
//	⎿  💻 Bash → 3 lines
//
// as a single chat message under the receipt card.
//
// This file owns the in-flight tracking:
//
//   - pushToolStart records the freshly-posted OutToolStart's
//     message_id + start body, keyed by userMsgID (ReplyTo).
//     Stream-json's tool_use → tool_result pairing is strictly
//     ordered, so we use a FIFO (slice) per userMsgID — parallel
//     tool_use blocks in one message produce multiple entries
//     that the matching Ends drain in order.
//
//   - popToolStart returns the front entry (oldest unpaired
//     Start) and removes it from the buffer. On miss (orphan End,
//     or a turn whose Start was lost to GC), the caller falls
//     back to posting the result body as a fresh thread reply —
//     preserves pre-F-38 behavior for the unhappy path.
//
//   - clearToolEvents empties the buffer for a userMsgID. Called
//     when the turn ends (EventAgentDone / EventAgentError) so a new turn
//     that re-anchors to the same userMsgID (rare but possible
//     after a partial flush) starts with a clean slate. Also
//     called by Adapter.Stop to avoid leaks.
//
// Locking: a.mu guards the map. Same lock discipline as
// receiptsByUserMsgID / messageStates — concurrent writers are
// pumpOutbound goroutines, but Send is single-threaded per
// ChatSession (SPEC §1.3: ChatSession.EventCallback is the sole
// consumer of agentSession.Events()). The lock is held only for
// the slice/map op, not across any SDK call.
//
// See docs/SPEC.md §3.1.3 + docs/channel/feishu.md §13.x.
package feishu

import (
	"context"
	"errors"
)

// toolEventEntry is one in-flight OutToolStart. The matching
// OutToolEnd will PATCH this message_id with the merged body
// (start body + newline + result body).
type toolEventEntry struct {
	// startMsgID is the Feishu message_id returned by
	// SendMessageText when the start body was posted as a thread
	// reply. PATCH target for the merge.
	startMsgID string

	// startBody is the pre-formatted call line (`● Tool(args)`)
	// — we keep the exact string we posted so the merge appends
	// it byte-for-byte without re-running formatToolStartCall.
	startBody string
}

// pushToolStart records a freshly-posted OutToolStart so the
// matching OutToolEnd can PATCH the same reply.
//
// FIFO order is critical: stream-json guarantees tool_use blocks
// precede their matching tool_result blocks in the same order.
// Parallel tool_use blocks in one message are matched in
// declaration order; the FIFO ensures each End edits the correct
// Start's message even when several are in flight at once.
//
// userMsgID identifies the current turn (ReplyTo == userMsgID
// per SPEC §2.2). When pushToolStart observes a different
// userMsgID than what it has cached, it does NOT GC here — GC
// happens at turn end (clearToolEvents). This avoids prematurely
// discarding a Start whose End just hasn't arrived yet.
func (a *Adapter) pushToolStart(userMsgID, startMsgID, startBody string) {
	if startMsgID == "" {
		// Orphan: post returned no message_id (rootID was empty
		// → fell back to sendRawOutText). Nothing to PATCH.
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.toolEventBuf == nil {
		a.toolEventBuf = make(map[string][]toolEventEntry)
	}
	a.toolEventBuf[userMsgID] = append(a.toolEventBuf[userMsgID], toolEventEntry{
		startMsgID: startMsgID,
		startBody:  startBody,
	})
}

// popToolStart returns and removes the front entry (oldest
// unpaired Start) for userMsgID. Returns (_, false) when the
// buffer is empty for that userMsgID — caller should fall back
// to posting the result body as a fresh thread reply.
func (a *Adapter) popToolStart(userMsgID string) (toolEventEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.toolEventBuf == nil {
		return toolEventEntry{}, false
	}
	entries := a.toolEventBuf[userMsgID]
	if len(entries) == 0 {
		return toolEventEntry{}, false
	}
	front := entries[0]
	// Shift the slice (preserve remaining entries for any future
	// in-flight pairs in the same turn — parallel tool_use).
	if len(entries) == 1 {
		delete(a.toolEventBuf, userMsgID)
	} else {
		// Re-slice in place to avoid retaining the backing array
		// past the live window (entries[1:] would leak the
		// underlying array across many turns).
		next := make([]toolEventEntry, len(entries)-1, len(entries)-1)
		copy(next, entries[1:])
		a.toolEventBuf[userMsgID] = next
	}
	return front, true
}

// clearToolEvents empties the buffer for userMsgID. Called when
// a turn ends (EventAgentDone / EventAgentError) so any orphaned Starts
// (whose matching Ends never arrived — agent crashed mid-tool,
// stream truncated, etc.) don't leak across turns. Also called
// from Adapter.Stop to release all per-turn state.
func (a *Adapter) clearToolEvents(userMsgID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.toolEventBuf == nil {
		return
	}
	delete(a.toolEventBuf, userMsgID)
}

// clearAllToolEvents empties the entire buffer. Called from
// Adapter.Stop on daemon shutdown — no turn will ever complete,
// so releasing all pending state is correct.
func (a *Adapter) clearAllToolEvents() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.toolEventBuf = nil
}

// mergeToolReply is the F-38 §3.1.3 PATCH path: edit an existing
// tool thread reply with the merged body (start body + "\n" +
// result body). Wraps the mergeTextFunc call in F-36 transient
// retry (3 attempts, exponential backoff) so a transient blip
// doesn't drop the merged result; on retry exhaustion returns the
// last error so the caller can fall back to a fresh thread reply.
//
// Rate-limit note: Feishu's PATCH endpoint counts against the
// same per-chat 5 QPS bucket as Create. The threadReplyLimiter
// only guards the Create path (postThreadReply / postThreadMarkdownReply).
// PATCH calls from mergeToolReply are at most 1 per tool
// per turn — well below the 5/s limit even for a hot agent —
// so we intentionally do NOT add a separate limiter here. If a
// future workload ever exceeds the bucket, add a Wait() on
// a.threadReplyLimiter (same key) before the SDK call.
//
// The PATCH body is plain text (msg_type == "text"), matching
// the original post's msg_type — Feishu's update API requires
// the new msg_type to match the existing one (cannot switch
// text → card in-place).
func (a *Adapter) mergeToolReply(ctx context.Context, startMsgID, merged string) error {
	if startMsgID == "" {
		// Defensive: popToolStart already filtered empty
		// msgIDs (pushToolStart refused to record them), but a
		// future caller might pass "" through — surface a
		// clear error instead of an opaque SDK failure.
		return errors.New("feishu: merge tool reply: empty startMsgID")
	}
	a.mu.RLock()
	fn := a.mergeTextFunc
	a.mu.RUnlock()
	if fn == nil {
		return errors.New("feishu: merge tool reply: mergeTextFunc is nil")
	}
	return WithTransientRetry(ctx, RetryOpts{
		Op:     "merge_tool_reply",
		Cfg:    DefaultRetryConfig,
		Logger: a.logger,
		Attrs: []any{
			"start_msg_id", startMsgID,
		},
	}, func() error {
		return fn(ctx, startMsgID, merged)
	})
}

// mergeTextViaUpdate is the production implementation of the F-38
// tool-merge update dispatch (wired in NewAdapter as
// a.mergeTextFunc). Thin wrapper around UpdateMessage so the
// merge flow can mock the network call via mergeTextFunc without
// instantiating a larkClient.
func (a *Adapter) mergeTextViaUpdate(ctx context.Context, messageID, merged string) error {
	return a.UpdateMessage(ctx, messageID, merged)
}
