package gateway

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fakeChannel is a minimal Channel implementation used by
// gateway-level tests (kept local because the legacy shared
// definition lived in the deleted handlers_chatsession_test.go).
// Records every Send / SendCard so tests can assert on the
// resulting OutboundMessages.
type fakeChannel struct {
	mu    sync.Mutex
	sends []OutboundMessage
}

func (c *fakeChannel) Name() string { return "fake" }
func (c *fakeChannel) Incoming() <-chan InboundMessage {
	return make(<-chan InboundMessage)
}
func (c *fakeChannel) Send(_ context.Context, m OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, m)
	return nil
}
func (c *fakeChannel) SendCard(_ context.Context, m OutboundMessage) (string, error) {
	_ = c.Send(context.Background(), m)
	c.mu.Lock()
	defer c.mu.Unlock()
	return "fake-card-" + strconv.Itoa(len(c.sends)), nil
}

// TestOnMessageState_TranslatesToOutbound verifies that
// Gateway.OnMessageState produces the right OutboundMessage
// (Kind, MessageStatePayload) and forwards it through
// the resolved channel's Send. After §1.4 cleanup, the
// message_id + state fields live in the typed MessageStatePayload
// (not in Meta).
func TestOnMessageState_TranslatesToOutbound(t *testing.T) {
	gw, ch := newWiredRouter(t)
	gw.OnMessageState("oc_chat", "om_user_msg", agent.MessageQueued)

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
	if got.MessageState == nil {
		t.Fatalf("MessageState payload is nil; want typed transport")
	}
	if got.MessageState.MessageID != "om_user_msg" {
		t.Errorf("MessageState.MessageID = %q; want om_user_msg", got.MessageState.MessageID)
	}
	if got.MessageState.State != agent.MessageQueued {
		t.Errorf("MessageState.State = %v; want StateReceived", got.MessageState.State)
	}
	// F-44 + reply.go safe pattern: ReplyTo must be set to userMsgID
	// so the Feishu channel's Typing-placeholder handler has an
	// anchor to reserve the inline-rendering slot. Before this was
	// added, msg.ReplyTo arrived empty and the placeholder was
	// silently skipped — invisible bug (user sees no Typing card).
	if got.ReplyTo != "om_user_msg" {
		t.Errorf("ReplyTo = %q; want om_user_msg (F-44 Typing placeholder anchor)", got.ReplyTo)
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
	gw.OnMessageState("oc_unknown", "om_msg", agent.MessageQueued)
	if len(ch.sends) != 0 {
		t.Errorf("got %d sends; want 0 (no channel registered)", len(ch.sends))
	}
}

// TestOnMessageState_EmptyIDsDrops verifies that empty chatID or
// userMsgID is a silent drop (defensive against malformed events).
func TestOnMessageState_EmptyIDsDrops(t *testing.T) {
	gw, ch := newWiredRouter(t)
	gw.OnMessageState("", "om_msg", agent.MessageQueued)
	gw.OnMessageState("oc_chat", "", agent.MessageQueued)
	if len(ch.sends) != 0 {
		t.Errorf("got %d sends; want 0 (empty chat/user ID)", len(ch.sends))
	}
}

// TestOnMessageState_AllStatesPassThrough verifies that each
// MessageState value passes through to Channel.Send unchanged.
//
// F-53: only 3 states now (Queued / Submitted / Dropped). Done /
// Failed no longer exist on the abstract layer.
func TestOnMessageState_AllStatesPassThrough(t *testing.T) {
	gw, ch := newWiredRouter(t)
	states := []agent.MessageState{
		agent.MessageQueued,
		agent.MessageSubmitted,
		agent.MessageDropped,
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
		if got.MessageState == nil || got.MessageState.State != s {
			t.Errorf("sends[%d].MessageState.State = %v; want %v", i, got.MessageState.State, s)
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
	gw := New(nil).(*Router)
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
