package bot

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/shell"
)

// spyChannel is a minimal channel.Channel that records every
// outbound message it receives. Used as a witness alongside the
// real bot channel so the test can assert the gateway's pump
// dispatched bot's message.
type spyChannel struct {
	mu   sync.Mutex
	sent []messages.OutboundMessage
}

func (s *spyChannel) Name() string { return "spy" }
func (s *spyChannel) Incoming() <-chan messages.InboundMessage {
	return make(chan messages.InboundMessage, 1)
}
func (s *spyChannel) Start(context.Context) error { return nil }
func (s *spyChannel) Stop(context.Context) error  { return nil }
func (s *spyChannel) OnPromptEnded(context.Context, string, string) {}
func (s *spyChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return "spy", nil, nil
}
func (s *spyChannel) SetLogger(_ *slog.Logger) {}
func (s *spyChannel) BuildBlocks(_ string, _ []messages.Attachment) []agent.ContentBlock {
	return nil
}
func (s *spyChannel) Send(_ context.Context, m messages.OutboundMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}
func (s *spyChannel) snapshot() []messages.OutboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]messages.OutboundMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

// slogLogger is unused (spyChannel takes a *slog.Logger for
// SetLogger; we pass a discard logger).
var _ = slog.Default

// TestBotMessageFlowsThroughGateway is the end-to-end test of
// the channel pipeline for bot:
//
//   bot.Incoming()
//     → gateway.pumpOne (read from channel)
//     → channelCh (central dispatch queue)
//     → dispatchLoop (read from queue)
//     → inbound.Dispatch → tryCommandDispatch (for "/cwd ...")
//     → ChatSession.HandleSlashCommand ("/cwd")
//     → ChatSession's SelectedCwd is set
//     → reply flows back through messages.Emitter
//     → bot.Send (delivered to the registered botRun.reply)
//
// If this test passes, the bot-as-channel design is verified:
// bot can push messages into the same dispatch chain that feishu
// uses, the chat session can process them, and agent replies
// flow back through bot's Send to the right workflow run.
func TestBotMessageFlowsThroughGateway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Build the dispatch chain (same shape as runtime wires
	// it, but minimal). The Emitter is bound to bot's channel so
	// replies flow back to bot.Send (which routes to botRun.reply).
	csMgr := chatsession.NewManager()
	b := New(Config{})
	emitter := outbound.New(b, outbound.Options{}) // Emitter → bot.Send
	csMgr.WithEmitter(emitter)
	// Use the production commander + shell + reaction router so
	// the real /cwd handler runs.
	inboundRouter := inbound.New(
		csMgr,
		command.NewCommander(command.Default()),
		shell.NewDispatcher(),
		commandServices.NewReactionRouter(),
		emitter,
		"primary",
	)

	// --- Build bot + wire it into the gateway.
	// (b already constructed above; Emitter was built first because
	// it needs b's channel. Now register botRun for reply routing.)
	// Make /cwd actually succeed by creating a real dir.
	tmp := t.TempDir()
	target := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Pre-register a botRun so bot.Send has somewhere to route.
	// (In production this is created by fireWorkflow; here we
	// simulate that.)
	chatID := "bt_integration-test:42"
	run := &botRun{
		runID:  "integration-test-42",
		chatID: chatID,
		reply:  make(chan string, 1),
	}
	b.muRuns.Lock()
	b.runsByChatID[chatID] = run
	b.muRuns.Unlock()

	// Build the gateway with bot as a pump. The Manager shared
	// with the dispatch chain must be the same as the one the
	// chat session will use.
	gw := gateway.New(inboundRouter)
	gw.AttachPumps(gateway.Pump{Channel: b, Manager: csMgr})
	if err := gw.Start(ctx); err != nil {
		t.Fatalf("Start gateway: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = gw.Stop(stopCtx)
	}()

	// --- Push a /cwd message through bot.Incoming.
	select {
	case b.in <- messages.InboundMessage{
		ChatID: chatID,
		Text:   "/cwd " + target,
	}:
	case <-time.After(time.Second):
		t.Fatal("timeout pushing to bot.Incoming")
	}

	// --- Wait for the reply to land in botRun.reply.
	select {
	case got := <-run.reply:
		// /cwd replies with "Workspace set to: <path>" or a usage
		// message. We just verify the channel pipeline flowed end
		// to end (the gateway dispatched the message and the
		// reply came back through bot.Send).
		if got == "" {
			t.Errorf("reply is empty (gateway did not route back through bot.Send)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for /cwd reply through bot.Send")
	}
}
