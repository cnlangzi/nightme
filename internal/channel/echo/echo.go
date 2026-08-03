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
	"fmt"
	"io"
	"sync"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/gateway"
)

// Channel is the echo implementation. It satisfies channel.Channel.
// Incoming returns a channel that never produces; Send writes
// every message to the configured writer. Tests can read Record()
// for assertions.
type Channel struct {
	name string
	out  io.Writer

	mu       sync.Mutex
	recorded []gateway.OutboundMessage
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
	return &Channel{name: name, out: out}
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

// Incoming implements channel.Channel. Echo never produces
// inbound messages, so the channel never yields.
func (c *Channel) Incoming() <-chan channel.Message {
	return make(chan channel.Message)
}

// var _ block ensures echo.Channel satisfies channel.Channel at
// compile time.
var _ channel.Channel = (*Channel)(nil)

// Send implements channel.Channel. Writes a one-line log and
// records the message for test assertions.
func (c *Channel) Send(ctx context.Context, msg gateway.OutboundMessage) error {
	if c.out != nil {
		fmt.Fprintf(c.out, "echo: %s chat=%s text=%q\n", msg.Kind, msg.ChatID, msg.Text)
	}
	c.mu.Lock()
	c.recorded = append(c.recorded, msg)
	c.mu.Unlock()
	return nil
}

// Record returns a snapshot of every message sent to this Channel
// in the order they were sent. The slice is a copy — callers can
// mutate it without affecting the Channel's internal state.
func (c *Channel) Record() []gateway.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]gateway.OutboundMessage, len(c.recorded))
	copy(out, c.recorded)
	return out
}
