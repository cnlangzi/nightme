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
package channel

import (
	"context"

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
// adapter. In v0.3 it is intentionally thin: connection management,
// format conversion, and per-Channel display strategy. Heavy logic
// (routing, buffering, retry) lives in the Gateway.
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
}
