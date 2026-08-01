// Package session — InputBuffer: in-memory queue of user messages
// arriving while Claude is busy. See docs/feat/F-25-input-buffer.md.
//
// Design decisions (locked in via the spec):
//
//   - In-memory only (no persistence). A nightme restart drops the
//     queue; the user notices via a startup message and re-sends.
//
//   - []agent.ContentBlock — preserves text + image/file
//     attachments across the busy → idle flush boundary. The
//     bridge layer maps message IDs to MessageReceipt instances
//     separately; the buffer itself just holds structured content.
//
//   - 3 states: IDLE / BUSY. Transitions come from the agent's
//     stream-json events (assistant.message → BUSY, result → IDLE).
//
//   - Flush on result event AND on user /flush command. /clear
//     discards without sending.
//
//   - No retry. If onFlush fails, the user re-sends.
package session

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/cnlangzi/nightme/internal/agent"
)

// SessionState is the busy/idle marker for the buffer's owner.
// One state machine lives per session — InputBuffer shares it with
// the agent bridge via SetState / State.
type SessionState int32

const (
	// StateIdle means the agent is not currently processing a turn.
	// New messages are forwarded immediately via onFlush.
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
// exceeds maxMsgs. The user-visible behavior is: the new message is
// dropped, the bridge layer's MessageReceipt emits ⚠️, and the user
// can /flush or /clear to make room.
var ErrBufferFull = errors.New("session: input buffer full")

// FlushHook is invoked when the buffer is flushed. userMsgIDs lets
// the caller correlate pending receipts (so each receipt can be
// transitioned to StateExecuting at flush time).
//
// Returns nil on success. Non-nil errors are logged but not retried —
// the user re-sends.
type FlushHook func(combined []agent.ContentBlock, userMsgIDs []string) error

// InputBuffer is the per-session in-memory queue of pending user
// messages.
//
// Lifecycle:
//
//	b := NewInputBuffer(FlushHook, 50, 102400)
//	b.SetState(StateBusy)                   // agent started a turn
//	b.Add(blocks, "msg_id_1")               // user message arrives
//	b.Add(blocks, "msg_id_2")
//	b.SetState(StateIdle)                   // result event arrives
//	b.OnTurnEnded()                         // flushes via hook
type InputBuffer struct {
	mu       sync.Mutex
	state    atomic.Int32 // SessionState (IDLE / BUSY)
	messages []bufferEntry
	maxMsgs  int
	maxBytes int

	onFlush FlushHook
}

// bufferEntry couples a structured user turn (text + attachments)
// with the originating userMsgID so FlushHook can correlate. We do
// NOT store any other metadata (no timestamp, no sender) — that
// lives in the channel layer's MessageReceipt.
type bufferEntry struct {
	Blocks    []agent.ContentBlock
	UserMsgID string
}

// NewInputBuffer constructs an empty buffer.
//
// maxMsgs caps the queue length; exceeding it returns ErrBufferFull
// on Add. maxBytes caps the cumulative content size (UTF-8 byte
// length). Both must be > 0; defaults of 50 / 100 KiB are
// reasonable starting points.
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

// Add enqueues a structured user turn. Behavior depends on current
// state:
//
//   - StateIdle: the blocks are sent immediately via onFlush. The
//     buffer stays empty. Returns nil on success or the hook's
//     error (the caller surfaces it to the user).
//
//   - StateBusy: the blocks are appended to the queue. Returns
//     ErrBufferFull if either limit is hit. The size estimate is
//     the cumulative UTF-8 byte length of every block's Text +
//     Path (MediaType is short and ignored); this is a coarse
//     approximation that over-estimates for image blocks (which
//     will be base64-inlined to a much larger wire form), but
//     that over-estimation just makes ErrBufferFull trip sooner
//     — a safe direction.
//
// Add is safe for concurrent callers (sync.Mutex).
func (b *InputBuffer) Add(blocks []agent.ContentBlock, userMsgID string) error {
	if len(blocks) == 0 {
		return nil
	}

	b.mu.Lock()

	if SessionState(b.state.Load()) == StateIdle {
		// Idle: bypass buffer, send directly.
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

// blockBytes returns an approximate byte size for a slice of
// ContentBlocks: sum of Text lengths + Path lengths. Used only
// for buffer-full accounting; the actual wire size after encoding
// (e.g. base64 expansion of images) is larger.
func blockBytes(blocks []agent.ContentBlock) int {
	n := 0
	for _, b := range blocks {
		n += len(b.Text) + len(b.Path)
	}
	return n
}

// SetState transitions the buffer's owner between IDLE and BUSY.
// The bridge calls this from its event pump:
//
//	assistant.message (first) -> SetState(StateBusy)
//	result                    -> SetState(StateIdle)
//
// SetState does NOT trigger a flush on its own — call OnTurnEnded
// after SetState(StateIdle) to flush. This two-step dance lets the
// bridge update receipts (StateExecuting) before the buffer drain
// happens, avoiding a visible "send then immediately mark executing"
// flicker.
func (b *InputBuffer) SetState(s SessionState) {
	b.state.Store(int32(s))
}

// State returns the current state.
func (b *InputBuffer) State() SessionState {
	return SessionState(b.state.Load())
}

// OnTurnEnded flushes the buffer. Called by the bridge after
// receiving a `result` event. Concatenates all buffered blocks
// (across all queued messages) into a single ordered slice and
// invokes onFlush once.
//
// OnTurnEnded is a no-op when the buffer is empty.
//
// Order matters: we drain the buffer BEFORE invoking onFlush so a
// failed flush doesn't trap messages in the queue. The user will
// re-send if the flush errored.
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

	// Concatenate all blocks across entries into a single ordered
	// slice — the bridge receives one structured user turn that
	// spans every queued Feishu message.
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

// Flush is the manual user-initiated flush (via /flush command).
// Behaves identically to OnTurnEnded — exposed as a separate method
// for log clarity (caller knows the trigger).
func (b *InputBuffer) Flush() error {
	return b.OnTurnEnded()
}

// Clear discards all buffered messages without sending. Returns
// the number of messages that were cleared (useful for /clear
// confirmation messages).
func (b *InputBuffer) Clear() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.messages)
	b.messages = nil
	return n
}

// Pending reports the current queue size. Useful for /status and
// for tests asserting on buffer state without draining.
func (b *InputBuffer) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.messages)
}