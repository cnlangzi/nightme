package gateway

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/receipt"
)

// TestOnMessageState_TranslatesToOutbound verifies that
// Gateway.OnMessageState produces the right OutboundMessage
// (Kind, Meta, MessageStatePayload) and forwards it through
// the resolved channel's Send.
func TestOnMessageState_TranslatesToOutbound(t *testing.T) {
	gw, ch := newWiredRouter(t)
	gw.OnMessageState("oc_chat", "om_user_msg", receipt.StateReceived)

	if len(ch.sends) != 1 {
		t.Fatalf("got %d sends; want 1", len(ch.sends))
	}
	got := ch.sends[0]
	if got.Kind != OutMessageState {
		t.Errorf("Kind = %v; want OutMessageState", got.Kind)
	}
	if got.ChatID != "oc_chat" {
		t.Errorf("ChatID = %q; want oc_chat", got.ChatID)
	}
	if id, _ := got.Meta["message_id"].(string); id != "om_user_msg" {
		t.Errorf("Meta[message_id] = %v; want om_user_msg", got.Meta["message_id"])
	}
	st, ok := got.Meta["state"].(receipt.MessageState)
	if !ok {
		t.Fatalf("Meta[state] type = %T; want receipt.MessageState", got.Meta["state"])
	}
	if st != receipt.StateReceived {
		t.Errorf("Meta[state] = %v; want StateReceived", st)
	}
	if got.MessageState == nil || got.MessageState.State != receipt.StateReceived {
		t.Errorf("MessageState payload missing or wrong: %+v", got.MessageState)
	}
}

// TestOnMessageState_NoChannelDrops verifies that OnMessageState
// is a silent drop when no channel is registered for the chat
// (per F-31 §9: never block caller, log warn).
func TestOnMessageState_NoChannelDrops(t *testing.T) {
	gw, ch := newWiredRouter(t)
	// Clear defaultChannel fallback so unknown chatID has no path.
	gw.mu.Lock()
	gw.defaultChannel = nil
	gw.mu.Unlock()
	gw.OnMessageState("oc_unknown", "om_msg", receipt.StateReceived)
	if len(ch.sends) != 0 {
		t.Errorf("got %d sends; want 0 (no channel registered)", len(ch.sends))
	}
}

// TestOnMessageState_EmptyIDsDrops verifies that empty chatID or
// userMsgID is a silent drop (defensive against malformed events).
func TestOnMessageState_EmptyIDsDrops(t *testing.T) {
	gw, ch := newWiredRouter(t)
	gw.OnMessageState("", "om_msg", receipt.StateReceived)
	gw.OnMessageState("oc_chat", "", receipt.StateReceived)
	if len(ch.sends) != 0 {
		t.Errorf("got %d sends; want 0 (empty chat/user ID)", len(ch.sends))
	}
}

// TestOnMessageState_AllStatesPassThrough verifies that each
// MessageState value passes through to Channel.Send unchanged.
func TestOnMessageState_AllStatesPassThrough(t *testing.T) {
	gw, ch := newWiredRouter(t)
	states := []receipt.MessageState{
		receipt.StateReceived,
		receipt.StateForwarded,
		receipt.StateDone,
		receipt.StateError,
	}
	for i, s := range states {
		gw.OnMessageState("oc_chat", "om_"+string(rune('a'+i)), s)
	}
	if len(ch.sends) != len(states) {
		t.Fatalf("got %d sends; want %d", len(ch.sends), len(states))
	}
	for i, s := range states {
		got := ch.sends[i]
		if got.Kind != OutMessageState {
			t.Errorf("sends[%d].Kind = %v; want OutMessageState", i, got.Kind)
		}
		st, _ := got.Meta["state"].(receipt.MessageState)
		if st != s {
			t.Errorf("sends[%d].Meta[state] = %v; want %v", i, st, s)
		}
	}
}

// newWiredRouter builds a minimal Gateway with a single fakeChannel
// attached and chatToChan populated for "oc_chat". Returns the
// concrete *Router so the test can call package-internal helpers
// (OnMessageState, AttachChannels) directly.
func newWiredRouter(t *testing.T) (*Router, *fakeChannel) {
	t.Helper()
	ch := &fakeChannel{}
	gw := New(nil, nil).(*Router)
	gw.AttachChannels(ch)
	// Resolve-channel path uses g.chatToChan populated by pumpInbound
	// in production; for tests, seed it directly.
	gw.mu.Lock()
	gw.chatToChan["oc_chat"] = ch
	gw.defaultChannel = ch
	gw.mu.Unlock()
	return gw, ch
}

// silence unused-import warning for context (used in fakeChannel.Send signature).
var _ = context.Background