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

// mockReceiptBot captures AddReaction, SendMessageText, ReplyMessage,
// and UpdateMessage calls so tests can assert on the dual-track
// behavior without a live Feishu connection. Embeds *Adapter with nil
// fields, so any unguarded access to a.larkClient will panic — guards
// in the receipt must stay routed through our methods.
type mockReceiptBot struct {
	mu             sync.Mutex
	reactions      []reactionCall
	messages       []string
	replies        []replyCall
	updates        []updateCall
	addReactionErr error
	sendMsgErr     error
	replyErr       error
	updateErr      error
}

type replyCall struct {
	ChatID    string
	UserMsgID string
	Text      string
}

type updateCall struct {
	MessageID string
	Text      string
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

func (m *mockReceiptBot) ReplyMessage(_ context.Context, chatID, userMsgID, text string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.replyErr != nil {
		return "", m.replyErr
	}
	m.replies = append(m.replies, replyCall{ChatID: chatID, UserMsgID: userMsgID, Text: text})
	return "mock-reply-msg-id", nil
}

func (m *mockReceiptBot) UpdateMessage(_ context.Context, msgID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, updateCall{MessageID: msgID, Text: text})
	return m.updateErr
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

// TestReceipt_RollingLogInPlaceUpdate verifies the canonical F-25
// v1.1 behavior: one user message → ONE Feishu reply card, edited
// in place via UpdateMessage as agent events append to the rolling
// log. The pre-fix design (commit dd91e44) shipped one fresh
// message per event; the canonical design collapses them all into
// one card.
//
// The test:
//  1. Creates a receipt with the mockReceiptBot (no live Feishu).
//  2. Appends three events (text, tool start, tool end).
//  3. Asserts UpdateMessage was called once per event (the same
//     message id every time — the in-place edit).
//  4. Asserts SendMessageText / ReplyMessage were NEVER called by
//     Append (the initial post is the caller's responsibility, not
//     Append's).
//  5. Asserts the updated bodies grow — each edit carries the full
//     rolled log so far (header + every entry appended to date).
func TestReceipt_RollingLogInPlaceUpdate(t *testing.T) {
	bot := &mockReceiptBot{}
	// NewMessageReceiptForReply already sets replyMsgID = "om_initial",
	// so the first Append will UpdateMessage that message in place.
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_initial", bot)
	r.receivedAt = time.Now()

	// Three events across three agent kinds — a text reply, a
	// tool start, and a tool end. Each one rolls into the same
	// reply card via UpdateMessage.
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "hello from the agent",
	})
	// The receipt throttles UpdateMessage (Feishu per-message
	// quota is 5/sec — see renderLocked). Space the events out
	// past the cooldown so each one paints the body.
	time.Sleep(minBodyUpdateInterval + 10*time.Millisecond)
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{
			Name: "Read",
			ID:   "toolu_001",
			Args: "/tmp/foo",
		},
	})
	time.Sleep(minBodyUpdateInterval + 10*time.Millisecond)
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{
			ID:   "toolu_001",
			Name: "Read",
		},
	})

	// In-place edit: one UpdateMessage per event, all on the same
	// message id.
	bot.mu.Lock()
	updates := append([]updateCall(nil), bot.updates...)
	messages := append([]string(nil), bot.messages...)
	replies := append([]replyCall(nil), bot.replies...)
	bot.mu.Unlock()

	if len(updates) != 3 {
		t.Fatalf("UpdateMessage calls = %d, want 3 (one per event)", len(updates))
	}
	for i, u := range updates {
		if u.MessageID != "om_initial" {
			t.Errorf("updates[%d].MessageID = %q, want %q (in-place edit, same id)",
				i, u.MessageID, "om_initial")
		}
	}

	// Append never posts fresh messages or replies — that is the
	// caller's job (SendUserMessage / CreateReceipt).
	if len(messages) != 0 {
		t.Errorf("SendMessageText calls = %d, want 0 (Append edits, doesn't post)",
			len(messages))
	}
	if len(replies) != 0 {
		t.Errorf("ReplyMessage calls = %d, want 0 (Append edits, doesn't reply)",
			len(replies))
	}

	// Each updated body grows — last body is the longest, and every
	// body contains the header line + every entry appended up to
	// that point. We don't assert exact strings (the receipt
	// renderer owns that contract), only the growth pattern.
	for i := 1; i < len(updates); i++ {
		if len(updates[i].Text) < len(updates[i-1].Text) {
			t.Errorf("updates body shrank from %d bytes to %d bytes — entries should accumulate",
				len(updates[i-1].Text), len(updates[i].Text))
		}
	}

	// Sanity: the final body contains all three event entries.
	final := updates[len(updates)-1].Text
	for _, want := range []string{"💬 hello from the agent", "🔧 Read(", "✅ Read"} {
		if !strings.Contains(final, want) {
			t.Errorf("final body missing %q — full log should include every event", want)
		}
	}
}

// TestReceipt_FooterRenders verifies SetFooter renders the
// caller-supplied session-attribution line at the bottom of the
// reply body, separated by a horizontal rule from the rolling
// log entries. The receipt does NOT compose the footer — that is
// the adapter's job (nightme doesn't track tokens or session
// metadata itself; the caller forwards the agent's values).
func TestReceipt_FooterRenders(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_initial", bot)
	r.receivedAt = time.Now()

	r.SetFooter("Agent: claude | cwd: ~/code/nightme | tokens: 8.8K / 4.5K")

	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "body line",
	})

	bot.mu.Lock()
	updates := append([]updateCall(nil), bot.updates...)
	bot.mu.Unlock()

	if len(updates) == 0 {
		t.Fatal("no UpdateMessage calls; Append should have rendered the body")
	}
	body := updates[len(updates)-1].Text
	if !strings.Contains(body, "─────────") {
		t.Errorf("body missing footer separator ─────────\nbody = %q", body)
	}
	if !strings.Contains(body, "Agent: claude | cwd: ~/code/nightme | tokens: 8.8K / 4.5K") {
		t.Errorf("body missing footer text\nbody = %q", body)
	}
	// Footer must come AFTER every entry in the body.
	sep := strings.Index(body, "─────────")
	bodyEnd := strings.Index(body, "💬 body line")
	if sep < bodyEnd {
		t.Errorf("footer separator appeared at offset %d, before body line at offset %d (must come after entries)",
			sep, bodyEnd)
	}
}

// TestReceipt_FooterEmpty verifies that an unset footer produces
// no separator line — important so receipts without session
// attribution don't render a dangling rule.
func TestReceipt_FooterEmpty(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_initial", bot)
	r.receivedAt = time.Now()

	mustAppend(t, r, agent.AgentEvent{Kind: agent.EventText, Text: "body line"})

	bot.mu.Lock()
	updates := append([]updateCall(nil), bot.updates...)
	bot.mu.Unlock()

	if len(updates) == 0 {
		t.Fatal("no UpdateMessage calls; Append should have rendered the body")
	}
	body := updates[len(updates)-1].Text
	if strings.Contains(body, "─────────") {
		t.Errorf("body contains footer separator despite no footer set\nbody = %q", body)
	}
}

// TestReceipt_FooterReplace verifies that a later SetFooter call
// replaces the earlier footer wholesale — the receipt is a pure
// renderer, so the caller (adapter) is responsible for composing
// the latest values. The receipt does not merge or track history.
func TestReceipt_FooterReplace(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_initial", bot)
	r.receivedAt = time.Now()

	r.SetFooter("first footer")
	mustAppend(t, r, agent.AgentEvent{Kind: agent.EventText, Text: "first"})

	// The receipt throttles UpdateMessage (Feishu per-message
	// quota is 5/sec — see renderLocked) so the second footer
	// must wait out the cooldown before its body update lands.
	time.Sleep(minBodyUpdateInterval + 10*time.Millisecond)

	r.SetFooter("second footer")
	mustAppend(t, r, agent.AgentEvent{Kind: agent.EventText, Text: "second"})

	bot.mu.Lock()
	updates := append([]updateCall(nil), bot.updates...)
	bot.mu.Unlock()

	last := updates[len(updates)-1].Text
	if !strings.Contains(last, "second footer") {
		t.Errorf("footer not refreshed\nbody = %q", last)
	}
	if strings.Contains(last, "first footer") {
		t.Errorf("stale footer still rendering\nbody = %q", last)
	}
}

// mustAppend calls Append and asserts no error. Helper for the
// rolling-log test above.
func mustAppend(t *testing.T, r *MessageReceipt, ev agent.AgentEvent) {
	t.Helper()
	if err := r.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// --- v1.1 tests for applyState / dispose ---

func TestApplyState_FourStateEnum(t *testing.T) {
	mb := &mockReceiptBot{}
	mb.reactions = nil
	r := NewMessageReceiptForReply("chat-x", "user-x", "msg-x", mb)

	cases := []struct {
		name   string
		target channel.ReceiptState
		want   ReceiptState
		emoji  string
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

// TestReceipt_ThinkingAggregation covers the F-25 / F-26 design
// where consecutive EventText-with-thinking-prefix events from
// the same Claude Code turn are merged into a single LogEntry so
// Feishu's native code-block auto-collapse ("N 行代码 >" expand
// button) kicks in. formatLocked wraps the merged text in ```
// fences so the chat surface shows one collapsed thinking block
// rather than N separate 💭 lines.
func TestReceipt_ThinkingAggregation(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_initial", bot)
	r.receivedAt = time.Now()

	// Three consecutive thinking events from Claude Code. Append
	// should merge them into ONE LogEntry rather than three.
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] planning step 1",
	})
	// Wait out the body-update throttle so the next merge
	// triggers an UpdateMessage that the body assertion can
	// read. The entries state is correct regardless (verified
	// below), but the formatLocked body check needs the latest
	// write to have happened.
	time.Sleep(minBodyUpdateInterval + 10*time.Millisecond)
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] checking workspace",
	})
	time.Sleep(minBodyUpdateInterval + 10*time.Millisecond)
	mustAppend(t, r, agent.AgentEvent{
		Kind: agent.EventText,
		Text: "[思考] ready to answer",
	})

	// One LogEntry, not three. The merged text carries all
	// three thinking segments separated by a horizontal rule.
	r.mu.Lock()
	entryCount := len(r.entries)
	var merged string
	if entryCount > 0 {
		merged = r.entries[0].Text
	}
	r.mu.Unlock()
	if entryCount != 1 {
		t.Fatalf("entries = %d, want 1 (thinking aggregation collapsed 3 events into 1 entry)", entryCount)
	}
	for _, want := range []string{"planning step 1", "checking workspace", "ready to answer"} {
		if !strings.Contains(merged, want) {
			t.Errorf("merged thinking missing %q\ntext = %q", want, merged)
		}
	}
	if !strings.Contains(merged, "\n---\n") {
		t.Errorf("merged thinking should use --- separators\ntext = %q", merged)
	}

	// formatLocked wraps the thinking entry in ``` fences so
	// Feishu auto-collapses the block.
	bot.mu.Lock()
	updates := append([]updateCall(nil), bot.updates...)
	bot.mu.Unlock()
	if len(updates) == 0 {
		t.Fatal("no UpdateMessage calls; Append should have rendered the body")
	}
	last := updates[len(updates)-1].Text
	if !strings.Contains(last, "```\nplanning step 1\n---\nchecking workspace\n---\nready to answer\n```") {
		t.Errorf("body should wrap merged thinking in code-block fences\nbody = %q", last)
	}
}

// TestReceipt_ThinkingAndReply_NoMerge verifies the aggregation
// doesn't cross event boundaries: a thinking block followed by a
// reply text produces two separate LogEntries (the reply has a
// different Kind).
func TestReceipt_ThinkingAndReply_NoMerge(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_initial", bot)
	r.receivedAt = time.Now()

	mustAppend(t, r, agent.AgentEvent{Kind: agent.EventText, Text: "[思考] planning"})
	mustAppend(t, r, agent.AgentEvent{Kind: agent.EventText, Text: "Hi! How can I help?"})

	r.mu.Lock()
	entryCount := len(r.entries)
	kinds := make([]string, entryCount)
	for i, e := range r.entries {
		kinds[i] = e.Kind
	}
	r.mu.Unlock()
	if entryCount != 2 {
		t.Fatalf("entries = %d, want 2 (thinking + reply shouldn't merge)", entryCount)
	}
	if kinds[0] != "thinking" || kinds[1] != "reply" {
		t.Errorf("entry kinds = %v, want [thinking reply]", kinds)
	}
}
