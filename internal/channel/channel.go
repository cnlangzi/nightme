// Package channel — IM adapter interface and concrete adapters
// (Feishu, echo test stub). Channel is the runtime contract every
// transport (Feishu, future Slack, future Web UI, …) must satisfy:
// it owns BOTH the inbound pump (Incoming) and the outbound send
// surface (Send). The runtime wires it into the outbound
// chokepoint (which holds the only outbound reference to the
// Channel) and into the Gateway's inbound pump (which reads
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
	"time"

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
//     begins publishing on Incoming. Adapter-specific
//     (Feishu: WS connect; echo: no-op).
//   - Stop:        closes the receive loop and releases adapter
//     resources.
//   - Incoming:    the channel publishes inbound user messages on
//     this channel; the Gateway reads it in pumpInbound.
//     Closed when the channel shuts down.
//   - Send:        the sole egress for the messages the runtime
//     sends itself. Every OutboundKind (text, thinking, tool,
//     OutChoice, OutChoicePatch, receipt, reaction, …) goes
//     through here. Channel routes by Kind; interactive-choice
//     correlation is Channel-private via Choice.RequestID.
//     Send returns the id of the chat message the message became:
//     the id a caller uses to address that message again. It is
//     empty when the channel drops the kind or cannot name the
//     message.
//
// Impersonator is a different contract: what a program says in a
// chat has to come back into the daemon as an inbound message, so
// it has an interface of its own.
//
// "Abstract stays abstract, concrete stays concrete": the Gateway
// knows only this surface. Receipt shape and card handles are
// each Channel's private affair.
//
// The chat-id namespace prefix (e.g. telegram "tg_", feishu
// "oc_", slack "sl_") lives in the registry, not
// on this interface — see channel.Register /
// channel.ChatIDPrefix(name). It is a static property of the
// channel family, not per-instance state, so consumers
// (chatstore validation, routing) read it from the registry
// rather than calling a method on every constructed Channel.
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
//   - HealthSummary: the cross-channel normalized health view
//     (see HealthSummary below). The daemoncontrol server calls
//     this next to HealthSnapshot so the CLI can render one
//     section per channel without decoding any adapter's
//     payload shape.
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
	Send(ctx context.Context, msg messages.OutboundMessage) (string, error)

	OnPromptEnded(ctx context.Context, chatID, userMsgID string, reason agent.PromptEndReason)
	HealthSnapshot() (name string, payload json.RawMessage, err error)
	HealthSummary() HealthSummary
	SetLogger(logger *slog.Logger)
	BuildBlocks(text string, attachments []messages.Attachment) []agent.ContentBlock
}

// HealthSummary is the cross-channel normalized health view. Every
// adapter fills it in and the fields mean the same thing for every
// channel, so `nightme doctor` can render one section per channel
// without decoding any adapter's own payload shape.
//
// Connected is the adapter's own view of the link. LinkVerified
// reports whether a real remote round trip has proven that the
// credentials and the link both work. The two differ for adapters
// that can only prove they were started — a started adapter whose
// link was never verified must not be reported as connected.
//
// LastInboundAt / LastOutboundAt are zero when the adapter does not
// track them; readers must treat a zero value as "never", not as the
// zero timestamp. The zero value is left out of the JSON so a
// consumer never reads it as a real time.
type HealthSummary struct {
	Channel        string    `json:"channel"`
	Connected      bool      `json:"connected"`
	LinkVerified   bool      `json:"link_verified"`
	LastError      string    `json:"last_error,omitempty"`
	LastInboundAt  time.Time `json:"last_inbound_at,omitzero"`
	LastOutboundAt time.Time `json:"last_outbound_at,omitzero"`
}

// Impersonator lets a program take part in a chat the way a person
// does: what it says is a visible message of the chat, and it enters
// the daemon through Incoming() like any other message.
//
// A channel that implements it can be driven by the personal agent
// (internal/me). What the agent says is posted on the platform like
// any other message and comes back in as a real inbound message, so
// the receipts, the reactions and the interactive prompts anchor to
// it the way they anchor to a person's message.
//
// An adapter that does not implement it says so when the agent asks
// to speak: the run cannot be driven in that chat.
type Impersonator interface {
	// Speak posts text into the chat and returns the id of the
	// message it became. The id is the id the daemon sees when the
	// message comes back in.
	Speak(ctx context.Context, chatID, text string, opts SpeakOptions) (string, error)
	// Ask puts a question in the chat and returns the id of the
	// message it became.
	//
	// The message is visible and the operator can answer it, but the
	// daemon does not take it as a turn of its own: the question
	// belongs to the program that asked it, not to the agent.
	Ask(ctx context.Context, chatID, text string, opts SpeakOptions) (string, error)
	// Act clicks a button of an interactive prompt, as if a person
	// had clicked it. The prompt is patched to show the choice.
	Act(ctx context.Context, chatID string, action messages.ActionPayload) error
}

// SpeakOptions shapes what a Speak call says and how it is attributed.
//
// The zero value says the text itself, as the user of the run.
type SpeakOptions struct {
	// ReplyTo names the message the text quotes, like a human reply.
	ReplyTo string
	// Display is what the chat shows. Empty means the text itself.
	// A run prefixes its text with a bot mark, so the display and the
	// dispatched text usually differ.
	Display string
	// Owner is the user the message is attributed to. Empty means the
	// run's own user.
	Owner string
	// RunID names the run the message belongs to.
	RunID string
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
