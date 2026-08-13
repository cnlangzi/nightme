// Tests for inline <think>...</think> extraction.
//
// Pi normally surfaces reasoning via structured thinking_* events
// (handled in translate_test.go). This file covers the fallback
// path: when Pi emits reasoning inline as raw text wrapped in
// <think>...</think> tags — consistent on the Windows trace that
// prompted think_tags.go — the bridge must strip the tags and
// route the content to the [思考] surface instead of letting it
// leak into the reply text.
//
// Coverage:
//
//   - splitThinking: pure-function unit tests for the splitter
//     itself (no translator state). Locks the Held protocol that
//     keeps split-boundary blocks whole.
//
//   - text_delta handler: end-to-end through the translator,
//     confirming inline tags in a single delta, tags that span two
//     deltas (the case that motivated the Held buffer), and a
//     delta with text before and after a single think block.
//
//   - message_end content blocks: two flavours — Pi's wire-level
//     Type "thinking" block, and a Type "text" block whose payload
//     contains <think>...</think>. Both must surface as thinking
//     events without polluting lastMessageText.

package pi

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── splitThinking unit tests ─────────────────────────────────────

func TestSplitThinking_PlainTextUnchanged(t *testing.T) {
	got := splitThinking("hello world")
	if got.Kept != "hello world" {
		t.Errorf("Kept = %q, want %q", got.Kept, "hello world")
	}
	if got.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", got.Thinking)
	}
	if got.Held != "" {
		t.Errorf("Held = %q, want empty", got.Held)
	}
}

func TestSplitThinking_SingleBlock(t *testing.T) {
	got := splitThinking("hello <think>reasoning</think> world")
	if got.Kept != "hello  world" {
		t.Errorf("Kept = %q, want %q", got.Kept, "hello  world")
	}
	if got.Thinking != "reasoning" {
		t.Errorf("Thinking = %q, want %q", got.Thinking, "reasoning")
	}
	if got.Held != "" {
		t.Errorf("Held = %q, want empty", got.Held)
	}
}

func TestSplitThinking_MultipleBlocks(t *testing.T) {
	got := splitThinking("a<think>first</think>b<think>second</think>c")
	if got.Kept != "abc" {
		t.Errorf("Kept = %q, want %q", got.Kept, "abc")
	}
	if got.Thinking != "first\nsecond" {
		t.Errorf("Thinking = %q, want %q", got.Thinking, "first\nsecond")
	}
	if got.Held != "" {
		t.Errorf("Held = %q, want empty", got.Held)
	}
}

func TestSplitThinking_TrailingPartialOpen(t *testing.T) {
	got := splitThinking("hello <think>half-baked")
	if got.Kept != "hello " {
		t.Errorf("Kept = %q, want %q", got.Kept, "hello ")
	}
	if got.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", got.Thinking)
	}
	if got.Held != "<think>half-baked" {
		t.Errorf("Held = %q, want %q", got.Held, "<think>half-baked")
	}
}

// TestSplitThinking_HeldCompletesAcrossCalls simulates the streaming
// protocol that keeps a split-boundary block whole: the next call
// prepends the Held partial before rescanning.
func TestSplitThinking_HeldCompletesAcrossCalls(t *testing.T) {
	first := splitThinking("hello <think>the user wants")
	if first.Held == "" {
		t.Fatalf("first call did not hold a partial; Held = %q", first.Held)
	}
	if first.Thinking != "" {
		t.Errorf("first call Thinking = %q, want empty", first.Thinking)
	}

	second := splitThinking(first.Held + " me to switch</think> world")
	if second.Thinking != "the user wants me to switch" {
		t.Errorf("second call Thinking = %q, want %q", second.Thinking, "the user wants me to switch")
	}
	if second.Kept != " world" {
		t.Errorf("second call Kept = %q, want %q", second.Kept, " world")
	}
	if second.Held != "" {
		t.Errorf("second call Held = %q, want empty", second.Held)
	}
}

// TestSplitThinking_StrayCloseTagKept guards against an overzealous
// splitter: a bare </think> with no preceding <think> in the input
// must be preserved as ordinary text. Pi never emits stray close
// tags today, but the bridge must not eat user content if some
// future variant does.
func TestSplitThinking_StrayCloseTagKept(t *testing.T) {
	got := splitThinking("user typed </think> here")
	if got.Kept != "user typed </think> here" {
		t.Errorf("Kept = %q, want %q", got.Kept, "user typed </think> here")
	}
	if got.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", got.Thinking)
	}
}

// ─── text_delta end-to-end tests ──────────────────────────────────

// textDeltaEvent builds a single text_delta wire event for the
// given contentIndex. Helper for the streaming tests below.
func textDeltaEvent(idx int, delta string) string {
	return `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":` +
		testItoa(idx) + `,"delta":` + testJSONString(delta) + `}}`
}

// testItoa / testJSONString are tiny helpers used only by the
// inline-tag test cases. We avoid pulling strconv/encoding/json
// into the test surface by hand-rolling the trivial cases. The
// `test` prefix avoids collisions with package-level helpers of
// the same name (rpc.go's jsonString, session_real_unix_test.go's
// itoa, both of which would clash in the test build).
func testItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// testJSONString quotes a string with the escapes the protocol
// expects: backslash and double-quote. We do not need the full JSON
// spec for the test inputs — they only contain ASCII text — but
// the wire parser requires valid JSON, so the delta has to be
// properly quoted.
func testJSONString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestTranslate_TextDeltaInlineThinkStripsTags verifies the central
// invariant: a single text_delta whose payload contains
// <think>...</think> yields ONE EventAgentText with the [思考]
// prefix and leaves the surrounding text in textBuf without the
// tags. The reply surface never sees "<think>".
func TestTranslate_TextDeltaInlineThinkStripsTags(t *testing.T) {
	tr := newTestTranslator()

	mustTranslate(t, tr, textDeltaEvent(0, "Hello "))
	events := mustTranslate(t, tr, textDeltaEvent(0, "<think>reasoning text</think> world"))

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1; got %+v", len(events), events)
	}
	if events[0].Kind != agent.EventAgentText {
		t.Fatalf("event kind = %s, want EventAgentText", events[0].Kind)
	}
	if events[0].Text != "[思考] reasoning text" {
		t.Errorf("event Text = %q, want %q", events[0].Text, "[思考] reasoning text")
	}
	if got := tr.turn.textBuf[0].String(); got != "Hello  world" {
		t.Errorf("textBuf[0] = %q, want %q (tags must not leak into reply)", got, "Hello  world")
	}
	if strings.Contains(tr.turn.textBuf[0].String(), "<think>") {
		t.Error("textBuf contains <think> — tag leaked into reply surface")
	}
}

// TestTranslate_TextDeltaThinkSpansTwoDeltas is the case that
// motivated the Held buffer. The <think> opens at the end of one
// delta and closes mid-way through the next — without Held the
// bridge would either drop both halves (no complete substring in
// either delta) or leak the tags into the reply.
func TestTranslate_TextDeltaThinkSpansTwoDeltas(t *testing.T) {
	tr := newTestTranslator()

	mustTranslate(t, tr, textDeltaEvent(0, "Hello <think>the user wants"))
	events := mustTranslate(t, tr, textDeltaEvent(0, " me to switch</think> world"))

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1; got %+v", len(events), events)
	}
	if events[0].Text != "[思考] the user wants me to switch" {
		t.Errorf("event Text = %q, want %q", events[0].Text, "[思考] the user wants me to switch")
	}
	if got := tr.turn.textBuf[0].String(); got != "Hello  world" {
		t.Errorf("textBuf[0] = %q, want %q", got, "Hello  world")
	}
}

// TestTranslate_TextDeltaOnlyThinkingLockedAway mirrors the
// TestTranslate_ThinkingDoesNotLeakIntoResult invariant but for
// the inline path: a turn whose only reply content is reasoning
// (no text outside the think block) must not surface that
// reasoning in the EventAgentResult.
func TestTranslate_TextDeltaOnlyThinkingLockedAway(t *testing.T) {
	tr := newTestTranslator()

	// Collect events across the whole turn — the [思考] event
	// fires on the text_delta, before text_end / agent_settled.
	var all []agent.AgentEvent
	all = append(all, mustTranslate(t, tr, textDeltaEvent(0, "<think>secret plan</think>"))...)
	all = append(all, mustTranslate(t, tr,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0}}`,
	)...)
	all = append(all, mustTranslate(t, tr, `{"type":"agent_settled"}`)...)

	result := findResult(t, all)
	if strings.Contains(result.Text, "secret plan") {
		t.Errorf("EventAgentResult.Text = %q, must not contain reasoning text", result.Text)
	}
	// And the reasoning did surface earlier as a [思考] event.
	hasThink := false
	for _, ev := range all {
		if ev.Kind == agent.EventAgentText && strings.Contains(ev.Text, "secret plan") {
			hasThink = true
			break
		}
	}
	if !hasThink {
		t.Errorf("reasoning never surfaced as [思考] event; got texts=%v", texts(all))
	}
}

// ─── message_end content-block tests ──────────────────────────────

// TestTranslate_MessageEndThinkingBlock verifies that a non-streamed
// message carrying Pi's wire-level Type "thinking" content block
// surfaces as a [思考] EventAgentText — the inline-equivalent of
// the structured thinking_end path.
func TestTranslate_MessageEndThinkingBlock(t *testing.T) {
	tr := newTestTranslator()

	msg := map[string]any{
		"role":       "assistant",
		"stopReason": "stop",
		"content": []map[string]any{
			{"type": "thinking", "thinking": "block-level reasoning"},
			{"type": "text", "text": "final reply"},
		},
	}
	raw := mustMarshal(t, map[string]any{"type": "message_end", "message": msg})
	events := mustTranslate(t, tr, string(raw))

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1; got %+v", len(events), events)
	}
	if events[0].Text != "[思考] block-level reasoning" {
		t.Errorf("event Text = %q, want %q", events[0].Text, "[思考] block-level reasoning")
	}
	// lastMessageText holds the plain text only.
	if tr.turn.lastMessageText != "final reply" {
		t.Errorf("lastMessageText = %q, want %q (must not contain reasoning)", tr.turn.lastMessageText, "final reply")
	}
}

// TestTranslate_MessageEndTextBlockWithInlineTags verifies the
// non-streamed counterpart of TestTranslate_TextDeltaInlineThinkStripsTags:
// a Type "text" block whose payload contains <think>...</think>
// must have its tags stripped and the reasoning extracted, with
// the kept text going into lastMessageText.
func TestTranslate_MessageEndTextBlockWithInlineTags(t *testing.T) {
	tr := newTestTranslator()

	msg := map[string]any{
		"role":       "assistant",
		"stopReason": "stop",
		"content": []map[string]any{
			{"type": "text", "text": "Hello <think>in-block reasoning</think> world"},
		},
	}
	raw := mustMarshal(t, map[string]any{"type": "message_end", "message": msg})
	events := mustTranslate(t, tr, string(raw))

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1; got %+v", len(events), events)
	}
	if events[0].Text != "[思考] in-block reasoning" {
		t.Errorf("event Text = %q, want %q", events[0].Text, "[思考] in-block reasoning")
	}
	if tr.turn.lastMessageText != "Hello  world" {
		t.Errorf("lastMessageText = %q, want %q", tr.turn.lastMessageText, "Hello  world")
	}
	if strings.Contains(tr.turn.lastMessageText, "<think>") {
		t.Error("lastMessageText contains <think> — tag leaked into reply fallback")
	}
}