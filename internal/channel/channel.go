// Package channel — IM adapter interface and concrete adapters
// (Feishu, echo test stub). Channel is the runtime contract every
// transport (Feishu, future Slack, future Web UI, …) must satisfy:
// it owns BOTH the inbound pump (Incoming) and the outbound send
// surface (Send / SendCard). The runtime wires it into the
// outbound chokepoint (which holds the only outbound reference to
// the Channel) and into the Gateway's inbound pump (which reads
// from Incoming and never calls Send).
//
// Adapter packages in this directory (channel/feishu,
// channel/echo) implement the interface via Go's structural typing
// — no explicit registration required.
package channel

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Channel is the lifecycle and messaging contract every IM
// adapter must satisfy.
//
// Lifecycle:
//
//   - Name:        diagnostic / logging identifier
//   - Start:       opens the adapter's long-lived receive loop and
//                  begins publishing on Incoming. Adapter-specific
//                  (Feishu: WS connect; echo: no-op).
//   - Stop:        closes the receive loop and releases adapter
//                  resources.
//   - Incoming:    the channel publishes inbound user messages on
//                  this channel; the Gateway reads it in pumpInbound.
//                  Closed when the channel shuts down.
//   - Send:        plain-text outbound (one shot, no thread binding).
//                  Used for OutReply / OutResult / OutCommandReply /
//                  OutInit / OutTask* messages.
//   - SendCard:    interactive card outbound; returns the bot-side
//                  message id assigned by the channel so callers can
//                  correlate the rendered card with later
//                  card.action.trigger callbacks.
//
// "Abstract stays abstract, concrete stays concrete": the Gateway
// knows only this surface. Receipt shape is each Channel's private
// affair.
//
// Channel-specific extensions:
//
//   - OnPromptEnded: the runtime fires this when a chat's
//     active prompt terminates (success / error / cancel).
//     Implementations typically transition a receipt card to
//     PromptDone + add a ✅ reaction. No-op for adapters
//     without receipt rendering (echo, …).
//   - HealthSnapshot: the daemoncontrol server calls this on
//     every "health" RPC. The returned JSON is served
//     verbatim. No-op adapters return (Name(), empty
//     RawMessage, nil).
//   - SetLogger: the runtime calls this once after
//     construction, before Start. Lets adapters route their
//     internal log lines through the daemon's structured
//     logger. No-op for adapters that don't log (echo).
//   - BuildBlocks: the runtime's dispatcher calls this as the
//     fallback when msg.Blocks is empty. Adapters that
//     produce structured rich text (Feishu paragraphs,
//     Slack blocks, …) implement their per-channel shape;
//     plain-text adapters return [{Type: ContentText, Text}].
//     The package-level feishu.BuildBlocks helper stays for
//     direct callers (chatsession tests); the Adapter
//     method delegates to it.
type Channel interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Incoming() <-chan messages.InboundMessage
	Send(ctx context.Context, msg messages.OutboundMessage) error
	SendCard(ctx context.Context, msg messages.OutboundMessage) (msgID string, err error)

	OnPromptEnded(ctx context.Context, chatID, userMsgID string)
	HealthSnapshot() (name string, payload json.RawMessage, err error)
	SetLogger(logger *slog.Logger)
	BuildBlocks(text string, attachments []messages.Attachment) []agent.ContentBlock
}

// Message is an alias for messages.InboundMessage. Kept for
// back-compat with adapter code that imports channel.Message.
type Message = messages.InboundMessage

// Attachment is an alias for messages.Attachment. Same rationale as
// Message: adapter code reads this struct from a single canonical
// definition.
type Attachment = messages.Attachment

// Normalized chat type constants. Channel adapters may use these
// internally for chat-type classification, but **no Channel
// introduces a thread concept into nightme data model** (F-33):
//   - ChatTypeThread was removed; thread is a Feishu-side rendering
//     concern that Channel handles without a separate constant.
//   - nightme Gateway / ChatSession / Registry never see ChatType
//     (the field was removed in v1.3.x; see SPEC §3.1).
//
// Channel adapters should map their native chat_type onto one of
// the two values below, or treat unknown types as not-supported.
const (
	ChatTypeP2P   = "p2p"   // 1-on-1 DM (Feishu) / private (Telegram) / im (Slack)
	ChatTypeGroup = "group" // group chat (Feishu / Telegram / Slack channel)
)

// IsDM was removed in F-33 (D1). Chat type classification is no
// longer exposed via this channel-package helper; if a caller
// needs to distinguish DM from group, derive it from the chat id
// shape (e.g. presence of a leading @ for 1-on-1 DMs in some
// channels, or a Channel-specific mapping) rather than relying on
// a removed field. The IsDM helper was the last remaining user of
// InboundMessage.IsDM() which itself was removed in the same PR.