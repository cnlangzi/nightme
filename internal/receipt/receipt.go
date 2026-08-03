// Package receipt defines the MessageState enum used by
// ChatSession lifecycle and Channel rendering.
//
// v1.3 history (see SPEC §0.1): previously this package also
// defined Receipt (opaque handle) and ReceiptState (cross-channel
// FSM). Both have been removed — Gateway no longer owns any
// receipt FSM, and the receipt OBJECT is now entirely Channel-
// internal (Feishu: *feishu.MessageReceipt; echo: *echo.echoReceipt;
// future Slack / Web: their own types). Each Channel picks its
// own state shape; Gateway sees none of it.
//
// What remains here is MessageState — a parallel concept that
// tracks the inbound user message's progress through the system.
// Independent of any Channel's receipt implementation.
package receipt

// MessageState is the lifecycle stage of one inbound user message
// within nightme's processing pipeline. It answers "where is this
// message in the system right now?" so channels can render a
// user-visible progress indicator.
//
// Owner: ChatSession (per-userMsg).
// Trigger: ChatSession lifecycle events (received / forwarded /
// done / error). See SPEC §2.5 for full semantics.
//
// Scope: only produced for plain user messages, NOT slash commands.
// See docs/feat/F-31-message-state.md.
type MessageState int

const (
	// StateReceived: ChatSession has accepted the message but not
	// yet dispatched it to an AgentSession. Triggered on
	// ChatSession.GetOrCreate.
	StateReceived MessageState = iota

	// StateForwarded: the message has been dispatched to an
	// AgentSession (lazy spawn succeeded; blocks enqueued or
	// sent to PTY stdin). Triggered on successful
	// LookupActiveAgentSession.
	StateForwarded

	// StateDone: the AgentSession has finished processing this
	// message (EventDone arrived on the readPump). Triggered by
	// ChatSession.runReadPump on EventDone.
	StateDone

	// StateError: the AgentSession reported an error for this
	// message (EventError arrived). Triggered by
	// ChatSession.runReadPump on EventError.
	StateError
)

// String renders MessageState as a short human label, primarily
// for log lines and test diagnostics.
func (s MessageState) String() string {
	switch s {
	case StateReceived:
		return "received"
	case StateForwarded:
		return "forwarded"
	case StateDone:
		return "done"
	case StateError:
		return "error"
	}
	return "unknown"
}