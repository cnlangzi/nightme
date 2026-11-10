// chat_sender_test.go — the runtime ChatSender: posting into a
// chat that the caller does not own.
//
// The sender resolves the channel that owns the chat id, so the
// tests wire two stub channels and assert that a message posted
// into a chat lands on the owning channel only.
package runtime

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestChatSender_ReachesTheOwningChannel checks that a message
// posted into a chat lands on the channel that owns it — and on no
// other channel.
func TestChatSender_ReachesTheOwningChannel(t *testing.T) {
	clearAllMgrs(t)
	opts, sendsAlpha := setupWire(t)
	sendsBeta := &capturedSends{}

	chA := newStubChannel("alpha", sendsAlpha)
	chB := newStubChannel("beta", sendsBeta)

	pumpA, err := buildStack(chA, opts, registerMgrInAllMgrs)
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if _, err := buildStack(chB, opts, registerMgrInAllMgrs); err != nil {
		t.Fatalf("beta: %v", err)
	}

	// The chat belongs to alpha: only alpha's Manager holds it.
	if _, err := pumpA.Manager.GetOrCreate("oc_chat1", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	sender := NewChatSender()
	if err := sender.Send(context.Background(), "oc_chat1", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := sendsAlpha.messages()
	if len(got) != 1 {
		t.Fatalf("alpha received %d messages, want 1", len(got))
	}
	if got[0].ChatID != "oc_chat1" {
		t.Errorf("chat id = %q, want %q", got[0].ChatID, "oc_chat1")
	}
	if got[0].Text != "hello" {
		t.Errorf("text = %q, want %q", got[0].Text, "hello")
	}
	if got[0].Kind != messages.OutCommandReply {
		t.Errorf("kind = %v, want %v", got[0].Kind, messages.OutCommandReply)
	}
	if n := sendsBeta.count(); n != 0 {
		t.Errorf("beta received %d messages, want 0", n)
	}
}

// TestChatSender_UnknownChat_ReturnsError checks that a chat id no
// manager knows is reported instead of silently dropped.
func TestChatSender_UnknownChat_ReturnsError(t *testing.T) {
	clearAllMgrs(t)
	opts, sends := setupWire(t)

	if _, err := buildStack(newStubChannel("alpha", sends), opts, registerMgrInAllMgrs); err != nil {
		t.Fatalf("alpha: %v", err)
	}

	sender := NewChatSender()
	if err := sender.Send(context.Background(), "oc_unknown", "hello"); err == nil {
		t.Fatal("Send to an unknown chat: got nil error, want an error")
	}
	if n := sends.count(); n != 0 {
		t.Errorf("alpha received %d messages, want 0", n)
	}
}

// TestChatSender_NoEmitter_ReturnsError checks that a chat whose
// session has no emitter bound is reported instead of silently
// dropped.
func TestChatSender_NoEmitter_ReturnsError(t *testing.T) {
	clearAllMgrs(t)

	// A Manager with no Emitter wired: every session it creates
	// has a nil emitter.
	mgr := chatsession.NewManager()
	registerMgrInAllMgrs(mgr)
	if _, err := mgr.GetOrCreate("oc_chat1", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	sender := NewChatSender()
	if err := sender.Send(context.Background(), "oc_chat1", "hello"); err == nil {
		t.Fatal("Send to a chat without an emitter: got nil error, want an error")
	}
}

// TestChatSender_EmptyChatID_ReturnsError checks the empty chat id
// guard.
func TestChatSender_EmptyChatID_ReturnsError(t *testing.T) {
	clearAllMgrs(t)

	sender := NewChatSender()
	if err := sender.Send(context.Background(), "  ", "hello"); err == nil {
		t.Fatal("Send with an empty chat id: got nil error, want an error")
	}
}

// TestChatSender_EmptyText_ReturnsError checks the empty text guard.
// The channel adapters disagree on empty text (Slack posts an empty
// bullet, Feishu errors, Telegram drops it), so the sender rejects
// it before the message reaches a channel.
func TestChatSender_EmptyText_ReturnsError(t *testing.T) {
	clearAllMgrs(t)
	opts, sends := setupWire(t)

	pump, err := buildStack(newStubChannel("alpha", sends), opts, registerMgrInAllMgrs)
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if _, err := pump.Manager.GetOrCreate("oc_chat1", "claude"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	sender := NewChatSender()
	if err := sender.Send(context.Background(), "oc_chat1", "  "); err == nil {
		t.Fatal("Send with empty text: got nil error, want an error")
	}
	if n := sends.count(); n != 0 {
		t.Errorf("alpha received %d messages, want 0", n)
	}
}
