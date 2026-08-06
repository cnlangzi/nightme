package gateway

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gtw"
)

// TestRunGTWTest_ModeYesNo asserts the debug/UAT setup path for
// /gtw test yes-no: seed draft + OutCard + click instructions.
// Full reaction/PATCH needs a real Feishu session — no auto-dispatch.
func TestRunGTWTest_ModeYesNo(t *testing.T) {
	const chatID = "oc_chat_test"

	mgr := chatsession.NewManager()
	channel := &capturingChannelForTest{chatID: chatID}

	res, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, UserID: "ou_user_1", MessageID: "msg-1"},
		[]string{"yes-no"},
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("yes-no: %v", err)
	}
	if res == nil {
		t.Fatal("yes-no: nil result")
	}

	channel.mu.Lock()
	defer channel.mu.Unlock()
	// Debug setup: OutCard + setup OutReply (click instructions).
	// No auto-dispatched "result:" summary — that is Feishu UAT.
	if !hasKind(channel.msgs, OutCard) {
		t.Errorf("yes-no: expected an OutCard decision preview")
	}
	summary := findTextByNeedles(channel.msgs, "yes-no", "click", "debug")
	if summary == "" {
		t.Errorf("yes-no: no setup reply containing 'yes-no' + 'click' + 'debug'")
	}
	if cs := mgr.Get(chatID); cs == nil {
		t.Fatal("yes-no: chat not created")
	} else if n := cs.GTWDraftCount(); n == 0 {
		t.Errorf("yes-no: draft missing after setup (count=%d); auto-dispatch must not consume it", n)
	}
}

// TestRunGTWTest_ModeUnknown asserts debug setup for the unknown-
// emoji scenario: card + draft left in place for a real Feishu click.
func TestRunGTWTest_ModeUnknown(t *testing.T) {
	const chatID = "oc_chat_test"

	mgr := chatsession.NewManager()
	channel := &capturingChannelForTest{chatID: chatID}

	res, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, UserID: "ou_user_1", MessageID: "msg-1"},
		[]string{"unknown"},
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("unknown: %v", err)
	}
	if res == nil {
		t.Fatal("unknown: nil result")
	}

	channel.mu.Lock()
	defer channel.mu.Unlock()
	summary := findTextByNeedles(channel.msgs, "unknown", "click", "debug")
	if summary == "" {
		t.Errorf("unknown: no setup reply containing 'unknown' + 'click' + 'debug'")
	}
	if cs := mgr.Get(chatID); cs == nil {
		t.Fatal("unknown: chat not created")
	} else if n := cs.GTWDraftCount(); n == 0 {
		t.Errorf("unknown: draft missing after setup (count=%d)", n)
	}
}

// TestRunGTWTest_ModeOrphan covers the "no draft" debug setup:
// orphan scenario seeds nothing and only emits the setup reply.
func TestRunGTWTest_ModeOrphan(t *testing.T) {
	const chatID = "oc_chat_test"

	mgr := chatsession.NewManager()
	channel := &capturingChannelForTest{chatID: chatID}

	res, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, UserID: "ou_user_1", MessageID: "msg-1"},
		[]string{"orphan"},
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("orphan: %v", err)
	}
	if res == nil {
		t.Fatal("orphan: nil result")
	}

	channel.mu.Lock()
	defer channel.mu.Unlock()
	// Exactly 1 message: the debug setup reply. No draft → no card.
	if n := len(channel.msgs); n != 1 {
		t.Errorf("orphan: captured %d messages, want 1 (setup reply only)", n)
	}
	if findTextByNeedles(channel.msgs, "orphan", "debug") == "" {
		t.Errorf("orphan: setup reply missing 'orphan' + 'debug'")
	}
}

// TestRunGTWTest_ListAndReset covers the utility subcommands.
func TestRunGTWTest_ListAndReset(t *testing.T) {
	const chatID = "oc_chat_test"

	mgr := chatsession.NewManager()
	channel := &noopChannel{}

	// Seed a draft via the "yes-no" mode (which internally calls
	// gtwTestSeedDraft).
	if _, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, MessageID: "msg-1"},
		[]string{"yes-no"},
		gtw.HandlerDeps{},
		nil,
	); err != nil {
		t.Fatalf("yes-no seed: %v", err)
	}

	// /gtw test list — should show the seeded draft.
	if _, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, MessageID: "msg-2"},
		[]string{"list"},
		gtw.HandlerDeps{},
		nil,
	); err != nil {
		t.Fatalf("list: %v", err)
	}

	// /gtw test reset — should clear gtwContext.
	if _, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: chatID, MessageID: "msg-3"},
		[]string{"reset"},
		gtw.HandlerDeps{},
		nil,
	); err != nil {
		t.Fatalf("reset: %v", err)
	}

	if cs := mgr.Get(chatID); cs == nil {
		t.Fatal("chat not found after reset")
	}
}

// TestRunGTWTest_Help covers the catalogue (no args).
func TestRunGTWTest_Help(t *testing.T) {
	mgr := chatsession.NewManager()
	channel := &noopChannel{}

	res, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: "oc_chat", MessageID: "msg-1"},
		nil, // no args → show help
		gtw.HandlerDeps{},
		nil,
	)
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	if res == nil {
		t.Fatal("help: nil result")
	}
}

// TestRunGTWTest_UnknownMode covers the rejection path for an
// unrecognised mode name (falls through to help).
func TestRunGTWTest_UnknownMode(t *testing.T) {
	mgr := chatsession.NewManager()
	channel := &noopChannel{}

	if _, err := runGTWTest(
		context.Background(),
		mgr, channel,
		&InboundMessage{ChatID: "oc_chat", MessageID: "msg-1"},
		[]string{"bogus-mode"},
		gtw.HandlerDeps{},
		nil,
	); err != nil {
		t.Fatalf("unknown mode: %v", err)
	}
}

// findTextByNeedles returns the Text of the first captured
// OutboundMessage whose body contains every needle. Empty string if
// no match. Used by /gtw test debug-setup assertions (card + click
// instructions; no auto-dispatched result summary).
func findTextByNeedles(msgs []OutboundMessage, needles ...string) string {
	for _, m := range msgs {
		if containsAll(m.Text, needles...) {
			return m.Text
		}
	}
	return ""
}

func hasKind(msgs []OutboundMessage, kind OutboundKind) bool {
	for _, m := range msgs {
		if m.Kind == kind {
			return true
		}
	}
	return false
}

// containsAll is a tiny substring-presence helper.
func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}

// --- minimal fakes used by these tests ---

// noopChannel implements channel.Channel with no captures.
// Used by the help / list / reset tests which don't exercise
// outbound messaging.
type noopChannel struct{}

func (n *noopChannel) Name() string                                  { return "noop" }
func (n *noopChannel) Start(_ context.Context) error                { return nil }
func (n *noopChannel) Stop(_ context.Context) error                 { return nil }
func (n *noopChannel) Send(_ context.Context, _ OutboundMessage) error { return nil }
func (n *noopChannel) SendCard(_ context.Context, _ OutboundMessage) (string, error) { return "", nil }
func (n *noopChannel) Incoming() <-chan InboundMessage {
	ch := make(chan InboundMessage, 1)
	close(ch)
	return ch
}
func (n *noopChannel) MessageDispatcher() MessageDispatcher {
	return func(_ context.Context, _ *InboundMessage) error { return nil }
}

// capturingChannelForTest is a thread-safe in-memory channel
// that records every OutboundMessage the gateway sends.
type capturingChannelForTest struct {
	chatID string
	mu     sync.Mutex
	msgs   []OutboundMessage
}

func (c *capturingChannelForTest) Name() string                     { return "capture" }
func (c *capturingChannelForTest) Start(_ context.Context) error   { return nil }
func (c *capturingChannelForTest) Stop(_ context.Context) error    { return nil }
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

func (c *capturingChannelForTest) SendCard(_ context.Context, m OutboundMessage) (string, error) {
	_ = c.Send(context.Background(), m)
	c.mu.Lock()
	defer c.mu.Unlock()
	return "capture-test-card-" + strconv.Itoa(len(c.msgs)), nil
}
func (c *capturingChannelForTest) MessageDispatcher() MessageDispatcher {
	return func(_ context.Context, _ *InboundMessage) error { return nil }
}
