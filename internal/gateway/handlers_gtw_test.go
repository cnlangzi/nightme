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

// TestRunGTWTest_ModeYesNo exercises the real /gtw test command
// (NOT the runGTWTestScenario helper) so the test guards the
// command-path argument forwarding. It picks the two-choice mode
// to cover the "yes/no" reaction shape.
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
		nil, // gw may be nil; runGTWTestScenario falls back to a stub gateway
	)
	if err != nil {
		t.Fatalf("yes-no: %v", err)
	}
	if res == nil {
		t.Fatal("yes-no: nil result")
	}

	channel.mu.Lock()
	defer channel.mu.Unlock()
	// /gtw test yes-no now emits TWO outbound messages: the
	// decision card preview (OutCard, no Text) and the result
	// summary (OutReply with "yes-no" + "result:"). Find the
	// text reply by scanning for the marker substrings.
	summary := findTextByNeedles(channel.msgs, "yes-no", "result:")
	if summary == "" {
		t.Errorf("yes-no: no captured text reply containing 'yes-no' and 'result:'")
	}

	if cs := mgr.Get(chatID); cs == nil {
		t.Fatal("yes-no: chat not created")
	}
}

// TestRunGTWTest_ModeUnknown covers the unknown-emoji case: the
// draft is left in place, no follow-up card is emitted, and the
// result string reflects the "unrecognised emoji" path.
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
	summary := findTextByNeedles(channel.msgs, "unknown", "result:")
	if summary == "" {
		t.Errorf("unknown: no captured text reply containing 'unknown' and 'result:'")
	}
	if cs := mgr.Get(chatID); cs == nil {
		t.Fatal("unknown: chat not created")
	}
	// F-46: the draft is now re-keyed under the bot's card message id
	// (the one returned by SendCard), so we walk the ChatSession's
	// draft set rather than looking up the original SetupUserMsgID.
	// The intent is "unknown emoji leaves draft in place" — verified
	// by count > 0 after the dispatch.
	if n := mgr.Get(chatID).GTWDraftCount(); n == 0 {
		t.Errorf("unknown: draft was taken (count=%d), want draft left in place for re-reaction", n)
	}
}

// TestRunGTWTest_ModeOrphan covers the "no draft" path: a
// reaction dispatched against a random msg_id that has no
// gtwDraft. The dispatcher should return Dropped=true and no
// follow-up card should be sent.
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
	// Exactly 1 message: the test reply card. The gtw executor
	// has no draft to act on, so no follow-up card is sent.
	if n := len(channel.msgs); n != 1 {
		t.Errorf("orphan: captured %d messages, want 1 (test reply only)", n)
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
// no match. Used by /gtw test tests now that pipeline modes emit an
// OutCard preview followed by the result text reply.
func findTextByNeedles(msgs []OutboundMessage, needles ...string) string {
	for _, m := range msgs {
		if containsAll(m.Text, needles...) {
			return m.Text
		}
	}
	return ""
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
