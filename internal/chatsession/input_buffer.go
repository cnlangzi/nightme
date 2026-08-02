// Package chatsession — InputBuffer (commit 9).
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
//     Idle + OnTurnEnded.
//   - FlushHook is installed by the runtime: it looks up the
//     current active AgentSession and SendBlocks to it.
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
	// New messages are forwarded immediately via FlushHook.
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
	default:
		return "unknown"
	}
}

// ErrBufferFull is returned by InputBuffer.Add when the queue
// exceeds maxMsgs.
var ErrBufferFull = errors.New("chatsession: input buffer full")

// FlushHook is invoked when the buffer is flushed. userMsgIDs lets
// the caller correlate pending receipts (so each receipt can be
// transitioned to StateExecuting at flush time).
type FlushHook func(combined []agent.ContentBlock, userMsgIDs []string) error

// InputBuffer is the per-ChatSession in-memory queue of pending
// user messages.
//
// commit 9: lifecycle is unchanged from v1.1; only the owner type
// (ChatSession instead of Session) is different. The buffer
// itself does NOT know about agents / chatIds / AgentSessions —
// it is a pure state machine.
type InputBuffer struct {
	mu       sync.Mutex
	state    atomic.Int32
	messages []bufferEntry
	maxMsgs  int
	maxBytes int

	onFlush FlushHook
}

type bufferEntry struct {
	Blocks    []agent.ContentBlock
	UserMsgID string
}

// NewInputBuffer constructs an empty buffer. Defaults: 50 messages,
// 100 KiB.
func NewInputBuffer(onFlush FlushHook, maxMsgs, maxBytes int) *InputBuffer {
	if maxMsgs <= 0 {
		maxMsgs = 50
	}
	if maxBytes <= 0 {
		maxBytes = 100 * 1024
	}
	return &InputBuffer{
		maxMsgs:  maxMsgs,
		maxBytes: maxBytes,
		onFlush:  onFlush,
	}
}

// Add enqueues a structured user turn. Idle → flush immediately;
// Busy → queue.
func (b *InputBuffer) Add(blocks []agent.ContentBlock, userMsgID string) error {
	if len(blocks) == 0 {
		return nil
	}

	b.mu.Lock()

	if SessionState(b.state.Load()) == StateIdle {
		hook := b.onFlush
		b.mu.Unlock()

		if hook == nil {
			return nil
		}
		return hook(blocks, []string{userMsgID})
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
	if totalBytes+blockBytes(blocks) > b.maxBytes {
		b.mu.Unlock()
		return ErrBufferFull
	}

	b.messages = append(b.messages, bufferEntry{
		Blocks:    blocks,
		UserMsgID: userMsgID,
	})
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

// OnTurnEnded flushes the buffer (called by the bridge after a
// result event).
func (b *InputBuffer) OnTurnEnded() error {
	b.mu.Lock()

	if len(b.messages) == 0 {
		b.mu.Unlock()
		return nil
	}

	entries := b.messages
	b.messages = nil
	hook := b.onFlush
	b.mu.Unlock()

	var combined []agent.ContentBlock
	var userMsgIDs []string
	for _, e := range entries {
		combined = append(combined, e.Blocks...)
		userMsgIDs = append(userMsgIDs, e.UserMsgID)
	}

	if hook == nil {
		return nil
	}
	return hook(combined, userMsgIDs)
}

// Flush is the manual user-initiated flush (/flush command).
func (b *InputBuffer) Flush() error {
	return b.OnTurnEnded()
}

// Clear discards all buffered messages without sending. Returns
// the number cleared.
func (b *InputBuffer) Clear() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.messages)
	b.messages = nil
	return n
}

// Pending reports the current queue size.
func (b *InputBuffer) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.messages)
}

// SetFlushHook installs a new hook (e.g., runtime rebinding after
// agent switch).
func (b *InputBuffer) SetFlushHook(h FlushHook) {
	b.mu.Lock()
	b.onFlush = h
	b.mu.Unlock()
}