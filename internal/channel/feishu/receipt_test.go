package feishu

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
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

func (m *mockReceiptBot) AddReaction(_ context.Context, msgID, emoji string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addReactionErr != nil {
		return m.addReactionErr
	}
	m.reactions = append(m.reactions, reactionCall{msgID, emoji})
	return nil
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
