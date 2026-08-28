package acp

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestShouldFlushBufferedText_DottedIdentifierNotSentence(t *testing.T) {
	// Per-token pause after "session." must NOT flush — next token
	// may be "idle_timeout". Same for the fully-formed key.
	cases := []string{
		"session.",
		"session.idle",
		"session.idle_timeout",
		"cfg.guest.enabled",
		"a.b.c.",
	}
	for _, c := range cases {
		padded := padToMinFlush(c)
		if shouldFlushBufferedText(padded) {
			t.Errorf("shouldFlushBufferedText(%q) = true, want false (dotted id)", c)
		}
	}
}

func TestShouldFlushBufferedText_TrueSentenceNeedsSpaceAfterASCII(t *testing.T) {
	// Bare trailing ASCII "." is ambiguous under per-token streams.
	bare := padToMinFlush("timeout is 30s.")
	if shouldFlushBufferedText(bare) {
		t.Errorf("bare ASCII period must wait for whitespace or boundary")
	}
	// Period + space is a real sentence break ("…30s. Next").
	withSpace := padToMinFlush("timeout is 30s. ")
	if !shouldFlushBufferedText(withSpace) {
		t.Errorf("ASCII period followed by space should flush")
	}
	withNL := padToMinFlush("timeout is 30s.\n")
	if !shouldFlushBufferedText(withNL) {
		t.Errorf("ASCII period followed by newline should flush")
	}
}

func TestShouldFlushBufferedText_ChineseSentenceAtEnd(t *testing.T) {
	s := padToMinFlush("配置默认是 30s。")
	if !shouldFlushBufferedText(s) {
		t.Errorf("Chinese 。 at end should flush (not used in identifiers)")
	}
}

func TestShouldFlushBufferedText_ListOrdinal(t *testing.T) {
	s := padToMinFlush("steps:\n4.")
	if shouldFlushBufferedText(s) {
		t.Errorf("list ordinal 4. must not flush")
	}
}

func TestShouldFlushBufferedText_Paragraph(t *testing.T) {
	s := padToMinFlush("first paragraph\n\n")
	if !shouldFlushBufferedText(s) {
		t.Errorf("paragraph break \\n\\n should flush")
	}
}

func TestShouldFlushBufferedText_BelowMinRunes(t *testing.T) {
	short := "Done. "
	if utf8.RuneCountInString(strings.TrimSpace(short)) >= minFlushRunes {
		t.Fatalf("fixture too long")
	}
	if shouldFlushBufferedText(short) {
		t.Errorf("below minFlushRunes must not mid-turn flush")
	}
}

func TestEndsWithTrueSentenceBoundary(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"session.", false},
		{"session.idle_timeout", false},
		{"hello. ", true},
		{"hello.\n", true},
		{"hello.", false},
		{"你好。", true},
		{"你好！", true},
		{"ok?", false},
		{"ok? ", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := endsWithTrueSentenceBoundary(tc.in); got != tc.want {
			t.Errorf("endsWithTrueSentenceBoundary(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEndsWithListOrdinal(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"4.", true},
		{"12.", true},
		{"\n4.", true},
		{"step 4.", true},
		{"v1.", false}, // 'v' before digits → not a list marker
		{"done.", false},
		{"session.", false},
	}
	for _, tc := range cases {
		if got := endsWithListOrdinal(tc.in); got != tc.want {
			t.Errorf("endsWithListOrdinal(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAppendAndMaybeFlush_SlidingIdleCoalescesWhileTokensArrive(t *testing.T) {
	prev := flushDebounce
	flushDebounce = 40 * time.Millisecond
	defer func() { flushDebounce = prev }()

	d := newFlushTestDriver(t)
	d.appendAndMaybeFlush(d.textBuf, padToMinFlush("第一句。"))

	// Still inside the idle window — nothing emitted yet.
	select {
	case ev := <-d.events:
		t.Fatalf("flushed too early during idle window: %q", ev.Text)
	case <-time.After(15 * time.Millisecond):
	}

	// Another token resets the sliding idle clock.
	d.appendAndMaybeFlush(d.textBuf, "第二句也够长了。")

	select {
	case ev := <-d.events:
		t.Fatalf("flushed before idle after second sentence: %q", ev.Text)
	case <-time.After(15 * time.Millisecond):
	}

	var got agent.AgentEvent
	select {
	case got = <-d.events:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for idle flush")
	}
	if got.Kind != agent.EventAgentText {
		t.Fatalf("Kind = %v, want EventAgentText", got.Kind)
	}
	if !strings.Contains(got.Text, "第一句") || !strings.Contains(got.Text, "第二句") {
		t.Fatalf("coalesced text missing sentences: %q", got.Text)
	}
}

func TestAppendAndMaybeFlush_ThoughtTokensDoNotBlockReadyReply(t *testing.T) {
	prev := flushDebounce
	flushDebounce = 40 * time.Millisecond
	defer func() { flushDebounce = prev }()

	d := newFlushTestDriver(t)
	d.appendAndMaybeFlush(d.textBuf, padToMinFlush("回复已经断句。"))
	// Incomplete thought resets the shared idle timer but must not
	// prevent the ready reply from flushing on the next quiet window.
	d.appendAndMaybeFlush(d.thoughtBuf, "还在想")

	var got agent.AgentEvent
	select {
	case got = <-d.events:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ready reply blocked by incomplete thought")
	}
	if strings.HasPrefix(got.Text, "[思考]") {
		t.Fatalf("expected reply flush, got thought: %q", got.Text)
	}
	if !strings.Contains(got.Text, "回复已经断句") {
		t.Fatalf("reply text = %q", got.Text)
	}
	// Thought stays buffered (not ready).
	d.textMu.Lock()
	left := d.thoughtBuf.String()
	d.textMu.Unlock()
	if left != "还在想" {
		t.Fatalf("thoughtBuf = %q, want incomplete thought retained", left)
	}
}

func TestAppendAndMaybeFlush_IdleAfterDottedIdDoesNotFlush(t *testing.T) {
	prev := flushDebounce
	flushDebounce = 30 * time.Millisecond
	defer func() { flushDebounce = prev }()

	d := newFlushTestDriver(t)
	d.appendAndMaybeFlush(d.textBuf, padToMinFlush("session.idle_timeout"))

	select {
	case ev := <-d.events:
		t.Fatalf("dotted identifier must not flush on idle, got %q", ev.Text)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestAppendAndMaybeFlush_IdleAfterBareASCIIPeriodDoesNotFlush(t *testing.T) {
	prev := flushDebounce
	flushDebounce = 30 * time.Millisecond
	defer func() { flushDebounce = prev }()

	d := newFlushTestDriver(t)
	d.appendAndMaybeFlush(d.textBuf, padToMinFlush("session."))

	select {
	case ev := <-d.events:
		t.Fatalf("bare trailing ASCII period must not flush on idle, got %q", ev.Text)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestAppendAndMaybeFlush_AbbreviationNotSentence(t *testing.T) {
	prev := flushDebounce
	flushDebounce = 30 * time.Millisecond
	defer func() { flushDebounce = prev }()

	d := newFlushTestDriver(t)
	d.appendAndMaybeFlush(d.textBuf, padToMinFlush("see e.g. "))

	select {
	case ev := <-d.events:
		t.Fatalf("abbreviation e.g. must not flush on idle, got %q", ev.Text)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestEndsWithTrueSentenceBoundary_Abbreviations(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"see e.g. ", false},
		{"ask Mr. ", false},
		{"done etc. ", false},
		{"real end. ", true},
		{"hello? ", true},
	}
	for _, tc := range cases {
		if got := endsWithTrueSentenceBoundary(tc.in); got != tc.want {
			t.Errorf("endsWithTrueSentenceBoundary(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFlushTextBuffers_CancelsIdleTimer(t *testing.T) {
	prev := flushDebounce
	flushDebounce = 500 * time.Millisecond
	defer func() { flushDebounce = prev }()

	d := newFlushTestDriver(t)
	d.appendAndMaybeFlush(d.textBuf, padToMinFlush("即将被 tool 打断。"))
	d.flushTextBuffers()

	select {
	case got := <-d.events:
		if !strings.Contains(got.Text, "即将被 tool 打断") {
			t.Fatalf("forced flush text = %q", got.Text)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("forced flushTextBuffers should emit immediately")
	}

	select {
	case ev := <-d.events:
		t.Fatalf("unexpected post-cancel flush: %q", ev.Text)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestClose_DrainsPendingBuffers(t *testing.T) {
	prev := flushDebounce
	flushDebounce = 500 * time.Millisecond
	defer func() { flushDebounce = prev }()

	d := newFlushTestDriver(t)
	d.appendAndMaybeFlush(d.textBuf, padToMinFlush("Close 前要保住。"))
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case got := <-d.events:
		if !strings.Contains(got.Text, "Close 前要保住") {
			t.Fatalf("Close drain text = %q", got.Text)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Close should drain pending text before cancel")
	}
}

func newFlushTestDriver(t *testing.T) *driver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &driver{
		ctx:            ctx,
		cancel:         cancel,
		events:         make(chan agent.AgentEvent, 16),
		textBuf:        &strings.Builder{},
		thoughtBuf:     &strings.Builder{},
		thinkingPrefix: "[思考] ",
		agentName:      "test",
	}
}

// padToMinFlush prefixes filler so the fixture clears minFlushRunes
// without changing the trailing boundary under test.
func padToMinFlush(suffix string) string {
	need := minFlushRunes - utf8.RuneCountInString(strings.TrimSpace(suffix)) + 8
	if need < 0 {
		need = 8
	}
	return strings.Repeat("字", need) + suffix
}
