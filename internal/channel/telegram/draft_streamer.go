package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// draftIDCounter is a process-global monotonic source for draft_id
// values. draft_id only needs to be unique within the chat+thread
// scope from Telegram's perspective (server keys drafts by
// chat+thread+draft_id); using a global counter avoids any chance of
// accidental collision and produces stable, debuggable ids across
// the process.
var draftIDCounter atomic.Int32

// Constants mirror group_draft.go so DM and group share the same
// visual surface contract (latest 5 thinking + 5 tool slots per
// flush; 10s debounce; 50 cap on each stack).
const (
	dmThinkingStackCap   = 50
	dmToolsStackCap      = 50
	dmDraftSendMax       = 5 // per-stack, per flush
	dmDraftBatchInterval = 10 * time.Second
)

// draftStreamer accumulates OutThinking / OutToolStart / OutToolEnd
// events into a single animated rich draft via sendRichMessageDraft
// (Bot API 10.3+). Same buffer+flush design as groupDraftManager —
// two FIFO stacks, debounce timer, lock-disciplined flush — but the
// transport differs:
//
//   - DM:    sendRichMessageDraft (server-managed animated rich
//     draft, no message_id — server animates in place by
//     draft_id).
//   - group: sendRichMessage (cold-create) + editMessageText
//     (subsequent PATCH) — bot-owned message.
//
// See docs/channel/telegram.md §11.12.1 (DM rich draft) and
// §11.12.2 (group simulated DraftMessage) for the parallel contract.
//
// Lifecycle: one streamer per (chat_id, thread_id, user_msg_id) —
// per-turn scope. Allocated on first draft-eligible event; the
// streamer object lives until endProcess evicts it from the index
// (called from Send's OutResult branch and OnPromptEnded). Each
// turn gets a fresh draft_id; cross-turn reuse of the streamer
// object would conflate back-to-back user prompts and is explicitly
// not supported (see docs/channel/telegram.md §11.12.11.3 for the
// per-prompt isolation contract).
//
// Lock discipline (matches groupDraftManager):
//
//   - mu      protects the two stacks. Held briefly during append /
//     peek / pop. NEVER held during the wire call.
//   - flushMu serializes concurrent flushes (the API call + the
//     post-call buffer mutation). Held during the API call only.
//
// Holding mu through the network roundtrip was the source of the
// "agent stuck on streamDraftEvent" hang (issue observed at
// 2026-09-15 21:00, group side); the same fix applies here: release
// mu before the API call, serialize the API call under flushMu.
type draftStreamer struct {
	api apiClient
	log *slog.Logger

	chatID    int64
	threadID  int
	userMsgID int

	mu      sync.Mutex
	flushMu sync.Mutex

	// draftID is process-global monotonic; first flush allocates.
	// Server keys drafts by (chat, thread, draft_id), so a fresh
	// draftID each turn keeps the visual surfaces isolated.
	draftID int32

	// Two parallel FIFO stacks, each capped at 50.
	thinkingStack []richTurnEntry // cap 50
	toolsStack    []toolSlot      // cap 50 slots

	// 10s debounce timer (analogous to groupDraftBatchInterval).
	flushTimer *time.Timer

	// Captured for timer / flush callbacks.
	rawChatID string
	topicID   int
}

func newDraftStreamer(api apiClient, log *slog.Logger, chatID int64, threadID int, userMsgID int) *draftStreamer {
	return &draftStreamer{
		api:       api,
		log:       log,
		chatID:    chatID,
		threadID:  threadID,
		userMsgID: userMsgID,
		rawChatID: strconv.FormatInt(chatID, 10),
	}
}

// streamDraftEvent appends one event to the appropriate stack (with
// FIFO eviction at cap 50) and arms the 10s flush timer if no
// timer is already pending. The actual Telegram API call happens in
// flushLocked when the timer fires (or endProcess).
//
// Returns (true, nil) on success — caller treats it as consumed and
// does NOT fall through to the rich-turn path. Returns (false, nil)
// only when the kind is unsupported or the streamer state is
// invalid; the caller falls through in those cases. Errors from the
// wire call are swallowed inside flushLocked (buffer restored on
// failure); this method itself only returns errors that prevent
// buffering.
func (s *draftStreamer) streamDraftEvent(_ context.Context, segment string, kind messages.OutboundKind) (bool, error) {
	s.mu.Lock()
	switch kind {
	case messages.OutThinking:
		s.thinkingStack = append(s.thinkingStack, richTurnEntry{
			kind: "thinking",
			body: segment,
		})
		if len(s.thinkingStack) > dmThinkingStackCap {
			s.thinkingStack = s.thinkingStack[len(s.thinkingStack)-dmThinkingStackCap:]
		}
	case messages.OutToolStart:
		s.toolsStack = append(s.toolsStack, toolSlot{
			start: richTurnEntry{kind: "tool", body: segment},
		})
		if len(s.toolsStack) > dmToolsStackCap {
			s.toolsStack = s.toolsStack[len(s.toolsStack)-dmToolsStackCap:]
		}
	case messages.OutToolEnd:
		if len(s.toolsStack) == 0 {
			// Orphan End: treat as the implicit Start of a new slot.
			s.toolsStack = append(s.toolsStack, toolSlot{
				start: richTurnEntry{kind: "tool", body: segment},
			})
		} else {
			slot := &s.toolsStack[len(s.toolsStack)-1]
			slot.ends = append(slot.ends, richTurnEntry{kind: "tool", body: segment})
		}
		if len(s.toolsStack) > dmToolsStackCap {
			s.toolsStack = s.toolsStack[len(s.toolsStack)-dmToolsStackCap:]
		}
	default:
		s.mu.Unlock()
		return false, nil
	}
	thinkLen := len(s.thinkingStack)
	toolLen := len(s.toolsStack)
	s.mu.Unlock()

	s.log.Info("telegram: streamDraftEvent buffered",
		"chat_id", s.rawChatID,
		"thread_id", s.threadID,
		"user_msg_id", s.userMsgID,
		"kind", kind.String(),
		"thinking_stack_len", thinkLen,
		"tools_stack_len", toolLen,
	)

	s.startFlushTimerIfIdleLocked()
	return true, nil
}

// flushLocked sends the LATEST N events from each stack (N =
// dmDraftSendMax, or all if fewer) as a single rich_message via
// sendRichMessageDraft. Popped entries are removed from the stacks;
// on API failure they are restored (issue #391 contract mirrored
// from groupDraftManager).
//
// Lock discipline:
//
//  1. mu — peek + pop the to-send slice from each stack, release.
//  2. flushMu — serialize the API call.
//  3. On failure, mu — restore the popped slices.
//
// Step 1+2 ensures the buffer mutation happens under mu, the API
// call happens under flushMu (without mu), so a slow API call
// never blocks new streamDraftEvent calls.
func (s *draftStreamer) flushLocked(ctx context.Context, reason string) (bool, error) {
	s.mu.Lock()
	if len(s.thinkingStack) == 0 && len(s.toolsStack) == 0 {
		s.mu.Unlock()
		return true, nil
	}

	nThink := dmDraftSendMax
	if nThink > len(s.thinkingStack) {
		nThink = len(s.thinkingStack)
	}
	sendingThink := make([]richTurnEntry, nThink)
	copy(sendingThink, s.thinkingStack[len(s.thinkingStack)-nThink:])
	s.thinkingStack = s.thinkingStack[:len(s.thinkingStack)-nThink]

	nTools := dmDraftSendMax
	if nTools > len(s.toolsStack) {
		nTools = len(s.toolsStack)
	}
	sendingTools := make([]toolSlot, nTools)
	for i, slot := range s.toolsStack[len(s.toolsStack)-nTools:] {
		sendingTools[i] = slot
	}
	s.toolsStack = s.toolsStack[:len(s.toolsStack)-nTools]

	remainingThink := len(s.thinkingStack)
	remainingTools := len(s.toolsStack)
	s.mu.Unlock()

	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	blocks := buildBlocksFromStacks(sendingThink, sendingTools)
	blocksJSON, err := json.Marshal(map[string]any{"blocks": blocks})
	if err != nil {
		// Marshal failure: restore the slices and bail.
		s.mu.Lock()
		s.thinkingStack = append(sendingThink, s.thinkingStack...)
		s.toolsStack = append(sendingTools, s.toolsStack...)
		s.mu.Unlock()
		return true, err
	}

	if s.draftID == 0 {
		s.draftID = draftIDCounter.Add(1)
	}

	params := map[string]any{
		"chat_id":      s.rawChatID,
		"draft_id":     s.draftID,
		"rich_message": json.RawMessage(blocksJSON),
	}
	if s.topicID > 0 {
		params["message_thread_id"] = s.topicID
	}

	if err := s.api.call(ctx, "sendRichMessageDraft", params, nil); err != nil {
		s.log.Warn("telegram: DM DraftMessage flush failed; buffer restored",
			"chat_id", s.rawChatID,
			"thread_id", s.threadID,
			"user_msg_id", s.userMsgID,
			"draft_id", s.draftID,
			"sent_thinking", len(sendingThink),
			"sent_tools", len(sendingTools),
			"remaining_thinking", remainingThink,
			"remaining_tools", remainingTools,
			"reason", reason,
			"err", err,
		)
		// Restore the popped slices back to the buffers.
		s.mu.Lock()
		s.thinkingStack = append(sendingThink, s.thinkingStack...)
		s.toolsStack = append(sendingTools, s.toolsStack...)
		s.mu.Unlock()
		return true, nil
	}

	s.log.Info("telegram: DM DraftMessage flushed",
		"chat_id", s.rawChatID,
		"thread_id", s.threadID,
		"user_msg_id", s.userMsgID,
		"draft_id", s.draftID,
		"sent_thinking", len(sendingThink),
		"sent_tools", len(sendingTools),
		"remaining_thinking", remainingThink,
		"remaining_tools", remainingTools,
		"reason", reason,
	)
	if remainingThink == 0 && remainingTools == 0 {
		s.mu.Lock()
		if s.flushTimer != nil {
			s.flushTimer.Stop()
			s.flushTimer = nil
		}
		s.mu.Unlock()
	}
	return true, nil
}

// startFlushTimerIfIdleLocked arms the 10s debounce timer if no
// timer is already pending. Caller MUST NOT hold s.mu.
func (s *draftStreamer) startFlushTimerIfIdleLocked() {
	s.mu.Lock()
	if s.flushTimer != nil {
		s.mu.Unlock()
		return
	}
	s.flushTimer = time.AfterFunc(dmDraftBatchInterval, func() {
		s.mu.Lock()
		if len(s.thinkingStack) == 0 && len(s.toolsStack) == 0 {
			s.flushTimer = nil
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		// No mu held during the API call.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = s.flushLocked(ctx, "timer")
	})
	s.mu.Unlock()
}

// endProcess finalizes the DM draft for a turn: stops the flush
// timer, flushes any remaining buffered events (last flush before
// OutResult lands), and resets draftID so the next turn allocates a
// fresh one. Does NOT call deleteMessage — server disposes the
// draft automatically when a real message (OutResult) lands on the
// same chat.
//
// Called from:
//
//   - Send case OutResult (real message → server pushes the draft)
//   - OnPromptEnded (safety net for turns without OutResult)
func (s *draftStreamer) endProcess(ctx context.Context) {
	s.mu.Lock()
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	hasBuffer := len(s.thinkingStack) > 0 || len(s.toolsStack) > 0
	s.mu.Unlock()

	if hasBuffer {
		_, _ = s.flushLocked(ctx, "endProcess")
	}

	// Reset draftID so the next turn allocates a fresh surface.
	// The buffer is empty by endProcess return (either was already
	// empty or flushLocked drained + restored nothing on success).
	s.mu.Lock()
	s.draftID = 0
	s.mu.Unlock()
}
