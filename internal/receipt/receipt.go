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