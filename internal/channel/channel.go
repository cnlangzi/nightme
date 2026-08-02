// Package channel defines the protocol-neutral boundary between nightme and
// an instant-messaging backend.
//
// In the v0.3 hub-and-spoke architecture (see docs/feat/F-26-gateway-hub.md)
// the Channel interface is thin: connection management, native ↔ Gateway
// format conversion, and per-Channel display strategy. The Gateway
// (internal/gateway) owns message routing, buffering, and delivery
// semantics; Channel implementations translate outbound messages into
// their native UI (Feishu collapses the stream into a rolling-log message;
// Slack could post per-event thread replies; Web could render HTML).
//
// In v1.1 (see docs/feat/F-08-channel-abstraction.md §2 and
// docs/feat/F-26-gateway-hub.md §2.4) Channel additionally owns the
// receipt OBJECT (its backend-native message / reaction handles) and
// renders the receipt STATE (Pending / Executing / Done / Error). The
// STATE itself is decided by Gateway; Channel only paints transitions.
package channel

import (
	"context"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/gateway"
)

// Message is an alias for gateway.InboundMessage. Kept as a type alias
// (not a duplicate struct) so existing callers continue to compile
// while the source-of-truth lives in the gateway package.
type Message = gateway.InboundMessage

// Attachment is an alias for gateway.Attachment. Same rationale as
// Message: channel adapters populate this struct, downstream code
// reads it, and there is one definition to keep in sync.
type Attachment = gateway.Attachment

// Normalized chat type constants. Channel adapters should map their
// native values onto these.
const (
	ChatTypeP2P    = "p2p"         // 1-on-1 DM (Feishu) / private (Telegram) / im (Slack)
	ChatTypeGroup  = "group"       // group chat (Feishu / Telegram / Slack channel)
	ChatTypeThread = "topic_group" // Feishu topic group; mapped to "thread" elsewhere
)

// IsDM is the channel-package free function that delegates to
// InboundMessage.IsDM. Provided so legacy callers (channel.Message
// receiver) keep working without depending on the gateway package
// for the method itself.
func IsDM(m Message) bool { return m.IsDM() }

// Receipt is an opaque handle returned by CreateReceipt and passed to
// UpdateReceipt / DisposeReceipt. Each Channel implementation returns
// its own concrete type (Feishu: *feishu.MessageReceipt; echo:
// *echo.echoReceipt). Gateway treats Receipt as a token — it never
// reads or writes fields, only threads the value between calls.
//
// See docs/feat/F-08-channel-abstraction.md §2 and
// docs/feat/F-26-gateway-hub.md §2.4 for the v1.1 responsibility
// split: Channel owns the receipt OBJECT (its backend-specific
// message ids / reaction ids), Gateway owns the receipt STATE
// (Pending / Executing / Done / Error).
type Receipt interface{}

// ReceiptState is the cross-channel state enum. Gateway is the only
// code that decides when to transition; Channel only renders.
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

// Channel is the lifecycle and messaging contract implemented by each IM
// adapter. In v0.3 it is intentionally thin: connection management,
// format conversion, and per-Channel display strategy. Heavy logic
// (routing, buffering, retry) lives at the Gateway.
//
// In v1.1 the interface also includes receipt lifecycle RENDERING
// (CreateReceipt / UpdateReceipt / DisposeReceipt). Gateway drives
// the FSM and decides when transitions happen; Channel paints the
// visual representation in its native UI.
//
// Compile-time assertions live alongside the concrete implementations
// (internal/channel/feishu/adapter.go, internal/channel/echo/echo.go).
type Channel interface {
	// Name returns the channel's identifier (e.g. "feishu", "echo",
	// "slack"). The Gateway uses this as the lookup key when
	// resolving an outbound message's destination.
	Name() string

	// Start starts the adapter's long-lived receive loop. The
	// adapter publishes normalized InboundMessages on Incoming()
	// from this point until Stop is called.
	Start(ctx context.Context) error

	// Stop closes the receive loop and releases adapter resources.
	Stop(ctx context.Context) error

	// Send dispatches one OutboundMessage to the channel's native UI.
	// "Delivered" means this call returned nil — Gateway treats Send
	// as fire-and-ack and does not retry. The Channel may silently
	// drop OutboundMessage kinds its UI cannot represent (e.g. Slack
	// cannot swap reactions in place) without surfacing an error.
	Send(ctx context.Context, msg gateway.OutboundMessage) error

	// Incoming returns the channel's normalized message stream.
	Incoming() <-chan Message

	// --- v1.1 additions: receipt lifecycle RENDERING ---
	//
	// Gateway is the only caller; Channels do not assume any
	// particular transition order. UpdateReceipt is idempotent for
	// the same state (Channel may short-circuit).

	// CreateReceipt creates a new channel-native receipt for an
	// incoming user message and returns an opaque Receipt handle
	// that Gateway holds for the lifetime of the user message's
	// receipt FSM.
	//
	// The channel decides how to render the initial state (typically
	// Pending → ⏳). blocks is the structured user turn (text +
	// optional image/file attachments); the channel formats these
	// into its native UI (Feishu: receipt text message + ⏳
	// reaction; echo: log line).
	//
	// Errors propagate to Gateway, which will skip receipt
	// bookkeeping and fall back to plain Send for that user
	// message.
	CreateReceipt(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) (Receipt, error)

	// UpdateReceipt transitions the receipt to the given state. The
	// channel renders the state in its native UI:
	//
	//   ReceiptPending   → ⏳  (or keep current emoji if already Pending)
	//   ReceiptExecuting → 🔄  (swap reaction or append indicator)
	//   ReceiptDone      → ✅  (swap reaction, optionally edit body)
	//   ReceiptError     → ❌  (swap reaction)
	//
	// Idempotent for the same state — channels may short-circuit.
	UpdateReceipt(ctx context.Context, receipt Receipt, state ReceiptState) error

	// DisposeReceipt cleans up the receipt (Feishu: delete the
	// receipt message; echo: log a dispose line; web: remove the
	// element). Called after the final UpdateReceipt(Done|Error).
	DisposeReceipt(ctx context.Context, receipt Receipt) error
}