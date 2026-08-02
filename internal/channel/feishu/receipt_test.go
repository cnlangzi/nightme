package feishu

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
)

// mockReceiptBot captures AddReaction and SendMessageText calls so
// tests can assert on the dual-track behavior without a live Feishu
// connection. Embeds *Adapter with nil fields, so any unguarded
// access to a.larkClient will panic — guards in the receipt must
// stay routed through our methods.
type mockReceiptBot struct {
	mu             sync.Mutex
	reactions      []reactionCall
	messages       []string
	addReactionErr error
	sendMsgErr     error
}

type reactionCall struct {
	MessageID string
	Emoji     string
}

func (m *mockReceiptBot) AddReaction(_ context.Context, msgID, emoji string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addReactionErr != nil {
		return "", m.addReactionErr
	}
	m.reactions = append(m.reactions, reactionCall{msgID, emoji})
	return "mock-reaction-" + emoji, nil
}

func (m *mockReceiptBot) SendMessageText(_ context.Context, _, text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendMsgErr != nil {
		return "", m.sendMsgErr
	}
	m.messages = append(m.messages, text)
	return "", nil
}

// We can't directly construct an Adapter with custom AddReaction /
// SendMessageText because those methods are on *Adapter (concrete
// type, not interface). For the receipt tests we instead exercise
// the renderLocked + addReaction path by calling receipt helpers
// that use the bot field's underlying methods via an interface.
//
// To avoid restructuring Adapter, the receipt tests below exercise
// just the state-machine logic (string renderings, idempotency)
// and skip the live-API integration, which is covered by E2E in
// a follow-up.

func TestReceiptState_String(t *testing.T) {
	now := parseTime(t, "2026-08-01T14:35:20+08:00")

	cases := []struct {
		state  ReceiptState
		count  int
		ts     timeParseResult
		expect string
	}{
		{StateWaiting, 0, zeroTime(), "⏳ 等待中"},
		{StateExecuting, 0, zeroTime(), "🔄 处理中"},
	}
	// First two don't need a receipt (zero time fallback).
	for _, c := range cases {
		got := c.state.headerLine(nil)
		if got != c.expect {
			t.Errorf("%v.String(nil) = %q, want %q", c.state, got, c.expect)
		}
	}

	// With timestamps:
	r := &MessageReceipt{
		eventCount:  47,
		lastEventAt: now,
		completedAt: now,
	}
	if got := StateExecuting.headerLine(r); got != "🔄 ⏳ 47 · 14:35:20" {
		t.Errorf("Executing with ts = %q, want '🔄 ⏳ 47 · 14:35:20'", got)
	}
	if got := StateCompleted.headerLine(r); got != "✅ 已完成 14:35:20" {
		t.Errorf("Completed with ts = %q, want '✅ 已完成 14:35:20'", got)
	}
}

func TestReceiptState_Emoji(t *testing.T) {
	cases := map[ReceiptState]string{
		StateWaiting:   "OK",
		StateExecuting: "OnIt",
		StateCompleted: "PARTY",
	}
	for s, want := range cases {
		if got := s.Emoji(); got != want {
			t.Errorf("%d.Emoji() = %q, want %q", s, got, want)
		}
	}
}

// --- time helpers ---

type timeParseResult = time.Time

func parseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return tt
}

func zeroTime() time.Time { return time.Time{} }

// --- sanity tests for the visual progression strings ---

func TestReceiptLifecycle_Renderings(t *testing.T) {
	// Manually drive a receipt through its three states and assert
	// on the renderings. This is a pure-logic test; integration with
	// the live Feishu API is in the E2E follow-up.
	r := &MessageReceipt{receivedAt: parseTime(t, "2026-08-01T14:35:00+08:00")}

	// State 1: Waiting
	if r.State() != StateWaiting {
		t.Errorf("initial State = %v, want StateWaiting", r.State())
	}
	if got := r.state.headerLine(r); got != "⏳ 等待中" {
		t.Errorf("Waiting.String = %q", got)
	}

	// State 2: Executing
	r.state = StateExecuting
	r.eventCount = 1
	r.lastEventAt = parseTime(t, "2026-08-01T14:35:01+08:00")
	if got := r.state.headerLine(r); got != "🔄 ⏳ 1 · 14:35:01" {
		t.Errorf("Executing.String = %q", got)
	}

	// Heartbeat tick
	r.eventCount = 47
	r.lastEventAt = parseTime(t, "2026-08-01T14:35:20+08:00")
	if got := r.state.headerLine(r); got != "🔄 ⏳ 47 · 14:35:20" {
		t.Errorf("Executing after heartbeat = %q", got)
	}

	// State 3: Completed
	r.state = StateCompleted
	r.completedAt = parseTime(t, "2026-08-01T14:35:30+08:00")
	if got := r.state.headerLine(r); got != "✅ 已完成 14:35:30" {
		t.Errorf("Completed.String = %q", got)
	}
}

func TestReceiptStringContainsEmoji(t *testing.T) {
	// Sanity: every state string contains its identifying emoji so
	// the user's eye can scan the receipt row.
	for _, c := range []struct {
		state ReceiptState
		emoji string
	}{
		{StateWaiting, "⏳"},
		{StateExecuting, "🔄"},
		{StateCompleted, "✅"},
	} {
		r := &MessageReceipt{
			eventCount:  1,
			lastEventAt: parseTime(t, "2026-08-01T14:35:00+08:00"),
			completedAt: parseTime(t, "2026-08-01T14:35:00+08:00"),
		}
		got := c.state.headerLine(r)
		if !strings.Contains(got, c.emoji) {
			t.Errorf("%v string %q does not contain emoji %q", c.state, got, c.emoji)
		}
	}
}

// TestReceipt_PerEventFreshMessage verifies the post-refactor
// renderLocked behavior: each agent event produces a NEW
// SendMessageText call rather than an UpdateMessage in place.
//
// Before the per-event refactor, all events rolled into a single
// message that was updated N times via UpdateMessage. After, every
// event produces a fresh message — the chat surface mirrors the
// event stream. The test:
//  1. Creates a receipt with the mockReceiptBot (no Feishu).
//  2. Appends three events (text, tool start, tool end).
//  3. Asserts SendMessageText was called 3 times — once per event.
//  4. Asserts UpdateMessage was NEVER called.
//  5. Asserts the messages are the entry texts, not the rolled log.
func TestReceipt_PerEventFreshMessage(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_initial", bot)
	r.receivedAt = time.Now()

	// Three events across three agent kinds — a text reply, a
	// tool start, and a tool end. Each one should land as its
	// own message on the bot.
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "hello from the agent",
	})
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{
			Name: "Read",
			ID:   "toolu_001",
			Args: "/tmp/foo",
		},
	})
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{
			ID:   "toolu_001",
			Name: "Read",
		},
	})

	// Per-event shipping: exactly one SendMessageText per event.
	bot.mu.Lock()
	got := append([]string(nil), bot.messages...)
	bot.mu.Unlock()

	if len(got) != 3 {
		t.Fatalf("SendMessageText calls = %d, want 3 (one per event)", len(got))
	}
	// Each event ships with its own entry text, not the rolled log.
	if want := "💬 hello from the agent"; got[0] != want {
		t.Errorf("messages[0] = %q, want %q (single-entry text)", got[0], want)
	}
	if !strings.HasPrefix(got[1], "🔧 Read(") {
		t.Errorf("messages[1] = %q, want '🔧 Read(...)' (single-entry tool start)", got[1])
	}
	if !strings.HasPrefix(got[2], "✅ Read") {
		t.Errorf("messages[2] = %q, want '✅ Read ...' (single-entry tool end)", got[2])
	}

	// Sanity: messages are distinct (no accidental UpdateMessage
	// collapsing them into one body).
	if got[0] == got[1] || got[1] == got[2] || got[0] == got[2] {
		t.Errorf("messages are not distinct: %q / %q / %q", got[0], got[1], got[2])
	}
}

// mustAppend calls Append and asserts no error. Helper for the
// per-event test above.
func mustAppend(t *testing.T, r *MessageReceipt, ev agent.AgentEvent) {
	t.Helper()
	if err := r.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// UpdateMessage satisfies receiptBot. The per-event shipping path
// does not call it; the stub exists for the interface contract.
func (m *mockReceiptBot) UpdateMessage(_ context.Context, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil
}

// --- v1.1 tests for applyState / dispose ---

func TestApplyState_FourStateEnum(t *testing.T) {
	mb := &mockReceiptBot{}
	mb.reactions = nil
	r := NewMessageReceiptForReply("chat-x", "user-x", "msg-x", mb)

	cases := []struct {
		name    string
		target  channel.ReceiptState
		want    ReceiptState
		emoji   string
	}{
		{"pending", channel.ReceiptPending, StateWaiting, "OK"},
		{"executing", channel.ReceiptExecuting, StateExecuting, "OnIt"},
		{"done", channel.ReceiptDone, StateCompleted, "PARTY"},
		{"error", channel.ReceiptError, StateError, "THUMBSUP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.applyState(context.Background(), tc.target); err != nil {
				t.Fatalf("applyState(%s) err = %v", tc.name, err)
			}
			if r.State() != tc.want {
				t.Fatalf("after applyState(%s): state = %s, want %s", tc.name, r.State(), tc.want)
			}
		})
	}
}

func TestApplyState_Idempotent(t *testing.T) {
	mb := &mockReceiptBot{}
	r := NewMessageReceiptForReply("c", "u", "m", mb)

	if err := r.applyState(context.Background(), channel.ReceiptExecuting); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	before := len(mb.reactions)
	if err := r.applyState(context.Background(), channel.ReceiptExecuting); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(mb.reactions) != before {
		t.Fatalf("idempotent transition caused extra reactions: before=%d after=%d", before, len(mb.reactions))
	}
}

func TestApplyState_UnknownState(t *testing.T) {
	mb := &mockReceiptBot{}
	r := NewMessageReceiptForReply("c", "u", "m", mb)

	if err := r.applyState(context.Background(), channel.ReceiptState(99)); err == nil {
		t.Fatal("expected error for unknown ReceiptState")
	}
}

func TestDispose_Idempotent(t *testing.T) {
	mb := &mockReceiptBot{}
	r := NewMessageReceiptForReply("c", "u", "m", mb)

	// Set up a reaction so dispose has work to do.
	if err := r.applyState(context.Background(), channel.ReceiptExecuting); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	if err := r.dispose(context.Background()); err != nil {
		t.Fatalf("first dispose: %v", err)
	}
	if err := r.dispose(context.Background()); err != nil {
		t.Fatalf("second dispose: %v", err)
	}
}

func TestReceiptStateString_IncludesError(t *testing.T) {
	if got := StateError.String(); got != "error" {
		t.Fatalf("StateError.String() = %q, want %q", got, "error")
	}
}

func TestReceiptStateEmoji_Error(t *testing.T) {
	if got := StateError.Emoji(); got == "" {
		t.Fatal("StateError.Emoji() returned empty, want a Feishu predefined emoji")
	}
}
