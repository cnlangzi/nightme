// Package echo implements a minimal Channel that prints everything
// to its configured writer. It exists for one reason: to give
// nightme a second Channel implementation so the v0.3 hub-and-spoke
// architecture can be exercised end-to-end in CI without external
// IM credentials (Feishu AppID/AppSecret, network, etc.).
//
// In production nightme always uses Feishu. The echo channel is
// for tests, for the v0.3 Stage 4 smoke test, and for the future
// TUI / web frontends that the F-26 §3 roadmap hints at.
//
// Echo is intentionally dumb: it accepts nothing via Incoming
// (returns a channel that never produces) and writes every
// outbound message verbatim to its writer. The runtime can
// inspect the recorded messages via the public Record() helper
// for assertions.
package echo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Channel is the echo implementation. It satisfies channel.Channel.
// Incoming returns a channel that never produces; Send writes
// every message to the configured writer. Tests can read Record()
// for assertions.
type Channel struct {
	name string
	out  io.Writer

	// in is the inbound queue. It is created once in New (rather
	// than per Incoming() call) so a test can Inject a message and
	// have the gateway's reader — which called Incoming() at
	// startup — actually receive it. Buffered so Inject never
	// blocks a test that has no reader attached.
	in chan channel.Message

	mu       sync.Mutex
	recorded []messages.OutboundMessage
}

// New constructs an echo Channel. The name is surfaced via Name()
// and used by the Gateway's chatID → channel index; the writer
// receives a one-line "echo: <kind> <text>" log per outbound
// message. Pass nil for out to discard (useful in tests that
// only inspect Record()).
func New(name string, out io.Writer) *Channel {
	if name == "" {
		name = "echo"
	}
	return &Channel{name: name, out: out, in: make(chan channel.Message, 8)}
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return c.name }

// Start implements channel.Channel. Echo has nothing to connect
// to; this is a no-op that returns immediately. Tests that don't
// drive Inbound don't even need to call Start.
func (c *Channel) Start(ctx context.Context) error { return nil }

// Stop implements channel.Channel. Closes the recorded-events
// snapshot (idempotent); no goroutines to drain because echo
// never starts any.
func (c *Channel) Stop(ctx context.Context) error { return nil }

// Incoming implements channel.Channel. The returned channel yields
// only what Inject puts there, so a caller that never injects sees
// the same "never produces" behavior echo has always had.
func (c *Channel) Incoming() <-chan channel.Message {
	return c.in
}

// Inject queues an inbound message as if it had arrived from a real
// IM channel. It is the counterpart of Record(): Record asserts what
// the runtime sent, Inject drives what it receives, which together
// let a test exercise the whole inbound → dispatch → outbound path
// without a network.
//
// Non-blocking: if the buffer is full the message is dropped and
// false is returned, so a stub channel can never wedge the caller.
func (c *Channel) Inject(msg channel.Message) bool {
	select {
	case c.in <- msg:
		return true
	default:
		return false
	}
}

// ChatIDPrefix implements channel.Channel. Echo is a smoke-test
// adapter and is not registered in the channel registry
// (production runtime must never start it), so it does not need
// to declare a prefix. Returns "" to be explicit; chatstore
// never reaches this path because echo does not write chat
// sessions.
func (c *Channel) ChatIDPrefix() string { return "" }

// var _ block ensures echo.Channel satisfies channel.Channel at
// compile time.
var _ channel.Channel = (*Channel)(nil)

// Send implements channel.Channel. Writes a one-line log and
// records the message for test assertions.
func (c *Channel) Send(ctx context.Context, msg messages.OutboundMessage) error {
	if c.out != nil {
		switch {
		case msg.Kind == messages.OutChoice && msg.Choice != nil:
			fmt.Fprintf(c.out, "echo: %s chat=%s title=%q options=%v\n",
				msg.Kind, msg.ChatID, msg.Choice.Title, msg.Choice.Options)
		default:
			fmt.Fprintf(c.out, "echo: %s chat=%s text=%q\n", msg.Kind, msg.ChatID, msg.Text)
		}
	}
	c.mu.Lock()
	c.recorded = append(c.recorded, msg)
	c.mu.Unlock()
	return nil
}

// Record returns a snapshot of every message sent to this Channel
// in the order they were sent. The slice is a copy — callers can
// mutate it without affecting the Channel's internal state.
func (c *Channel) Record() []messages.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]messages.OutboundMessage, len(c.recorded))
	copy(out, c.recorded)
	return out
}

// OnPromptEnded implements channel.Channel.OnPromptEnded as a
// no-op — echo doesn't render receipts.
func (c *Channel) OnPromptEnded(ctx context.Context, chatID, userMsgID string) {}

// HealthSnapshot implements channel.Channel.HealthSnapshot as a
// no-op — echo has no connection state worth reporting. Returns
// (Name(), empty JSON object, nil) so the daemoncontrol "health"
// RPC still answers.
func (c *Channel) HealthSnapshot() (string, json.RawMessage, error) {
	return c.Name(), json.RawMessage("{}"), nil
}

// SetLogger implements channel.Channel.SetLogger as a no-op —
// echo doesn't log internally.
func (c *Channel) SetLogger(logger *slog.Logger) {}

// BuildBlocks implements channel.Channel.BuildBlocks as the
// plain-text fallback: a single ContentText block. Echo has no
// paragraph / attachment awareness — when the dispatcher falls
// back to BuildBlocks, that's what an echo-fed agent sees.
func (c *Channel) BuildBlocks(text string, _ []messages.Attachment) []agent.ContentBlock {
	if text == "" {
		return nil
	}
	return []agent.ContentBlock{{Type: agent.ContentText, Text: text}}
}
