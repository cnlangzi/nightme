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
// v1.3 (SPEC §0.1): the receipt lifecycle interface methods
// (CreateReceipt / UpdateReceipt / DisposeReceipt) have been removed.
// The receipt OBJECT is now entirely Channel-internal — Gateway does
// not see it. Each Channel decides its own state shape and storage
// form (Feishu: *MessageReceipt with append/PATCH; Slack: thread map;
// Web: DOM nodes). Gateway routes outbound events by stamping
// msg.ReplyTo = currentTurnUserMsgID; Channel looks up its own
// receipt by userMsgID.
//
// "Abstract stays abstract, concrete stays concrete": Gateway knows
// only the five lifecycle/messaging methods below. Receipt shape is
// each Channel's private affair.
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

// Channel is the lifecycle and messaging contract implemented by each IM
// adapter. v1.3 intentionally minimal:
//
//   - 5 methods (Name / Start / Stop / Send / Incoming)
//   - No receipt FSM API (Gateway does not own receipt state)
//   - No receipt handle type (Channel owns its own receipt objects)
//
// Heavy logic (routing, buffering, the userMsgID anchor) lives at
// the Gateway. Channel implementations translate OutboundMessage
// (which carries msg.ReplyTo = userMsgID) into their native UI and
// decide internally how to find / create / patch the receipt for
// that userMsgID.
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
	//
	// For OutText / OutToolStart / OutToolEnd / OutThinking kinds:
	// the Channel is expected to route by msg.ReplyTo (userMsgID) to
	// find its existing receipt (card / thread / DOM node) and patch
	// it in place; if no receipt exists for that userMsgID yet,
	// cold-create one before patching. This is what makes F-25
	// rolling-log UX work without Gateway knowing the receipt shape.
	Send(ctx context.Context, msg gateway.OutboundMessage) error

	// Incoming returns the channel's normalized message stream.
	Incoming() <-chan Message
}

// Compile-time guard: the agent package is referenced in this file
// only via type aliases above (kept for back-compat with v0.x
// callers). If a future cleanup removes the agent import, replace
// this with the actual reference (e.g. a public helper that uses
// agent.ContentBlock). Until then, ensure the import is not
// tree-shaken by referencing it indirectly:
var _ = agent.ContentText