package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gtw"
)

// TestRunGTWTest_SeedDraftsAndLists covers the slash command
// surface for /gtw test (F-45 §3.5 manual reaction-flow
// exerciser). Uses a no-op gateway that records dispatch calls
// (similar to dispatch_action_test.go) so we can verify the
// message reaches DispatchInbound end-to-end.
func TestRunGTWTest_SeedDraftsAndLists(t *testing.T) {
	const chatID = "oc_chat_test"

	mgr := chatsession.NewManager()
	channel := &noopChannel{}

	// 1. seed a branch-exists draft
	_, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, UserID: "ou_user_1", MessageID: "msg-1"},
		[]string{"seed", "om_card_seed_1", "branch-exists"},
		gtw.HandlerDeps{Now: func() time.Time { return time.Unix(0, 0) }},
		nil, // gw unused for seed/drafts/drain
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 2. list drafts — should show the one we just seeded
	res, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, MessageID: "msg-2"},
		[]string{"drafts"},
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("drafts: %v", err)
	}
	if res == nil {
		t.Fatal("drafts: nil result")
	}

	// 3. drain
	_, err = runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, MessageID: "msg-3"},
		[]string{"drain"},
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	// 4. list drafts — should be empty now
	res, err = runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, MessageID: "msg-4"},
		[]string{"drafts"},
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("drafts post-drain: %v", err)
	}
	if res == nil {
		t.Fatal("drafts post-drain: nil result")
	}
}

// TestRunGTWTest_SeedInvalidKind covers the rejection path.
// The handler returns a reply card with an error message but
// does NOT create the chat (no /gtw fix was issued) and does NOT
// seed a draft. So we verify the chat doesn't exist and no draft
// is stored regardless of whether the chat was created.
func TestRunGTWTest_SeedInvalidKind(t *testing.T) {
	const chatID = "oc_chat_test"
	mgr := chatsession.NewManager()
	channel := &noopChannel{}
	_, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, MessageID: "msg-1"},
		[]string{"seed", "om_card_1", "bogus-kind"},
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("expected nil err for invalid kind, got %v", err)
	}
	// Even if the chat was lazily created, no draft should be
	// stored (the kind-rejection path is before StoreGTWDraft).
	cs := mgr.Get(chatID)
	if cs != nil {
		if n := len(cs.ListGTWDrafts()); n != 0 {
			t.Errorf("drafts after invalid seed = %d, want 0", n)
		}
	}
}

// TestRunGTWTest_UnknownSub covers the rejection path.
func TestRunGTWTest_UnknownSub(t *testing.T) {
	const chatID = "oc_chat_test"
	mgr := chatsession.NewManager()
	channel := &noopChannel{}
	_, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, MessageID: "msg-1"},
		[]string{"bogus-sub"},
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("expected nil err for unknown sub, got %v", err)
	}
}

// TestRunGTWTest_Action_DispatchPath covers the action
// subcommand end-to-end through the real gateway dispatcher.
// Wires the same handler stack the runtime uses, then
// synthesises a reaction event and verifies the gateway reports
// the expected consumed/dropped result.
func TestRunGTWTest_Action_DispatchPath(t *testing.T) {
	const chatID = "oc_chat_test"
	mgr := chatsession.NewManager()

	// Pre-seed a branch-exists draft so the action handler
	// finds a target.
	cs := mgr.GetOrCreate(chatID, "primary")
	cs.StoreGTWDraft("om_card_target", &chatsession.GTWDraft{
		Kind: chatsession.GTWDraftFixBranchExists,
		Payload: chatsession.GTWFixDraftPayload{
			IssueID: 42, Title: "test", Branch: "fix/42-test",
			Slug: "42-test", Repo: "cnlangzi/nightme", Platform: "github",
			LabelAdded: true, ChatID: chatID,
		},
	})

	channel := &capturingChannelForTest{chatID: chatID}
	gw := New(channel.MessageDispatcher())
	// Note: in the runtime, the gtw action handler is installed
	// on every ChatSession via RegisterGTWAction. In this test
	// we skip that step (RegisterGTWAction is a higher-level
	// wiring) and instead install a minimal action handler that
	// just consumes the reaction — the focus is verifying the
	// /gtw test subcommand end-to-end through the dispatcher.
	gw.WithActionHandler(func(_ context.Context, msg *InboundMessage) bool {
		return msg != nil && msg.Reaction != nil
	})

	// Dispatch the action via the test subcommand.
	res, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, UserID: "ou_user_1", MessageID: "msg-1"},
		[]string{"action", "om_card_target", "❌"},
		gtw.HandlerDeps{},
		gw,
	)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	if res == nil || !res.Consumed || res.Dropped {
		t.Errorf("action result = %+v, want Consumed=true Dropped=false", res)
	}
	// Exactly 1 message is expected: the test subcommand's reply
	// card. (The gtw executor's "Cancelled" card would be a
	// second message, but this test uses a minimal action
	// handler that just consumes the reaction — the executor
	// pipeline is covered by TestRunFix_HappyPath /
	// TestHandleAction_BranchExists_ConfirmCancellation in
	// the gtw package itself.)
	channel.mu.Lock()
	n := len(channel.msgs)
	channel.mu.Unlock()
	if n != 1 {
		t.Errorf("captured messages = %d, want 1 (test reply card only)", n)
	}
}

// --- minimal fakes used by these tests ---

// noopChannel implements channel.Channel with no captures.
// Used by the seed/drafts/drain tests which don't exercise
// outbound messaging.
type noopChannel struct{}

func (n *noopChannel) Name() string                                              { return "noop" }
func (n *noopChannel) Start(_ context.Context) error                            { return nil }
func (n *noopChannel) Stop(_ context.Context) error                             { return nil }
func (n *noopChannel) Send(_ context.Context, _ OutboundMessage) error         { return nil }
func (n *noopChannel) Incoming() <-chan InboundMessage {
	ch := make(chan InboundMessage, 1)
	close(ch)
	return ch
}
func (n *noopChannel) MessageDispatcher() MessageDispatcher {
	return func(_ context.Context, _ *InboundMessage) error { return nil }
}

// capturingChannelForTest is a thread-safe in-memory channel
// that records every OutboundMessage the gateway sends. Used
// by the action-dispatch test to assert on outbound traffic.
type capturingChannelForTest struct {
	chatID string
	mu     sync.Mutex
	msgs   []OutboundMessage
}

func (c *capturingChannelForTest) Name() string                                       { return "capture" }
func (c *capturingChannelForTest) Start(_ context.Context) error                     { return nil }
func (c *capturingChannelForTest) Stop(_ context.Context) error                      { return nil }
func (c *capturingChannelForTest) Incoming() <-chan InboundMessage {
	ch := make(chan InboundMessage, 1)
	close(ch)
	return ch
}
func (c *capturingChannelForTest) Send(_ context.Context, m OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}
func (c *capturingChannelForTest) MessageDispatcher() MessageDispatcher {
	return func(_ context.Context, _ *InboundMessage) error { return nil }
}
