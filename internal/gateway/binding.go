// Package gateway — binding and receipt bookkeeping types.
//
// v1.1 (F-26 §6 commit 3): the Gateway owns the chat → session
// binding table (BindingEntry) and the per-userMessage receipt FSM
// (receiptEntry). These types previously lived in:
//
//   - cmd/nightme/chat_coordinator.go (bindingRow + bindingSnapshot)
//   - cmd/nightme/run.go inline (receiptEntry as part of the
//     legacy fallback path)
//
// The migration into Gateway makes them available to handler code
// (gateway/cmd/handlers.go) without crossing the runtime boundary.

package gateway

import (
	"sync"

	"github.com/cnlangzi/nightme/internal/receipt"
)

// BindingEntry is the v1.1 chat → session binding. Persisted via
// registry.File in commit 5 (registry two-table split).
//
// ChatID is the natural key. ChatType is metadata (p2p / group /
// thread) carried for /status replies. SessionID is the FK into
// the session manager's session table. Workspace and Agent are
// denormalized for /cwd reply ("Workspace set to <ws>") and /run
// reply ("Started: <agent>") without re-querying the session.
type BindingEntry struct {
	ChatID    string
	ChatType  string
	SessionID string
	Workspace string
	Agent     string
}

// receiptEntry is the v1.1 per-userMessage receipt FSM bookkeeping.
//
// chatID is captured at CreateReceipt time so the outbound callback
// (EventResult / EventError) can locate the chat → channel mapping
// even though the Session itself no longer carries a chat_id.
//
// sessionID is the session the user message belongs to. EventResult
// / EventError iterate all receipts with this sessionID and flip
// them to Done / Error in one sweep (handles "queued then flushed"
// messages cleanly).
//
// receipt is the channel-native handle returned by
// Channel.CreateReceipt. Gateway treats it as opaque.
//
// state tracks the FSM transition history. Idempotent transitions
// (same state twice) are no-ops.
type receiptEntry struct {
	mu        sync.Mutex
	chatID    string
	sessionID string
	receipt   receipt.Receipt
	state     receipt.ReceiptState
}