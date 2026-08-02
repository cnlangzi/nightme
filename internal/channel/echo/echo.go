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

	"github.com/cnlangzi/nightme/internal/agent"
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

// --- v1.1 receipt lifecycle stubs ---
//
// CreateReceipt / UpdateReceipt / DisposeReceipt are logging-only
// stubs. They let the Gateway exercise the v1.1 receipt FSM in
// tests / smoke runs without a real IM backend. Echo never
// returns errors from these methods (no network).
//
// Tests asserting the receipt FSM can read the recorded entries
// from the Channel's record map.

type echoReceipt struct {
	chatID   string
	userMsg  string
	blocks   []agent.ContentBlock
	state    channel.ReceiptState
}

// CreateReceipt records the receipt and prints a one-line
// "echo: receipt created" log. Returns the opaque receipt handle.
func (c *Channel) CreateReceipt(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) (channel.Receipt, error) {
	rcpt := &echoReceipt{chatID: chatID, userMsg: userMsgID, blocks: blocks, state: channel.ReceiptPending}
	c.mu.Lock()
	c.recorded = append(c.recorded, gateway.OutboundMessage{ChatID: chatID, Kind: gateway.OutText, Text: fmt.Sprintf("[receipt %s] created (state=pending, chat=%s)", userMsgID, chatID)})
	c.mu.Unlock()
	if c.out != nil {
		fmt.Fprintf(c.out, "[receipt %s] created (state=pending, chat=%s)\n", userMsgID, chatID)
	}
	return channel.Receipt(rcpt), nil
}

// UpdateReceipt records the state transition and prints a log line.
// Idempotent for the same state.
func (c *Channel) UpdateReceipt(ctx context.Context, receipt channel.Receipt, state channel.ReceiptState) error {
	if receipt == nil {
		return nil
	}
	r, ok := receipt.(*echoReceipt)
	if !ok {
		return fmt.Errorf("echo: receipt is not *echoReceipt: %T", receipt)
	}
	r.state = state
	if c.out != nil {
		fmt.Fprintf(c.out, "[receipt %s] state=%s\n", r.userMsg, stateName(state))
	}
	c.mu.Lock()
	c.recorded = append(c.recorded, gateway.OutboundMessage{ChatID: r.chatID, Kind: gateway.OutText, Text: fmt.Sprintf("[receipt %s] state=%s", r.userMsg, stateName(state))})
	c.mu.Unlock()
	return nil
}

// DisposeReceipt records the dispose and prints a log line.
// Idempotent.
func (c *Channel) DisposeReceipt(ctx context.Context, receipt channel.Receipt) error {
	if receipt == nil {
		return nil
	}
	r, ok := receipt.(*echoReceipt)
	if !ok {
		return fmt.Errorf("echo: receipt is not *echoReceipt: %T", receipt)
	}
	if c.out != nil {
		fmt.Fprintf(c.out, "[receipt %s] disposed\n", r.userMsg)
	}
	return nil
}

// stateName renders a ReceiptState as a short human label for log
// lines. Kept private to the echo package.
func stateName(s channel.ReceiptState) string {
	switch s {
	case channel.ReceiptPending:
		return "pending"
	case channel.ReceiptExecuting:
		return "executing"
	case channel.ReceiptDone:
		return "done"
	case channel.ReceiptError:
		return "error"
	}
	return "unknown"
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
