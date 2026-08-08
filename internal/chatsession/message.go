// Package chatsession — Message.
//
// `Message` is the per-user-message domain object inside nightme.
// It is the value carried by the chat-session input queue
// (MessageQueue) into a Prompt and through AgentSession.Submit.
//
// # Immutability
//
// Message has NO mutable fields. All lifecycle state
// (Queued / Submitted / Dropped) and per-message bookkeeping
// (which Prompt it joined, when that Prompt ended, the end
// reason) lives outside the value — on the queue / Prompt /
// wire event stream — never on the message itself. Two callers
// that hold copies of the same Message observe no shared
// mutations; this is by design, and is what makes the value
// type safe to copy around the pipeline.
//
// The one knob that the dispatcher DOES set at construction
// is `Kind` (Normal vs Queue barrier); it is read by the queue
// during Peek and never changed thereafter.
//
// # Concurrency
//
// Safe to copy across goroutines by value. No locking required
// for Message access. The queue that owns Messages, and the
// ChatSession that owns the queue, take their own locks for
// list mutation; the values inside are immutable from the
// queue's perspective.
package chatsession

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// MessageKind tags how a Message participates in queue segmentation
// (see MessageQueue). The default zero value is MessageKindNormal,
// which is what `newMessageDispatcher` constructs for ordinary user
// input. MessageKindQueue is set by the dispatcher (or other
// caller) when a message must stand alone as its own Prompt
// batch — typical use cases are scheduled / cron deliveries and
// command messages that explicitly opt out of batching.
//
// Kind is read by MessageQueue during Peek to decide where one
// batch ends and the next begins. It is never mutated after
// construction; if you need to change a message's kind, Remove
// it from the queue and re-Push with the new Kind.
type MessageKind int

const (
	// MessageKindNormal: default. Can be merged with other Normal
	// messages into a single Prompt batch. The MessageQueue Peek
	// will collect consecutive Normal messages into one batch
	// until it hits a MessageKindQueue or the tail.
	MessageKindNormal MessageKind = iota

	// MessageKindQueue: barrier. Forms its own single-element
	// batch when at the head of the queue, and terminates any
	// preceding Normal run. The message is its own discrete
	// execution unit — never merged with its neighbours.
	//
	// Use for: scheduled / cron deliveries, explicit "execute
	// this now" commands, any caller that needs to guarantee
	// the message is delivered in a standalone Prompt.
	MessageKindQueue
)

// String renders MessageKind for logs / diagnostics.
func (k MessageKind) String() string {
	switch k {
	case MessageKindNormal:
		return "normal"
	case MessageKindQueue:
		return "queue"
	}
	return "unknown"
}

// Message is one inbound user message accepted by a ChatSession.
//
// All fields are set at construction and never mutated. The
// ChatSession tracks lifecycle state (Queued / Submitted /
// Dropped) externally — on the queue's in-flight region, on
// the wire event stream, and on the Prompt that consumed the
// message — so each Message is a stable snapshot of "what the
// user said" that flows through the pipeline.
type Message struct {
	// ID is the channel-native message id (e.g. Feishu message_id),
	// or the dispatcher fallback `UserID:RFC3339Nano` when the
	// channel does not provide one. Used as the receipt-card
	// anchor (LastMessageID) and as the wire-event UserMsgID
	// key.
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

	// Kind categorizes the message for queue segmentation. Zero
	// value (MessageKindNormal) is the default for ordinary user
	// input. Set to MessageKindQueue for messages that must
	// stand alone as their own batch (scheduled, explicit
	// "execute now", etc.). Read by MessageQueue.Peek; not
	// mutated after construction — Remove + Push to change.
	Kind MessageKind
}
