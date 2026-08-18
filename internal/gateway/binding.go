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

// BindingEntry is the v1.1 chat → session binding. Persisted via
// registry.File in commit 5 (registry two-table split).
//
// ChatID is the natural key. ChatType was removed in F-33 (D1):
// nightme no longer carries chat-type classification at the
// binding layer; Channel owns that internally. SessionID is the
// FK into the session manager's session table. Workspace and Agent
// are denormalized for /cwd reply ("Workspace set to <ws>") and
// /use reply ("Now using <agent>, pid=<N>, cwd=<ws>") without
// re-querying the session.
//
