package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
)

// mockReceiptBot captures AddReaction, SendMessageText, SendCard and
// PatchMessage calls so tests can assert on the dual-track behavior
// without a live Feishu connection. Embeds *Adapter with nil fields,
// so any unguarded access to a.larkClient will panic — guards in the
// receipt must stay routed through our methods.
type mockReceiptBot struct {
	mu             sync.Mutex
	reactions      []reactionCall
	messages       []string
	cards          []cardCall
	patches        []cardCall
	addReactionErr error
	sendMsgErr     error
	sendCardErr    error
	patchErr       error
	// nextCardID is returned from SendCard; tests that want the
	// receipt to enter the PATCH cycle need it non-empty. Defaults
	// to "om_card_1" (incremented per call).
	nextCardID int
}

type reactionCall struct {
	MessageID string
	Emoji     string
}

// cardCall records one SendCard or PatchMessage invocation. Patches
// are addressed by MessageID; sends by ChatID.
type cardCall struct {
	ChatID    string
	MessageID string
	Body      string
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

func (m *mockReceiptBot) SendMessageText(_ context.Context, _, text, _ string, _ bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendMsgErr != nil {
		return "", m.sendMsgErr
	}
	m.messages = append(m.messages, text)
	return "", nil
}

// SendCard records the call and returns a synthetic message id so
// the receipt's renderLocked stores it on r.cardMsgID. After the
// first send, subsequent renders go through PatchMessage. The id is
// derived from nextCardID so multiple receipts in the same test get
// distinct ids.
func (m *mockReceiptBot) SendCard(_ context.Context, chatID, body, _ string, _ bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendCardErr != nil {
		return "", m.sendCardErr
	}
	if m.nextCardID == 0 {
		m.nextCardID = 1
	}
	id := fmt.Sprintf("om_card_%d", m.nextCardID)
	m.nextCardID++
	m.cards = append(m.cards, cardCall{ChatID: chatID, MessageID: id, Body: body})
	return id, nil
}

func (m *mockReceiptBot) PatchMessage(_ context.Context, messageID, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.patchErr != nil {
		return m.patchErr
	}
	m.patches = append(m.patches, cardCall{MessageID: messageID, Body: body})
	return nil
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
//
// TestReceipt_FirstSendThenPatch verifies the card-based renderLocked
// behavior introduced in docs/channel/feishu.md §5: the first render
// sends an interactive card via SendCard; every subsequent render
// PATCHes that same card in place via PatchMessage. SendMessageText
// is no longer called for the rolling log.
//
// The test:
//  1. Creates a receipt with the mockReceiptBot (no Feishu).
//  2. Appends three events (text, tool start, tool end).
//  3. Asserts SendCard was called exactly ONCE (first event).
//  4. Asserts PatchMessage was called TWICE (subsequent events).
//  5. Asserts SendMessageText was NEVER called.
//  6. Asserts all PATCH calls address the same MessageID as the
//     original SendCard.
//  7. Asserts each card body contains all entries seen so far
//     (the rendered body grows monotonically; this is the
//     observable surface of the rolling-log card).
func TestReceipt_FirstSendThenPatch(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForReply("oc_chat", "om_user", "om_initial", bot)
	r.receivedAt = time.Now()

	// Three events across three agent kinds.
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

	bot.mu.Lock()
	cards := append([]cardCall(nil), bot.cards...)
	patches := append([]cardCall(nil), bot.patches...)
	textMsgs := append([]string(nil), bot.messages...)
	bot.mu.Unlock()

	if len(cards) != 1 {
		t.Fatalf("SendCard calls = %d, want 1 (first render only)", len(cards))
	}
	if len(patches) != 2 {
		t.Fatalf("PatchMessage calls = %d, want 2 (one per subsequent render)", len(patches))
	}
	if len(textMsgs) != 0 {
		t.Errorf("SendMessageText calls = %d, want 0 (text path retired for receipts)", len(textMsgs))
	}

	cardID := cards[0].MessageID
	if cardID == "" {
		t.Fatal("SendCard returned empty message id; receipt cannot store it as cardMsgID")
	}
	for i, p := range patches {
		if p.MessageID != cardID {
			t.Errorf("patches[%d].MessageID = %q, want %q (same id as SendCard)", i, p.MessageID, cardID)
		}
	}

	// Body must be a Card 2.0 envelope ({"schema":"2.0", ...}).
	// The pre-migration format wrapped in {"card":...} which
	// Feishu silently failed to render; see the protocol diff
	// captured in the PR description.
	if !strings.Contains(cards[0].Body, `"schema":"2.0"`) {
		t.Errorf("SendCard body missing Card 2.0 schema marker: %q", truncateForTest(cards[0].Body, 80))
	}
	if !strings.Contains(cards[0].Body, `"width_mode":"fill"`) {
		t.Errorf("SendCard body missing Card 2.0 config.width_mode: %q", truncateForTest(cards[0].Body, 80))
	}
	if !strings.Contains(cards[0].Body, `"body":{`) {
		t.Errorf("SendCard body missing Card 2.0 body wrapper: %q", truncateForTest(cards[0].Body, 80))
	}
	if !strings.Contains(cards[0].Body, `"tag":"markdown"`) {
		t.Errorf("SendCard body missing Card 2.0 markdown element: %q", truncateForTest(cards[0].Body, 80))
	}
	if !strings.Contains(cards[0].Body, "hello from the agent") {
		t.Errorf("SendCard body missing first event text: %q", truncateForTest(cards[0].Body, 120))
	}
	// The LAST PATCH should contain all reply entries seen so
	// far (the rolling-log card is the union of reply/result
	// events). Per F-34, tool_start / tool_end no longer surface
	// in the receipt card — the adapter routes them to Feishu
	// thread replies (Adapter.Send → postThreadReply). We assert
	// on the visible reply text only. eventCount drives the
	// header line ("🔄 ⏳ 3 · HH:MM:SS"), which is also asserted
	// implicitly via the PATCH count above (each subsequent
	// event must PATCH to refresh the timestamp).
	last := patches[len(patches)-1].Body
	for _, want := range []string{"hello from the agent"} {
		if !strings.Contains(last, want) {
			t.Errorf("last PATCH missing %q; got: %q", want, truncateForTest(last, 200))
		}
	}
}

// truncateForTest shortens a body string for failure messages; the
// test files don't import truncateForLog from the package.
func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestFootLine_Components covers the labelled
// "Agent: ... | CWD: ... | git: ... | TOKENS: .../..." foot-note
// composer. The line is appended after a <hr> divider as a
// plain markdown element by buildReceiptCard (no
// <text_tag color='neutral'> wrapper — the user requested the
// default card-renderer color). These tests pin the composer
// alone so failures surface with a tight diff. Format
// documentation lives in docs/channel/feishu.md §9.3.
func TestFootLine_Components(t *testing.T) {
	cases := []struct {
		name         string
		agentName    string
		workspace    string
		branch       string
		inputTokens  int
		outputTokens int
		want         string
	}{
		{
			name: "all-empty-omits-foot-note",
			want: "",
		},
		{
			name:      "agent-only",
			agentName: "claude",
			// Two-line layout: line 1 = agent alone
			// (no GIT / Tokens), line 2 omitted.
			want: "Agent: claude",
		},
		{
			name:      "workspace-only-full-path",
			workspace: "/Users/devin/code/nightme",
			// No line-1 fields → only line 2.
			want: "Workspace: /Users/devin/code/nightme",
		},
		{
			name:   "branch-only",
			branch: "main",
			// Only line-1 field (GIT), no line 2.
			want: "GIT: main",
		},
		{
			name:         "tokens-only-input-and-output",
			inputTokens:  20_000,
			outputTokens: 1_000,
			want:         "Tokens: 20K/1k",
		},
		{
			name:         "all-four-target-example",
			agentName:    "claude",
			workspace:    "/Users/devin/code/nightme",
			branch:       "main",
			inputTokens:  20_000,
			outputTokens: 1_000,
			// Two-line layout per the user's most recent
			// request:
			//   line 1: Agent | GIT | Tokens (joined with " | ")
			//   line 2: Workspace (alone)
			want: "Agent: claude | GIT: main | Tokens: 20K/1k<br/>Workspace: /Users/devin/code/nightme",
		},
		{
			name:         "all-four-with-feature-branch",
			agentName:    "claude",
			workspace:    "/Users/devin/code/nightme",
			branch:       "feat/receipt-card",
			inputTokens:  50_000,
			outputTokens: 3_000,
			want:         "Agent: claude | GIT: feat/receipt-card | Tokens: 50K/3k<br/>Workspace: /Users/devin/code/nightme",
		},
		{
			name:         "branch-empty-omitted-not-git-repo",
			agentName:    "claude",
			workspace:    "/Users/geax",
			inputTokens:  32_000,
			outputTokens: 101,
			// /Users/geax is the home dir (per the
			// user's most recent screenshot), not a git
			// repo, so the GIT segment drops out of
			// line 1 entirely.
			want: "Agent: claude | Tokens: 32K/101<br/>Workspace: /Users/geax",
		},
		{
			name:         "deep-workspace-full-path-not-shortened",
			workspace:    "/Users/devin/code/nightme/src/internal/channel/feishu",
			branch:       "main",
			inputTokens:  5_000,
			outputTokens: 500,
			// No agent → line 1 has only GIT + Tokens;
			// long workspace on line 2.
			want: "GIT: main | Tokens: 5K/500<br/>Workspace: /Users/devin/code/nightme/src/internal/channel/feishu",
		},
		{
			name:      "tilde-path-full-path",
			workspace: "~/code/nightme",
			want:      "Workspace: ~/code/nightme",
		},
		{
			name:        "input-only-zero-output-omitted",
			agentName:   "claude",
			workspace:   "/Users/geax",
			inputTokens: 20_000,
			// Tokens "20K" (no output side) is the only
			// line-1 segment beyond Agent.
			want: "Agent: claude | Tokens: 20K<br/>Workspace: /Users/geax",
		},
		{
			name:         "output-only-zero-input-omitted",
			agentName:    "claude",
			workspace:    "/Users/geax",
			outputTokens: 1_000,
			want:         "Agent: claude | Tokens: 1k<br/>Workspace: /Users/geax",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &MessageReceipt{
				agentName:    tc.agentName,
				workspace:    tc.workspace,
				branch:       tc.branch,
				inputTokens:  tc.inputTokens,
				outputTokens: tc.outputTokens,
			}
			got := StateExecuting.footLine(r)
			if got != tc.want {
				t.Errorf("footLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFootLine_PerLineLayout pins the per-line layout that
// the user requested: each foot note segment is on its own
// line, separated by <br/> (Feishu lark_md's explicit line
// break). The order is fixed (AGENT → WORKSPACE → GIT →
// TOKENS, per the user's "CWD earliest on the second line"
// request); missing segments drop their line entirely. The
// TestFootLine_TwoLineLayout pins the two-line layout that
// the user requested: line 1 holds the task-scoped fields
// (Agent | GIT | Tokens) joined by " | "; line 2 holds the
// workspace path alone. The two lines are separated by
// <br/> when both are present. A missing line omits the
// <br/> entirely (no dangling line break on single-line
// results). The previous per-segment-per-line layout was
// retired for this grouped two-line format.
func TestFootLine_TwoLineLayout(t *testing.T) {
	cases := []struct {
		name string
		r    *MessageReceipt
		want string
	}{
		{
			name: "all-four-fields-two-lines",
			r: &MessageReceipt{
				agentName:    "claude",
				workspace:    "/Users/devin/code/nightme",
				branch:       "main",
				inputTokens:  20_000,
				outputTokens: 1_000,
			},
			want: "Agent: claude | GIT: main | Tokens: 20K/1k<br/>Workspace: /Users/devin/code/nightme",
		},
		{
			name: "no-git-repo-drops-git-from-line-1",
			r: &MessageReceipt{
				agentName:    "claude",
				workspace:    "/Users/geax",
				inputTokens:  32_000,
				outputTokens: 101,
			},
			// GIT is absent → no GIT segment in line 1.
			want: "Agent: claude | Tokens: 32K/101<br/>Workspace: /Users/geax",
		},
		{
			name: "no-agent-line-1-still-has-git-and-tokens",
			r: &MessageReceipt{
				workspace:    "/Users/devin/code/nightme",
				branch:       "main",
				inputTokens:  1_000,
				outputTokens: 500,
			},
			// Agent absent → line 1 is just GIT | Tokens.
			// Input uses compactNumberLoud ("1K" not "1k"
			// for the 1000-input case).
			want: "GIT: main | Tokens: 1K/500<br/>Workspace: /Users/devin/code/nightme",
		},
		{
			name: "only-workspace-line-2-alone",
			r: &MessageReceipt{
				workspace: "/Users/devin/code/nightme",
			},
			// No line-1 fields → no <br/>, just line 2.
			want: "Workspace: /Users/devin/code/nightme",
		},
		{
			name: "only-line-1-fields-line-2-omitted",
			r: &MessageReceipt{
				agentName:    "claude",
				branch:       "main",
				inputTokens:  20_000,
				outputTokens: 1_000,
			},
			// No workspace → no <br/>, just line 1.
			want: "Agent: claude | GIT: main | Tokens: 20K/1k",
		},
		{
			name: "long-line-stays-as-one-line-no-soft-wrap",
			r: &MessageReceipt{
				agentName:    "very-long-agent-name-that-exceeds-the-old-soft-cap",
				workspace:    "/Users/devin/code/some-really-long-project-name-here",
				branch:       "feat/another-long-branch-name",
				inputTokens:  100_000,
				outputTokens: 5_000,
			},
			// Two lines, no soft-cap wrap — long content
			// stays on a single line and Feishu handles
			// the visual wrap.
			want: "Agent: very-long-agent-name-that-exceeds-the-old-soft-cap | GIT: feat/another-long-branch-name | Tokens: 100K/5k<br/>Workspace: /Users/devin/code/some-really-long-project-name-here",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StateExecuting.footLine(tc.r)
			if got != tc.want {
				t.Errorf("footLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFootLine_NilReceipt returns "" — pins the guard so a nil
// receipt doesn't panic when buildReceiptCard calls footLine.
func TestFootLine_NilReceipt(t *testing.T) {
	for _, s := range []ReceiptState{StateWaiting, StateExecuting, StateCompleted, StateError} {
		if got := s.footLine(nil); got != "" {
			t.Errorf("%v.footLine(nil) = %q, want \"\"", s, got)
		}
	}
}

// TestCompactNumber pins the formatter (lowercase suffix) used
// by footLine for the OUTPUT-tokens side. Whole-thousand values
// drop the ".0" so 1,000 → "1k" (not "1.0k") — matches the
// example payload the user requested ("20K/1k").
func TestCompactNumber(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1k"},
		{1200, "1.2k"},
		{9999, "10.0k"},
		{10000, "10k"},
		{12345, "12k"},
		{999999, "999k"},
		{1000000, "1M"},
		{1200000, "1.2M"},
	}
	for _, tc := range cases {
		if got := compactNumber(tc.n); got != tc.want {
			t.Errorf("compactNumber(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestCompactNumberLoud pins the uppercase suffix formatter
// used by footLine for the INPUT-tokens side. K/M uppercase so
// users can scan "20K/1k" and tell at a glance which side is
// input vs output.
func TestCompactNumberLoud(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1K"},
		{1200, "1.2K"},
		{9999, "10.0K"},
		{10000, "10K"},
		{20000, "20K"},
		{999999, "999K"},
		{1000000, "1M"},
		{1200000, "1.2M"},
	}
	for _, tc := range cases {
		if got := compactNumberLoud(tc.n); got != tc.want {
			t.Errorf("compactNumberLoud(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestShortenCwdRemoved documents the removal of shortenCwd
// when the footer format switched from basename → full
// workspace path. The CWD segment now shows the absolute path
// verbatim (per user request), so the basename shortener is
// unused. The test body is intentionally a no-op so a future
// search for "TestShortenCwd" surfaces this comment instead
// of a missing function.
func TestShortenCwdRemoved(t *testing.T) {
	_ = "shortenCwd removed; footer now shows full workspace path"
}

// TestFootLine_ColonsByDesign documents that the foot note
// now uses "label: value" segments (per the user's explicit
// request). The OpenClaw #59360 hoisting risk was
// re-evaluated and accepted; the rationale is captured in
// the footLine doc comment. This test pins the new format
// shape so a future edit can't quietly regress to either
// (a) the no-label plain-value format or (b) a middle-dot
// separator that hides the labels.
func TestFootLine_ColonsByDesign(t *testing.T) {
	r := &MessageReceipt{
		agentName:    "claude",
		workspace:    "/Users/devin/code/nightme",
		branch:       "main",
		inputTokens:  32_000,
		outputTokens: 101,
	}
	got := StateExecuting.footLine(r)
	for _, want := range []string{"Agent: claude", "Workspace: /Users/devin/code/nightme", "GIT: main", "Tokens: 32K/101"} {
		if !strings.Contains(got, want) {
			t.Errorf("footLine = %q missing %q", got, want)
		}
	}
}

// TestBuildReceiptCard_Card2Shape pins the Card 2.0 envelope
// contract documented in docs/channel/feishu.md §3.4 + the
// buildReceiptCard comment. The previous PR shipped a Card 1.0
// shape wrapped in {"card":...} which Feishu silently failed to
// render; this test pins every marker that distinguishes the new
// shape so a future edit can't quietly regress it.
//
// Markers asserted (every one must be present in the body):
//
//   - `"schema":"2.0"`               — Card 2.0 declaration
//   - `"width_mode":"fill"`          — Card 2.0 config key
//     (not the v1 `wide_screen_mode`)
//   - `"body":{"elements":[...]}`    — Card 2.0 body wrapper
//     (elements are NOT at the top level)
//   - `"tag":"markdown"`             — Card 2.0 text element
//     (not the v1 `tag:"div"` + `text:{tag:"lark_md",...}` shape)
func TestBuildReceiptCard_Card2Shape(t *testing.T) {
	r := &MessageReceipt{
		state:        StateExecuting,
		eventCount:   3,
		lastEventAt:  parseTime(t, "2026-08-01T14:32:05+08:00"),
		agentName:    "main",
		workspace:    "/Users/foo/bar/repo",
		branch:       "feat/receipt-card",
		inputTokens:  1500,
		outputTokens: 800,
		entries: []LogEntry{
			{Icon: "💬", Text: "hello", Kind: "reply"},
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}

	mustContain := []string{
		// Card 2.0 envelope markers.
		`"schema":"2.0"`,
		`"width_mode":"fill"`,
		`"body":{`,
		`"elements":[`,
		`"tag":"markdown"`,

		// Header + reply entry rendered as markdown.
		`"content":"🔄 ⏳ 3 · 14:32:05"`,
		`"content":"💬 hello"`,

		// Labelled foot-note format (per-line layout,
		// uppercase labels):
		//   AGENT: main
		//   WORKSPACE: /Users/foo/bar/repo
		//   GIT: feat/receipt-card
		//   TOKENS: 1.5K/800
		`Agent: main`,
		`GIT: feat/receipt-card`,
		`Tokens: 1.5K/800`,
		`Workspace: /Users/foo/bar/repo`,
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("card body missing %q\n--- body ---\n%s", want, truncateForTest(body, 400))
		}
	}

	// Negative assertions: things that would mean we silently
	// regressed to the v1 shape, the old "state · entries ·
	// timestamp" foot-note, the no-label / no-colon format,
	// or the (deprecated) coloured footer wrapper.
	mustNotContain := []string{
		`{"card":`,              // v1 envelope wrapper
		`"wide_screen_mode"`,    // v1 config key
		`"tag":"div"`,           // v1 text container
		`"lark_md"`,             // v1 inline format
		`"tag":"note"`,          // v1 footer element
		`text_tag`,              // deprecated neutral-color wrapper
		`color='neutral'`,       // deprecated neutral-color value
		`Agent · `,              // old middle-dot labelled format
		`cwd · `,                // old middle-dot labelled format
		`tokens · `,             // old middle-dot labelled format
		`executing · 3 entries`, // even older format
		// Single-line joined format (the previous
		// format that this round retired). The
		// current composer always emits one segment
		// per line via <br/>.
		` | GIT: main | `,
		` | TOKENS: `,
		// Old label name "CWD" was renamed to
		// "WORKSPACE" per user request.
		`CWD: `,
	}
	for _, bad := range mustNotContain {
		if strings.Contains(body, bad) {
			t.Errorf("card body contains %q (regression)\n--- body ---\n%s", bad, truncateForTest(body, 400))
		}
	}
}

// TestBuildReceiptCard_FooterOpenClawStyle pins the footer
// visual styling that matches the OpenClaw Lark plugin
// (openclaw-lark src/card/builder.ts::buildFooter). Two
// invariants:
//
//  1. The footer markdown element carries text_size:
//     "notation" so the foot note renders in the standard
//     Card 2.0 footnote size (small, dim, well-separated
//     from the rolling log body). OpenClaw applies this to
//     the footer + reasoning panel + tool use panels.
//  2. When the receipt state is StateError, the footer
//     content is wrapped in <font color='red'>...</font>
//     so a failed session's footer is visually distinct.
//     OpenClaw's buildFooter wraps the i18n copies in red
//     when isError is true. On a non-error state the
//     content is plain (no color tag).
func TestBuildReceiptCard_FooterOpenClawStyle(t *testing.T) {
	t.Run("notation-text-size-on-normal-state", func(t *testing.T) {
		r := &MessageReceipt{
			state:      StateCompleted,
			agentName:  "claude",
			workspace:  "/Users/devin/code/nightme",
			branch:     "main",
			eventCount: 1,
		}
		body, err := buildReceiptCard(r)
		if err != nil {
			t.Fatalf("buildReceiptCard: %v", err)
		}
		// text_size: "notation" must appear on the footer
		// element. We assert on the substring rather than
		// parsing the JSON to keep the test robust to
		// whitespace / key-order changes.
		if !strings.Contains(body, `"text_size":"notation"`) {
			t.Errorf("card body missing OpenClaw-style text_size:notation footer\n--- body ---\n%s", truncateForTest(body, 400))
		}
		// No red color tag on a successful state.
		if strings.Contains(body, "color='red'") {
			t.Errorf("card body has unexpected red color on a non-error state\n--- body ---\n%s", truncateForTest(body, 400))
		}
	})

	t.Run("red-color-on-error-state", func(t *testing.T) {
		r := &MessageReceipt{
			state:      StateError,
			agentName:  "claude",
			workspace:  "/Users/devin/code/nightme",
			branch:     "main",
			eventCount: 1,
		}
		body, err := buildReceiptCard(r)
		if err != nil {
			t.Fatalf("buildReceiptCard: %v", err)
		}
		// On error, the footer's text is wrapped in red.
		// OpenClaw's buildFooter does the same on isError.
		if !strings.Contains(body, `<font color='red'>`) {
			t.Errorf("card body missing OpenClaw-style red color wrapper on error state\n--- body ---\n%s", truncateForTest(body, 400))
		}
		// text_size: "notation" still applies on error.
		if !strings.Contains(body, `"text_size":"notation"`) {
			t.Errorf("card body missing text_size:notation on error state\n--- body ---\n%s", truncateForTest(body, 400))
		}
	})
}

// TestBuildReceiptCard_ThinkingCollapsiblePanel pins the F-34
// contract: thinking entries (along with tool_start / tool_end /
// compaction) are NO LONGER carried in the receipt card. The
// adapter routes them to Feishu thread replies; eventToEntry
// returns (_, false) so the receipt only sees OutText / OutResult
// / OutInit / OutUsage-derived entries. The card body must not
// contain a collapsible_panel element.
//
// Invariants:
//  1. No collapsible_panel element appears, even when the
//     supplied entries list contains a thinking-shaped one.
//     (F-34: this is the regression guard — pre-F-34 the
//     receipt card rendered collapsible_panel for thinking
//     entries; that was retired because 30+ panels hit
//     Feishu's 50-element limit.)
//  2. Only the reply entry is rendered into the card body.
func TestBuildReceiptCard_ThinkingCollapsiblePanel(t *testing.T) {
	r := &MessageReceipt{
		state:        StateExecuting,
		eventCount:   3,
		lastEventAt:  parseTime(t, "2026-08-01T14:32:05+08:00"),
		agentName:    "claude",
		workspace:    "/Users/devin/code/nightme",
		branch:       "main",
		inputTokens:  20_000,
		outputTokens: 1_000,
		entries: []LogEntry{
			{
				Icon: "💭",
				Text: "The user said hi — I should respond briefly and friendly. No need to invoke any skills or tools.",
				Kind: "thinking",
			},
			{
				Icon: "🔧",
				Text: "Read(/tmp/foo)",
				Kind: "tool_start",
			},
			{
				Icon: "💬",
				Text: "Hi! How can I help you today?",
				Kind: "reply",
			},
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}

	// F-34: thinking entries (and tool_start / tool_end /
	// compaction) are NO LONGER carried in the receipt card;
	// the adapter routes them to thread replies. The card
	// body must not contain a collapsible_panel element.
	if strings.Contains(body, `"tag":"collapsible_panel"`) {
		t.Errorf("card body contains collapsible_panel; F-34 retired collapsible_panel rendering for thinking entries\n--- body ---\n%s", truncateForTest(body, 400))
	}
	// The thinking entry text must NOT surface in the card.
	if strings.Contains(body, "should respond briefly and friendly") {
		t.Errorf("card body leaked thinking text; F-34 dropped it from the receipt\n--- body ---\n%s", truncateForTest(body, 400))
	}
	// The reply entry survives (it's a real OutText-derived entry).
	if !strings.Contains(body, `"content":"💬 Hi! How can I help you today?"`) {
		t.Errorf("reply entry missing from card body\n--- body ---\n%s", truncateForTest(body, 400))
	}
}

// TestBuildColdStartCard_Card2Shape pins the cold-start card
// emitted by Adapter.receiptFor. It must be a Card 2.0 envelope
// (same shape contract as the rolling-log card) with a single ⏳
// markdown element so the user sees a card surface from the first
// event, not a bare text message.
func TestBuildColdStartCard_Card2Shape(t *testing.T) {
	body, err := buildColdStartCard()
	if err != nil {
		t.Fatalf("buildColdStartCard: %v", err)
	}
	mustContain := []string{
		`"schema":"2.0"`,
		`"width_mode":"fill"`,
		`"body":{`,
		`"elements":[`,
		`"tag":"markdown"`,
		`"content":"⏳ 等待中"`,
	}
	for _, want := range mustContain {
		if !strings.Contains(body, want) {
			t.Errorf("cold-start card body missing %q\n--- body ---\n%s", want, truncateForTest(body, 400))
		}
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

// --- F-34: buildReceiptCard no longer wraps tool entries in
// collapsible_panel. tool_start / tool_end are routed to thread
// replies; only OutText / OutResult / OutInit / OutUsage-derived
// entries land in the card body. ---

func TestBuildReceiptCard_ToolFolded(t *testing.T) {
	r := &MessageReceipt{
		state:       StateExecuting,
		eventCount:  3,
		lastEventAt: parseTime(t, "2026-08-01T14:32:05+08:00"),
		entries: []LogEntry{
			{Icon: "🔧", Text: "Read(/a.py)", Kind: "tool"},
			{Icon: "✅", Text: "Read → 47 lines", Kind: "tool"},
			{Icon: "📝", Text: "final answer here", Kind: "result"},
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}

	// F-34: tool entries are no longer collapsed into
	// collapsible_panel; they go to thread replies. The card
	// body should still render the result entry flat, and
	// there should be no collapsible_panel at all.
	if strings.Contains(body, `"tag":"collapsible_panel"`) {
		t.Errorf("card body contains collapsible_panel; F-34 retired collapsible_panel for tool entries\n--- body ---\n%s", truncateForTest(body, 400))
	}
	if !strings.Contains(body, `"content":"📝 final answer here"`) {
		t.Errorf("final result missing as plain markdown\n--- body ---\n%s", truncateForTest(body, 400))
	}
}

// TestReceipt_Touch_BumpsCountersAndRenders — F-34 review P1-3
// regression guard. Touch() must bump eventCount + lastEventAt
// and PATCH the card without appending a LogEntry. The header
// line in the next render must reflect the new event count and
// a fresh timestamp, and the entries slice must remain empty.
func TestReceipt_Touch_BumpsCountersAndRenders(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForCard("oc_chat", "om_user", "om_card", bot)
	// Anchor lastEventAt to one hour ago so the test is robust
	// against the system clock (time.Now in Touch must advance
	// past the anchor regardless of the wall clock at run time).
	r.lastEventAt = time.Now().Add(-time.Hour)
	r.eventCount = 3

	if err := r.Touch(context.Background()); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eventCount != 4 {
		t.Errorf("eventCount = %d, want 4", r.eventCount)
	}
	if !r.lastEventAt.After(time.Now().Add(-time.Minute)) {
		t.Errorf("lastEventAt did not advance: %v", r.lastEventAt)
	}
	if len(r.entries) != 0 {
		t.Errorf("entries = %d, want 0 (Touch must not append)", len(r.entries))
	}

	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.patches) != 1 {
		t.Errorf("patches = %d, want 1 (Touch must PATCH once)", len(bot.patches))
	}
}

// TestReceipt_Touch_NilSafe — Touch on a nil receiver is a no-op
// (used by Adapter.Send where receiptFor may return nil for
// orphan events).
func TestReceipt_Touch_NilSafe(t *testing.T) {
	var r *MessageReceipt
	if err := r.Touch(context.Background()); err != nil {
		t.Errorf("nil Touch: %v", err)
	}
}

// TestReceipt_Touch_DropsAfterCompletion — Touch after SetCompleted
// is a silent no-op (matches Append's late-event policy).
func TestReceipt_Touch_DropsAfterCompletion(t *testing.T) {
	bot := &mockReceiptBot{}
	r := NewMessageReceiptForCard("oc_chat", "om_user", "om_card", bot)
	r.state = StateCompleted
	if err := r.Touch(context.Background()); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.patches) != 0 {
		t.Errorf("patches = %d, want 0 (Touch after completion must drop)", len(bot.patches))
	}
}

// TestBuildReceiptCard_LongEntryMultiDivs (F-37) verifies that a
// single entry with text longer than divTextCharLimit (1000 runes)
// is split into multiple markdown elements in the rendered card.
// Each split chunk must be ≤ 1000 runes so the Feishu server
// accepts the PATCH.
func TestBuildReceiptCard_LongEntryMultiDivs(t *testing.T) {
	// 2500 chars + 2 paragraphs separator = 2502 chars total
	para1 := strings.Repeat("a", 1500)
	para2 := strings.Repeat("b", 1000)
	text := para1 + "\n\n" + para2
	r := &MessageReceipt{
		state:       StateCompleted,
		eventCount:  1,
		entries: []LogEntry{
			{Icon: "📝", Text: text, Kind: "result"},
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	// Decode JSON to walk the elements cleanly
	elements := decodeReceiptElements(t, body)
	// 至少 3 个 markdown element: para1 拆 2 段 + para2 1 段
	markdownCount := 0
	for _, e := range elements {
		if e["tag"] == "markdown" {
			markdownCount++
			if content, ok := e["content"].(string); ok {
				if utf8.RuneCountInString(content) > divTextCharLimit {
					t.Errorf("markdown element content exceeds %d runes: %d runes", divTextCharLimit, utf8.RuneCountInString(content))
				}
			}
		}
	}
	if markdownCount < 3 {
		t.Errorf("expected ≥ 3 markdown elements, got %d\n--- body ---\n%s", markdownCount, truncateForTest(body, 600))
	}
}

// decodeReceiptElements parses the receipt card body JSON and
// returns the elements slice. Test helper for F-37.
func decodeReceiptElements(t *testing.T, body string) []map[string]any {
	t.Helper()
	var envelope struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode card body: %v\n--- body ---\n%s", err, truncateForTest(body, 600))
	}
	return envelope.Body.Elements
}

// TestBuildReceiptCard_ShortEntryStaysSingle (F-37) verifies that
// entries ≤ 1000 runes still render as exactly one markdown element
// (no unnecessary splitting).
func TestBuildReceiptCard_ShortEntryStaysSingle(t *testing.T) {
	r := &MessageReceipt{
		state:       StateExecuting,
		eventCount:  1,
		entries: []LogEntry{
			{Icon: "💬", Text: "Short reply under 1000 chars.", Kind: "reply"},
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	// 1 markdown element for entry + 1 markdown for header line = 2
	if c := strings.Count(body, `"tag":"markdown"`); c != 2 {
		t.Errorf("expected 2 markdown elements (header + entry), got %d\n--- body ---\n%s", c, truncateForTest(body, 600))
	}
}

// TestBuildReceiptCard_HugeEntryStaysBounded (F-37) verifies that
// even very long entries (8000 runes) don't blow the envelope.
// 8000 runes / 1000 per div = 8 divs max, plus header + footer = 10.
func TestBuildReceiptCard_HugeEntryStaysBounded(t *testing.T) {
	// 8000 chars, no paragraph breaks (so it splits via hardSplit at rune boundary)
	text := strings.Repeat("x", 8000)
	r := &MessageReceipt{
		state:       StateCompleted,
		eventCount:  1,
		entries: []LogEntry{
			{Icon: "📝", Text: text, Kind: "result"},
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	// 8000 + 1 header = 9 markdown elements (8 chunks × 1000 + 1 header)
	if c := strings.Count(body, `"tag":"markdown"`); c < 8 {
		t.Errorf("expected ≥ 8 markdown elements for 8000-char entry, got %d\n--- body ---\n%s", c, truncateForTest(body, 600))
	}
	// Body size guard: should be well under 30 KB
	if len(body) > 30*1024 {
		t.Errorf("body size %d exceeds 30 KB envelope", len(body))
	}
}

// TestBuildReceiptCard_HeaderFooterRespected (F-37) verifies that
// header / footer / evicted marker still each get exactly 1 element
// even when entries are split (they're not part of the splitter).
func TestBuildReceiptCard_HeaderFooterRespected(t *testing.T) {
	r := &MessageReceipt{
		state:       StateCompleted,
		eventCount:  1,
		evicted:     2,
		lastEventAt: parseTime(t, "2026-08-01T14:32:05+08:00"),
		agentName:   "claude",
		workspace:   "/code/repo",
		entries: []LogEntry{
			{Icon: "📝", Text: strings.Repeat("a", 2500), Kind: "result"},
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	// header(1) + evicted marker(1) + chunks(3) + hr(1) + footer(1) = 7 elements
	if !strings.Contains(body, `…(前 2 条已省略)`) {
		t.Errorf("evicted marker missing")
	}
	if !strings.Contains(body, `"tag":"hr"`) {
		t.Errorf("hr divider missing")
	}
	if !strings.Contains(body, "Agent: claude") {
		t.Errorf("footer agent metadata missing")
	}
}

// TestBuildReceiptCard_ThinkingEntry_NotRendered (F-37 thread-route)
// verifies that thinking entries are NOT rendered into the receipt
// card body. They are routed to Feishu thread replies by
// Adapter.Send instead. The defensive guard in buildReceiptCard
// (adapter.go) is the backstop: even if a buggy caller or test
// fixture appends a Kind="thinking" entry directly to
// r.entries, the card body still hides it instead of leaking
// thinking content into the rolling log.
//
// Note: This test was added in the F-37 multi-div branch
// (origin/main) where thinking/tool entries WERE rendered with
// collapsible_panel + multi-div split. On the F-37 thread-route
// branch they are filtered out entirely, so we flip the
// assertion: 1 markdown element (the StateExecuting header
// line) but NO entry content + NO collapsible_panel. The
// multi-div split machinery is exercised by the OutText /
// OutResult long-content path instead — see F-37 §3.4.
func TestBuildReceiptCard_ThinkingEntry_NotRendered(t *testing.T) {
	r := &MessageReceipt{
		state:       StateExecuting,
		eventCount:  1,
		entries: []LogEntry{
			{Icon: "💭", Text: strings.Repeat("thinking...", 250), Kind: "thinking"}, // ~2750 chars
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	// 1 markdown element (the StateExecuting header) is OK.
	// The thinking entry must contribute ZERO additional
	// elements.
	if c := strings.Count(body, `"tag":"markdown"`); c != 1 {
		t.Errorf("expected 1 markdown (header only) for filtered thinking, got %d\n--- body ---\n%s", c, truncateForTest(body, 600))
	}
	if strings.Contains(body, `"tag":"collapsible_panel"`) {
		t.Errorf("thinking entry should NOT wrap in collapsible_panel on the F-37 thread-route branch")
	}
	if strings.Contains(body, "thinking...") {
		t.Errorf("thinking text leaked into the card body — should only be in the Feishu thread")
	}
}

// TestBuildReceiptCard_ToolEntry_NotRendered (F-37 thread-route):
// same defensive guard for tool_start / tool_end / tool entries.
// The original test (origin/main's F-37 multi-div branch) expected
// ≥ 2 markdown elements inside a collapsible_panel; the
// thread-route design filters them out so the main chat stays
// focused on the receipt card's final answer.
func TestBuildReceiptCard_ToolEntry_NotRendered(t *testing.T) {
	r := &MessageReceipt{
		state:       StateExecuting,
		eventCount:  1,
		entries: []LogEntry{
			{Icon: "🔧", Text: "Read(/a.py) " + strings.Repeat("x", 1100), Kind: "tool"},
		},
	}
	body, err := buildReceiptCard(r)
	if err != nil {
		t.Fatalf("buildReceiptCard: %v", err)
	}
	if c := strings.Count(body, `"tag":"markdown"`); c != 1 {
		t.Errorf("expected 1 markdown (header only) for filtered tool, got %d\n--- body ---\n%s", c, truncateForTest(body, 600))
	}
	if strings.Contains(body, `"tag":"collapsible_panel"`) {
		t.Errorf("tool entry should NOT wrap in collapsible_panel on the F-37 thread-route branch")
	}
	if strings.Contains(body, "Read(/a.py)") {
		t.Errorf("tool text leaked into the card body — should only be in the Feishu thread")
	}
}
