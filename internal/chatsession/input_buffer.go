// Package chatsession — InputBuffer (F-53 rewrite).
//
// Mirrors v1.1's session.InputBuffer (see
// internal/session/input_buffer.go) but lives in the chatsession
// package because its owner is now ChatSession, not Session. The
// v1.1 type is preserved for back-compat (any callers that still
// use session.MemoryManager); v1.2 code uses this one.
//
// v1.2 ownership semantics:
//
//   - One InputBuffer per ChatSession (not per AgentSession). The
//     FSM survives AgentSession switches (i.e., /use <other> does
//     NOT reset the buffer; queued messages still flush to the
//     new active AgentSession).
//   - State transitions are driven by the active AgentSession's
//     events: any non-terminal event → Busy; EventDone / Error →
//     Idle + flushPending.
//   - PromptHook is installed by the runtime (default:
//     ChatSession.defaultPromptHookLocked, which sends to the
//     active AgentSession).
//
// F-53 changes (Phase 0):
//
//   - `bufferEntry` (transient tuple) → `*Message` (first-class
//     domain object owned by ChatSession.messagesByID).
//   - `FlushHook(blocks, userMsgIDs)` → `PromptHook(p *Prompt)` —
//     hook receives a fully-built Prompt and is responsible for
//     the submission transaction (SendBlocks → flip Stage → install
//     on AgentSession.currentPrompt → wire emit).
//   - `OnTurnEnded()` → `flushPending()` (single drain-and-submit
//     path). `Flush()` (manual /flush) is a thin wrapper.
//   - `Clear()` now returns the cleared `[]*Message` so the
//     caller (ChatSession.BufferClear) can flip Stage=Dropped
//     and emit `MessageDropped` per message — explicit clear is
//     the ONLY path that produces `Message.Stage=Dropped` per
//     docs/feat/message_lifecycle.md §5.1.
package chatsession

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/cnlangzi/nightme/internal/agent"
)

// SessionState is the busy/idle marker for the buffer's owner
// (alias type; intentionally separate from the same-named type in
// the legacy session package).
type SessionState int32

const (
	// StateIdle means the agent is not currently processing a turn.
	// New messages are forwarded immediately via PromptHook.
	StateIdle SessionState = iota

	// StateBusy means the agent is in the middle of a turn. New
	// messages are queued in the buffer until the next result event
	// (or /flush).
	StateBusy
)

// String renders a SessionState for logs.
func (s SessionState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateBusy:
		return "busy"
	}
	return "unknown"
}

// ErrBufferFull is returned by InputBuffer.Add when the queue
// exceeds maxMsgs.
var ErrBufferFull = errors.New("chatsession: input buffer full")

// PromptHook is defined in prompt.go (re-exported here as an alias
// for clarity — `chatsession.PromptHook` reads naturally in
// signatures below). See prompt.go for the full contract.

// InputBuffer is the per-ChatSession in-memory queue of pending
// user messages (now `*Message` rather than transient tuples).
//
// F-53: the queue stores `*Message` references (the same objects
// that live in ChatSession.messagesByID). When the buffer drains,
// the hook receives a single `*Prompt` whose `MessageIDs` field
// lists all message IDs in queue order; `LastMessageID` is the
// last element of MessageIDs (the EventHandler anchor).
type InputBuffer struct {
	mu       sync.Mutex
	state    atomic.Int32
	messages []*Message
	maxMsgs  int
	maxBytes int

	onPrompt PromptHook
}

// NewInputBuffer constructs an empty buffer. Defaults: 50 messages,
// 100 KiB.
//
// `onPrompt` may be nil (test-friendly default); flushPending
// becomes a no-op in that case. Production always installs the
// ChatSession's `defaultPromptHookLocked`.
func NewInputBuffer(onPrompt PromptHook, maxMsgs, maxBytes int) *InputBuffer {
	if maxMsgs <= 0 {
		maxMsgs = 50
	}
	if maxBytes <= 0 {
		maxBytes = 100 * 1024
	}
	return &InputBuffer{
		maxMsgs:  maxMsgs,
		maxBytes: maxBytes,
		onPrompt: onPrompt,
	}
}

// Add enqueues a user message. Idle → flush immediately via the
// hook (the hook receives a Prompt wrapping this single message);
// Busy → append to the queue.
//
// The caller (ChatSession.QueueUserMessage) is responsible for
// having already stamped Stage=MessageQueued and added the message
// to ChatSession.messagesByID BEFORE calling Add — InputBuffer
// itself does not own the message's lifecycle.
func (b *InputBuffer) Add(msg *Message) error {
	if msg == nil || len(msg.Blocks) == 0 {
		return nil
	}

	b.mu.Lock()

	if SessionState(b.state.Load()) == StateIdle {
		hook := b.onPrompt
		b.mu.Unlock()

		if hook == nil {
			return nil
		}
		// Idle: build a single-message Prompt and hand to the
		// hook. Stage flipping + currentPrompt install happen
		// inside the hook (defaultPromptHookLocked).
		p := &Prompt{
			ID:             "", // assigned inside the hook (after SendBlocks success)
			ChatSessionID:  msg.ChatID,
			AgentSessionID: "", // filled by the hook from the active AS
			MessageIDs:     []string{msg.ID},
			LastMessageID:  msg.ID,
			Blocks:         msg.Blocks,
		}
		return hook(p)
	}

	// Busy: queue.
	if len(b.messages) >= b.maxMsgs {
		b.mu.Unlock()
		return ErrBufferFull
	}
	totalBytes := 0
	for _, m := range b.messages {
		totalBytes += blockBytes(m.Blocks)
	}
	if totalBytes+blockBytes(msg.Blocks) > b.maxBytes {
		b.mu.Unlock()
		return ErrBufferFull
	}

	b.messages = append(b.messages, msg)
	b.mu.Unlock()
	return nil
}

func blockBytes(blocks []agent.ContentBlock) int {
	n := 0
	for _, b := range blocks {
		n += len(b.Text) + len(b.Path)
	}
	return n
}

// SetState transitions the buffer's owner between IDLE and BUSY.
func (b *InputBuffer) SetState(s SessionState) {
	b.state.Store(int32(s))
}

// State returns the current state.
func (b *InputBuffer) State() SessionState {
	return SessionState(b.state.Load())
}

// flushPending (F-53, was `OnTurnEnded`) drains the queue and
// hands a single Prompt wrapping all queued messages to the hook.
// Returns nil if the queue is empty.
//
// `OnTurnEnded` is preserved as a thin alias for backward
// compatibility with existing callers / tests (it now means
// "flush whatever's pending"). Production should call
// `flushPending` directly.
//
// Note: the hook is invoked WITHOUT `b.mu` held — see the locking
// note in defaultPromptHookLocked for the rationale.
func (b *InputBuffer) flushPending() error {
	b.mu.Lock()

	if len(b.messages) == 0 {
		b.mu.Unlock()
		return nil
	}

	msgs := b.messages
	b.messages = nil
	hook := b.onPrompt
	b.mu.Unlock()

	// Build a single Prompt covering all drained messages.
	var combined []agent.ContentBlock
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		combined = append(combined, m.Blocks...)
		ids = append(ids, m.ID)
	}

	p := &Prompt{
		ID:             "", // assigned by hook after SendBlocks success
		ChatSessionID:  msgs[0].ChatID,
		AgentSessionID: "", // filled by hook from active AS
		MessageIDs:     ids,
		LastMessageID:  ids[len(ids)-1],
		Blocks:         combined,
	}

	if hook == nil {
		return nil
	}
	return hook(p)
}

// OnTurnEnded is the legacy alias for flushPending. Kept so
// existing tests and external callers don't break in Phase 0;
// renamed callers should prefer `flushPending`.
//
// F-53 follow-up: this alias can be removed once we confirm no
// external consumers exist (currently: 1 internal caller in
// runReadPump's EventDone/Error branches).
func (b *InputBuffer) OnTurnEnded() error {
	return b.flushPending()
}

// Flush is the manual user-initiated flush (/flush slash command).
func (b *InputBuffer) Flush() error {
	return b.flushPending()
}

// Clear discards all buffered messages and returns the cleared
// `[]*Message` so the caller (ChatSession.BufferClear) can flip
// Stage=Dropped and wire emit MessageDropped per message.
//
// Returns nil when the buffer was already empty.
//
// F-53: this is the ONLY path that produces Message.Stage=Dropped
// (see docs/feat/message_lifecycle.md §5.1). SendBlocks failure
// does NOT call Clear — failed messages stay Queued for the next
// flushPending to retry (see docs §3 原则 5).
func (b *InputBuffer) Clear() []*Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.messages) == 0 {
		return nil
	}
	cleared := b.messages
	b.messages = nil
	return cleared
}

// Pending reports the current queue size.
func (b *InputBuffer) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.messages)
}

// SetPromptHook installs (or replaces) the runtime-provided hook.
// Replaces the v1.3 SetFlushHook. nil clears.
func (b *InputBuffer) SetPromptHook(h PromptHook) {
	b.mu.Lock()
	b.onPrompt = h
	b.mu.Unlock()
}

// SetFlushHook is the legacy alias for SetPromptHook. Kept for
// back-compat with existing callers; F-53 follow-up can remove
// once all callers migrate.
func (b *InputBuffer) SetFlushHook(h PromptHook) {
	b.SetPromptHook(h)
}