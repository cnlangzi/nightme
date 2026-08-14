package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/gateway/inbound/teststubs"
	"github.com/cnlangzi/nightme/internal/gatewaytest"
	"github.com/cnlangzi/nightme/internal/messages"
)

// fakeChannel is a minimal Channel implementation used by
// gateway-level tests (kept local because the legacy shared
// definition lived in the deleted handlers_chatsession_test.go).
// Records every Send / SendCard so tests can assert on the
// resulting OutboundMessages.
type fakeChannel struct {
	mu    sync.Mutex
	sends []messages.OutboundMessage
}

func (c *fakeChannel) Name() string { return "fake" }
func (c *fakeChannel) Start(_ context.Context) error { return nil }
func (c *fakeChannel) Stop(_ context.Context) error { return nil }
func (c *fakeChannel) Incoming() <-chan messages.InboundMessage {
	return make(<-chan messages.InboundMessage)
}
func (c *fakeChannel) Send(_ context.Context, m messages.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, m)
	return nil
}
func (c *fakeChannel) SendCard(_ context.Context, m messages.OutboundMessage) (string, error) {
	_ = c.Send(context.Background(), m)
	c.mu.Lock()
	defer c.mu.Unlock()
	return "fake-card-" + strconv.Itoa(len(c.sends)), nil
}

// Channel-interface extensions (Phase 2.1 + 2.2). fakeChannel
// has no live state — all four are trivial fallbacks.
func (c *fakeChannel) OnPromptEnded(_ context.Context, _, _ string)        {}
func (c *fakeChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return "fake", json.RawMessage("{}"), nil
}
func (c *fakeChannel) SetLogger(_ *slog.Logger) {}
func (c *fakeChannel) BuildBlocks(text string, _ []messages.Attachment) []agent.ContentBlock {
	if text == "" {
		return nil
	}
	return []agent.ContentBlock{{Type: agent.ContentText, Text: text}}
}

// TestEmitMessageState_TranslatesToOutbound verifies that
// emitMessageState produces the right messages.OutboundMessage (Kind,
// messages.MessageStatePayload) and forwards it through the resolved
// channel's Send. After §1.4 cleanup, the message_id + state
// fields live in the typed messages.MessageStatePayload (not in Meta).
//
// F-54: emitMessageState now lives in message_state_helpers_test.go
// (test-only). The production path is the MessageStateBus
// subscriber in cmd/nightme/run.go which adds the F-48 stamp.
func TestEmitMessageState_TranslatesToOutbound(t *testing.T) {
	gw, ch := newWiredRouter(t)
	emitMessageState(gw, "oc_chat", "om_user_msg", agent.MessageQueued)

	if len(ch.sends) != 1 {
		t.Fatalf("got %d sends; want 1", len(ch.sends))
	}
	got := ch.sends[0]
	if got.Kind != messages.OutMessageState {
		t.Errorf("Kind = %v; want messages.OutMessageState", got.Kind)
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

// TestEmitMessageState_NoChannelDrops verifies that emitMessageState
// is a silent drop when no channel is registered for the chat
// (per F-31 §9: never block caller, log warn).
func TestEmitMessageState_NoChannelDrops(t *testing.T) {
	gw, ch := newWiredRouter(t)
	// Clear defaultChannel fallback so unknown chatID has no path.
	gw.mu.Lock()
	gw.defaultChannel = nil
	gw.mu.Unlock()
	emitMessageState(gw, "oc_unknown", "om_msg", agent.MessageQueued)
	if len(ch.sends) != 0 {
		t.Errorf("got %d sends; want 0 (no channel registered)", len(ch.sends))
	}
}

// TestEmitMessageState_EmptyIDsDrops verifies that empty chatID or
// userMsgID is a silent drop (defensive against malformed events).
func TestEmitMessageState_EmptyIDsDrops(t *testing.T) {
	gw, ch := newWiredRouter(t)
	emitMessageState(gw, "", "om_msg", agent.MessageQueued)
	emitMessageState(gw, "oc_chat", "", agent.MessageQueued)
	if len(ch.sends) != 0 {
		t.Errorf("got %d sends; want 0 (empty chat/user ID)", len(ch.sends))
	}
}

// TestEmitMessageState_AllStatesPassThrough verifies that each
// MessageState value passes through to Channel.Send unchanged.
//
// F-53: only 3 states now (Queued / Submitted / Dropped). Done /
// Failed no longer exist on the abstract layer.
func TestEmitMessageState_AllStatesPassThrough(t *testing.T) {
	gw, ch := newWiredRouter(t)
	states := []agent.MessageState{
		agent.MessageQueued,
		agent.MessageSubmitted,
		agent.MessageDropped,
	}
	for i, s := range states {
		emitMessageState(gw, "oc_chat", "om_"+string(rune('a'+i)), s)
	}
	if len(ch.sends) != len(states) {
		t.Fatalf("got %d sends; want %d", len(ch.sends), len(states))
	}
	for i, s := range states {
		got := ch.sends[i]
		if got.Kind != messages.OutMessageState {
			t.Errorf("sends[%d].Kind = %v; want messages.OutMessageState", i, got.Kind)
		}
		if got.MessageState == nil || got.MessageState.State != s {
			t.Errorf("sends[%d].MessageState.State = %v; want %v", i, got.MessageState.State, s)
		}
	}
}

// newWiredRouter builds a minimal Gateway with a single fakeChannel
// attached and chatToChan populated for "oc_chat". Returns the
// concrete *Router so the test can call package-internal helpers
// (emitMessageState, AttachChannels) directly.
//
// F-58: gateway.New requires an *inbound.Router. This test
// never reaches the dispatch chain (it calls emitMessageState
// directly), so we wire the teststubs no-op / fall-through
// stubs — the chain never claims, but it doesn't matter for
// the helper we're testing.
func newWiredRouter(t *testing.T) (*Router, *fakeChannel) {
	t.Helper()
	ch := &fakeChannel{}
	ir := inbound.New(
		teststubs.NewMessage(nil),                 // mgr=nil — chain never reaches GetOrCreate
		teststubs.AlwaysFallThrough{},              // commander: never claims
		teststubs.AlwaysFallThroughShell{},         // shell: never claims
		teststubs.NewReaction(false),               // reaction router: never claims
		&gatewaytest.NoopEmitter{},                 // F-59: emitter moved into inbound.Router
		"primary",
	)
	gw := New(ir, &gatewaytest.NoopEmitter{})
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
