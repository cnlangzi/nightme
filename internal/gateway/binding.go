// Package gateway — binding bookkeeping types.
//
// v1.3 history (SPEC §0.1): previously this file also defined
// `receiptEntry`, the Gateway-side per-userMessage receipt FSM
// bookkeeping. That type has been removed along with the entire
// Gateway-side receipt concept — receipts are now entirely
// Channel-internal. Gateway's outbound flow simply stamps
// `OutboundMessage.ReplyTo = currentTurnUserMsgID` and lets each
// Channel route by that userMsgID.
//
// What remains is `BindingEntry` (the chat → ChatSession binding),
// which Gateway continues to own per SPEC §1.2.

package gateway

// BindingEntry is the (chat_id → session_id) row stored in
// chat_sessions.json. See internal/registry for the persisted
// schema.
type BindingEntry struct {
	ChatID    string
	SessionID string
	Workspace string
	Agent     string
}

