// Package chatsession — Message (F-53).
//
// `Message` is the per-user-message domain object inside nightme.
// It replaces the old transient tuple `(blocks, userMsgID)` that
// existed only during a flush. Once a user message is accepted by
// the ChatSession it lives as a `*Message` on
// `ChatSession.messagesByID` for as long as the chat is alive (no
// persistence in v1.3.x — see docs/feat/message_lifecycle.md §8).
//
// Lifecycle:
//   - Construction: caller (newMessageDispatcher) fills ID / ChatID /
//     Blocks / ReceivedAt; Stage defaults to MessageQueued.
//   - Submitted:    `defaultPromptHookLocked` flips Stage and stamps
//     PromptID after `as.SendBlocks` returns nil.
//   - Dropped:      `ChatSession.MarkDropped` flips Stage on explicit
//     clear (/kill, /new, BufferClear). NOT on SendBlocks failure —
//     a failed submit leaves the message Queued so the next
//     `flushPending` retries it (see docs/feat/message_lifecycle.md
//     §3 原则 5).
//
// Concurrency: Stage / PromptID are mutated under `ChatSession.mu`.
// Callers that already hold `cs.mu` (e.g. inside defaultPromptHookLocked)
// may write directly; external callers must go through the ChatSession
// methods (MarkDropped, GetMessage) which take the lock.
package chatsession

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Message is one inbound user message accepted by a ChatSession.
//
// Immutable fields (post construction):
//   - ID, ChatID, Blocks, ReceivedAt.
//
// Mutable fields (guarded by owning ChatSession.mu):
//   - Stage, PromptID.
//
// Stage starts at agent.MessageQueued and ends at either
// agent.MessageSubmitted (in the same Prompt as one or more siblings)
// or agent.MessageDropped (explicit clear). It never goes back — see
// docs/feat/message_lifecycle.md §5.1.
//
// PromptID is empty while the message is Queued; populated when Stage
// flips to Submitted, identifying the Prompt this message was merged
// into. Remains set even after the Prompt ends (Stage doesn't change).
type Message struct {
	// ID is the channel-native message id (e.g. Feishu message_id),
	// or the dispatcher fallback `UserID:RFC3339Nano` when the
	// channel does not provide one. Acts as the primary key into
	// ChatSession.messagesByID.
	ID string

	// ChatID is the owning IM chat id (1:1 with this ChatSession).
	ChatID string

	// Blocks is the structured content the agent will receive
	// (text / image / file). Preserved verbatim from the inbound
	// Channel message; ChatSession does not mutate.
	Blocks []agent.ContentBlock

	// ReceivedAt is when the message entered nightme (set at
	// dispatcher entry, before any AS interaction).
	ReceivedAt time.Time

	// PromptID is set when Stage transitions to Submitted;
	// identifies which Prompt this message was merged into. Empty
	// while Queued, and for Dropped messages. Once set, immutable.
	PromptID string

	// Stage is the current Message.Stage value. Defaults to
	// MessageQueued on construction. See docs/feat/message_lifecycle.md
	// §4.1 for the state machine.
	Stage agent.MessageState
}