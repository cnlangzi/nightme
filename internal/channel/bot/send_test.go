package bot

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestBotSendRoutesReplyToRun verifies the lock-step design
// invariant: when the gateway's outbound.Emitter calls bot.Send
// with a reply (carrying the chat's chatID), bot looks up the
// botRun registered under that chatID and delivers msg.Text to
// that run's reply channel. The waiting wfe.Tick goroutine
// receives the text and continues the workflow.
//
// This is Test B in the integration plan: it's a pure unit test
// (no gateway, no chatsession). The corresponding integration
// test (TestBotMessageFlowsThroughGateway in integration_test.go)
// wires the gateway end-to-end and verifies the same contract
// holds when the gateway actually invokes bot.Send.
func TestBotSendRoutesReplyToRun(t *testing.T) {
	b := New(Config{})

	const chatID = "bot:wf:test-reply:42"
	run := &botRun{
		runID:    "test-reply-42",
		chatID:   chatID,
		workflow: nil, // not needed for routing
		env:      map[string]string{},
		reply:    make(chan string, 1),
	}
	b.muRuns.Lock()
	b.runsByChatID[chatID] = run
	b.muRuns.Unlock()

	// Simulate the gateway's outbound.Emitter calling bot.Send
	// with an agent reply.
	err := b.Send(context.Background(), messages.OutboundMessage{
		ChatID: chatID,
		Kind:   messages.OutReply,
		Text:   "agent reply text",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The reply channel must have received the text.
	select {
	case got := <-run.reply:
		if got != "agent reply text" {
			t.Errorf("reply = %q, want 'agent reply text'", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: reply not delivered to run.reply")
	}
}

// TestBotSendNoOpForUnknownChatID verifies that Send silently
// drops messages for chatIDs that have no registered run. The
// gateway's emitter may fire Send for stale chatIDs (e.g. when
// the run finished but the agent is still flushing) — bot must
// not block or panic.
func TestBotSendNoOpForUnknownChatID(t *testing.T) {
	b := New(Config{})

	err := b.Send(context.Background(), messages.OutboundMessage{
		ChatID: "bot:wf:unknown:99",
		Kind:   messages.OutReply,
		Text:   "stale",
	})
	if err != nil {
		t.Errorf("Send on unknown chatID should be no-op, got %v", err)
	}
}

// TestBotSendFullReplyChannelDrops verifies the channel-full
// guard. If the run's reply channel is full (run finished, no
// consumer), Send drops the message rather than blocking the
// gateway's emitter.
func TestBotSendFullReplyChannelDrops(t *testing.T) {
	b := New(Config{})

	chatID := "bot:wf:full:1"
	run := &botRun{
		runID:  "full-1",
		chatID: chatID,
		reply: make(chan string, 1), // capacity 1
	}
	// Pre-fill the channel.
	run.reply <- "stale-reply"
	b.muRuns.Lock()
	b.runsByChatID[chatID] = run
	b.muRuns.Unlock()

	// Send one more — should drop silently.
	err := b.Send(context.Background(), messages.OutboundMessage{
		ChatID: chatID,
		Kind:   messages.OutReply,
		Text:   "new-reply",
	})
	if err != nil {
		t.Errorf("Send on full channel should drop, got %v", err)
	}

	// Verify: channel still has only the old message.
	if got := <-run.reply; got != "stale-reply" {
		t.Errorf("reply = %q, want 'stale-reply' (new should have been dropped)", got)
	}
}
