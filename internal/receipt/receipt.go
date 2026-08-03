// Package receipt defines the cross-channel Receipt / ReceiptState
// contract shared by Gateway and Channel implementations.
//
// v1.1 (F-26 §6 commit 3) rationale: Gateway owns the receipt FSM
// state machine; Channel owns the receipt OBJECT (its backend-
// native handles). The two layers need to communicate through a
// shared type. Previously these types lived in the channel
// package; that created an import cycle (channel imports gateway
// for InboundMessage / OutboundMessage types, gateway imports
// channel for Receipt). Moving them here breaks the cycle and
// keeps the contract in a neutral package.
//
// v1.3 (F-31): adds MessageState — a parallel concept that
// tracks the inbound user message's progress through the
// system (vs ReceiptState which tracks the response's
// rendering lifecycle). The two are independent — both keyed
// by userMsgID, but different owners, triggers, and semantics.
package receipt

// Receipt is an opaque handle. Each Channel returns its own
// concrete type (Feishu: *feishu.MessageReceipt; echo:
// *echo.echoReceipt). Gateway treats Receipt as a token — it
// never reads or writes fields, only threads the value between
// calls.
type Receipt interface{}

// ReceiptState is the cross-channel state enum. Gateway is the
// only code that decides when to transition; Channel only renders.
//
// State semantics:
//
//   - Pending:   the user message has been received and is either
//                queued in InputBuffer (Busy) or about to dispatch
//                (Idle). Rendered as ⏳ by Feishu, "[pending]" by
//                echo.
//   - Executing: the user message has been dispatched to the agent
//                and the agent is processing it. Rendered as 🔄.
//   - Done:      the agent finished processing this user message
//                and emitted a result event. Rendered as ✅.
//   - Error:     dispatch or processing failed; user may retry.
//                Rendered as ❌.
type ReceiptState int

const (
	// ReceiptPending is the initial state after CreateReceipt.
	ReceiptPending ReceiptState = iota

	// ReceiptExecuting means the user message has been sent to the
	// agent and the agent is processing it.
	ReceiptExecuting

	// ReceiptDone means the agent has finished processing this user
	// message and the final result has been emitted.
	ReceiptDone

	// ReceiptError means dispatch or processing failed. The user
	// should retry; nightme does not auto-retry receipt failures.
	ReceiptError
)

// MessageState is the lifecycle stage of one inbound user message
// within nightme's processing pipeline. It answers "where is this
// message in the system right now?" so channels can render a
// user-visible progress indicator.
//
// v1.3 (F-31): independent of ReceiptState. Both are keyed by
// userMsgID but have different owners, triggers, and semantics:
//
//   - MessageState is owned by ChatSession (lifecycle events)
//   - ReceiptState is owned by Gateway (response rendering FSM)
//
// Scope: only produced for plain user messages, NOT slash commands.
// See docs/feat/F-31-message-state.md and SPEC §2.5.
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