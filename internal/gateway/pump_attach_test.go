// pump_attach_test.go — pumps attached after Start.
//
// The daemon declares readiness before any channel has started and
// attaches each one as it comes up, so AttachPumps has to keep
// working once Start has returned. A pre-Start-only attach would
// leave every inbound message black-holed behind a green startup.

package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/gateway/inbound/teststubs"
	"github.com/cnlangzi/nightme/internal/gatewaytest"
	"github.com/cnlangzi/nightme/internal/messages"
)

// feedChannel is a Channel with a live Incoming channel so a test
// can push a message through a pump and watch it get dispatched.
type feedChannel struct {
	in chan messages.InboundMessage
}

func newFeedChannel() *feedChannel {
	return &feedChannel{in: make(chan messages.InboundMessage, 4)}
}

func (c *feedChannel) Name() string                             { return "feed" }
func (c *feedChannel) Start(context.Context) error              { return nil }
func (c *feedChannel) Stop(context.Context) error               { return nil }
func (c *feedChannel) Incoming() <-chan messages.InboundMessage { return c.in }
func (c *feedChannel) Send(context.Context, messages.OutboundMessage) error {
	return nil
}
func (c *feedChannel) OnPromptEnded(context.Context, string, string, agent.PromptEndReason) {
}
func (c *feedChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return "feed", json.RawMessage("{}"), nil
}
func (c *feedChannel) SetLogger(*slog.Logger) {}
func (c *feedChannel) BuildBlocks(string, []messages.Attachment) []agent.ContentBlock {
	return nil
}

// TestAttachPumps_AfterStartIsPumped walks the daemon's own order:
// start with no channel at all, then attach one, and check the
// message it publishes is actually dispatched.
func TestAttachPumps_AfterStartIsPumped(t *testing.T) {
	commander := teststubs.NewCommander()
	commander.Recognize("/help", teststubs.Result{Consumed: true, Reply: "help"})

	ir := inbound.New(
		chatsession.NewManager(),
		commander,
		teststubs.AlwaysFallThroughShell{},
		teststubs.NewReaction(false),
		&gatewaytest.NoopEmitter{},
		"primary",
	)

	ctx := t.Context()
	gw := New(ir)
	if err := gw.Start(ctx); err != nil {
		t.Fatalf("Start with no pumps attached: %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	defer func() { _ = gw.Stop(stopCtx) }()

	ch := newFeedChannel()
	gw.AttachPumps(Pump{Channel: ch, Manager: chatsession.NewManager()})

	ch.in <- messages.InboundMessage{
		ChatID:     "oc_late",
		UserID:     "u_late",
		MessageID:  "om_late",
		Text:       "/help",
		HasMention: true,
	}

	deadline := time.After(5 * time.Second)
	for commander.Calls() == 0 {
		select {
		case <-deadline:
			t.Fatal("a pump attached after Start never dispatched its channel's inbound message")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestStart_TwiceStillRejected pins the guard that makes "attach
// after Start" safe: Start must remain one-shot, otherwise the
// second Start would spawn a second set of pumps for channels
// attached in between.
func TestStart_TwiceStillRejected(t *testing.T) {
	ir := inbound.New(
		chatsession.NewManager(),
		teststubs.AlwaysFallThrough{},
		teststubs.AlwaysFallThroughShell{},
		teststubs.NewReaction(false),
		&gatewaytest.NoopEmitter{},
		"primary",
	)
	gw := New(ir)
	if err := gw.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := gw.Start(t.Context()); err == nil {
		t.Fatal("second Start: want error, got nil")
	}
}
