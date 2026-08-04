package feishu

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
)

// at is a small helper for fixed timestamps so test diffs stay
// deterministic (LogEntry.Time is included only for sorting /
// debugging, but having a non-zero value keeps the struct printed
// nicely when a test fails).
func at() time.Time { return time.Unix(1700000000, 0).UTC() }

// --- Existing kinds (sanity tests; the bulk of behaviour is exercised
// end-to-end in receipt_test.go). Kept short so this file can grow
// new-kind coverage without becoming a kitchen-sink suite.

func TestEventToEntry_Text(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{Kind: agent.EventText, Text: "hello"}, at(), nil)
	if !ok || e.Icon != "💬" || e.Text != "hello" || e.Kind != "reply" {
		t.Errorf("got %+v ok=%v, want 💬 hello/reply", e, ok)
	}
}

func TestEventToEntry_Text_ThinkingPrefix(t *testing.T) {
	// F-34: thinking events no longer fold into the receipt card
	// (the adapter routes them to a Feishu thread reply instead),
	// so eventToEntry returns (_, false) for the prefixed text.
	_, ok := eventToEntry(agent.AgentEvent{Kind: agent.EventText, Text: "[思考] step 1"}, at(), nil)
	if ok {
		t.Error("EventText with [思考] prefix should be dropped (F-34 routes it to a thread reply)")
	}
}

// --- New kinds (P1 follow-up). ---

func TestEventToEntry_Result(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "完成", Subtype: "success"},
	}, at(), nil)
	if !ok {
		t.Fatal("Result event should produce an entry")
	}
	if e.Icon != "📝" {
		t.Errorf("Icon = %q, want 📝", e.Icon)
	}
	if e.Text != "完成" {
		t.Errorf("Text = %q, want 完成", e.Text)
	}
	if e.Kind != "result" {
		t.Errorf("Kind = %q, want 'result'", e.Kind)
	}
}

func TestEventToEntry_Result_Error(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "max turns exceeded", IsError: true},
	}, at(), nil)
	if !ok || e.Icon != "⚠️" {
		t.Errorf("Error result should use ⚠️ icon; got %+v ok=%v", e, ok)
	}
}

func TestEventToEntry_Result_EmptyDropped(t *testing.T) {
	_, ok := eventToEntry(agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "", IsError: false},
	}, at(), nil)
	if ok {
		t.Error("Empty Result + !IsError should drop")
	}
}

func TestEventToEntry_Usage(t *testing.T) {
	// EventUsage is intentionally NOT rendered as a log
	// entry — the same token counts live in the receipt
	// footer (set on OutUsage → SetFooter). The eventToEntry
	// translator returns (_, false) so Append drops it from
	// the rolling log.
	_, ok := eventToEntry(agent.AgentEvent{
		Kind: agent.EventUsage,
		Usage: &agent.UsageEvent{
			InputTokens:              800,
			OutputTokens:             400,
			CacheReadInputTokens:     500,
			CacheCreationInputTokens: 100,
			CostUSD:                  0.0123,
		},
	}, at(), nil)
	if ok {
		t.Error("EventUsage must NOT produce a rolling-log entry (footer carries the numbers)")
	}
}

func TestEventToEntry_Usage_NoCost(t *testing.T) {
	// EventUsage is intentionally NOT rendered as a log
	// entry at all (footer carries the numbers), so the
	// CostUSD field doesn't drive rendering here. We still
	// assert the entry is dropped to lock in the contract.
	_, ok := eventToEntry(agent.AgentEvent{
		Kind:  agent.EventUsage,
		Usage: &agent.UsageEvent{InputTokens: 100, OutputTokens: 50},
	}, at(), nil)
	if ok {
		t.Error("EventUsage must NOT produce a rolling-log entry regardless of CostUSD")
	}
}

func TestEventToEntry_Usage_ZeroDropped(t *testing.T) {
	// All-zero usage is dropped (footer refresh would see
	// zeros and skip writing too — see appendToReceipt for
	// OutUsage). The translator still returns (_, false).
	_, ok := eventToEntry(agent.AgentEvent{
		Kind:  agent.EventUsage,
		Usage: &agent.UsageEvent{},
	}, at(), nil)
	if ok {
		t.Error("All-zero Usage should drop")
	}
}

func TestEventToEntry_Compaction(t *testing.T) {
	// F-34: compaction is posted as a thread reply; the receipt
	// card no longer carries it. eventToEntry returns (_, false).
	_, ok := eventToEntry(agent.AgentEvent{
		Kind:       agent.EventCompaction,
		Compaction: &agent.CompactionEvent{Subtype: "compact"},
	}, at(), nil)
	if ok {
		t.Error("EventCompaction should be dropped (F-34 routes it to a thread reply)")
	}
}

func TestEventToEntry_Init(t *testing.T) {
	// EventInit is intentionally NOT rendered as a log
	// entry — the agent name / model live in the receipt
	// footer (set on OutInit → SetAgentMeta), and the
	// session id is rarely useful in the chat surface.
	_, ok := eventToEntry(agent.AgentEvent{
		Kind: agent.EventInit,
		Init: &agent.InitEvent{SessionID: "s_001", Model: "claude-sonnet-4-5"},
	}, at(), nil)
	if ok {
		t.Error("EventInit must NOT produce a rolling-log entry (footer carries the same info)")
	}
}

func TestEventToEntry_Init_NoSessionID_Dropped(t *testing.T) {
	_, ok := eventToEntry(agent.AgentEvent{
		Kind: agent.EventInit,
		Init: &agent.InitEvent{SessionID: "", Model: "claude-sonnet-4-5"},
	}, at(), nil)
	if ok {
		t.Error("Init with empty SessionID should drop")
	}
}

func TestEventToEntry_Done_Dropped(t *testing.T) {
	// EventDone is reflected in the receipt's terminal header — no
	// per-event entry needed.
	_, ok := eventToEntry(agent.AgentEvent{Kind: agent.EventDone, Done: &agent.DoneEvent{}}, at(), nil)
	if ok {
		t.Error("EventDone should drop")
	}
}

// --- F-34: EventToolStart / EventToolEnd are dropped from the receipt card ---
// See docs/feat/F-34-tool-thread-routing.md. The adapter routes these
// events to a Feishu thread reply with a type-aware summary; the
// receipt card no longer carries them.

func TestEventToEntry_ToolStart_Dropped(t *testing.T) {
	ev := agent.AgentEvent{
		Kind: agent.EventToolStart,
		ToolStart: &agent.ToolStartEvent{
			Name: "Read",
			Args: "/a.py",
		},
	}
	if _, ok := eventToEntry(ev, at(), nil); ok {
		t.Error("EventToolStart should be dropped from the receipt card (F-34 routes it to a thread reply)")
	}
}

func TestEventToEntry_ToolEnd_Dropped(t *testing.T) {
	ev := agent.AgentEvent{
		Kind: agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{
			Name:   "Read",
			Output: "47 lines",
		},
	}
	if _, ok := eventToEntry(ev, at(), nil); ok {
		t.Error("EventToolEnd should be dropped from the receipt card (F-34 routes it to a thread reply)")
	}
}

// stringError is a tiny adapter so we can pass a string as error
// without importing "errors" into the test file (which already has
// its own scope).
type stringError string

func (s stringError) Error() string { return string(s) }

// TestTruncateForLog_RuneAware (F-37) verifies that truncateForLog
// counts characters (runes), not bytes. Before F-37, the function
// was byte-based despite its comment, which meant Chinese / emoji
// content was capped at 1/3-1/4 the intended character count.
func TestTruncateForLog_RuneAware(t *testing.T) {
	// 8000 Chinese chars (24 KB) — should NOT be truncated at
	// perEntryMaxRunes=8000 (rune-based) but WOULD be truncated
	// under the old byte-based behavior.
	text := strings.Repeat("中", 8000)
	got := truncateForLog(text, perEntryMaxRunes)
	if got != text {
		t.Errorf("Chinese 8000 chars at cap = 8000: got truncated (len %d), want unchanged", len([]rune(got)))
	}
	// 8001 chars: should truncate to 7999 chars + "…"
	text = strings.Repeat("中", 8001)
	got = truncateForLog(text, perEntryMaxRunes)
	if len([]rune(got)) != 8000 {
		t.Errorf("Chinese 8001 chars at cap = 8000: got %d runes, want 8000", len([]rune(got)))
	}
}

// TestTruncateForLog_EmojiRuneAware (F-37) verifies rune-aware
// counting for 4-byte UTF-8 emoji (🎉 = 1 rune, 4 bytes).
func TestTruncateForLog_EmojiRuneAware(t *testing.T) {
	text := strings.Repeat("🎉", 8000)
	got := truncateForLog(text, perEntryMaxRunes)
	if got != text {
		t.Errorf("Emoji 8000 chars at cap = 8000: got truncated (len %d), want unchanged", len([]rune(got)))
	}
}

// TestTruncateForLog_ByteLimit (F-37) verifies that the byte limit
// path still works for callers using perEntryMaxBytes=600.
func TestTruncateForLog_ByteLimit(t *testing.T) {
	// 600 ASCII chars: fits exactly
	text := strings.Repeat("a", 600)
	got := truncateForLog(text, perEntryMaxBytes)
	if got != text {
		t.Errorf("ASCII 600 chars at cap = 600: got truncated")
	}
	// 601 ASCII chars: truncate to 599 + "…"
	text = strings.Repeat("a", 601)
	got = truncateForLog(text, perEntryMaxBytes)
	if len([]rune(got)) != 600 {
		t.Errorf("ASCII 601 chars at cap = 600: got %d runes, want 600", len([]rune(got)))
	}
}

// TestTruncateForLog_NoMidRune (F-37) verifies that the truncation
// never splits a multi-byte UTF-8 rune.
func TestTruncateForLog_NoMidRune(t *testing.T) {
	// 50 Chinese chars (150 bytes) — leaving room for "…"
	text := strings.Repeat("中", 50)
	got := truncateForLog(text, 30)
	// Should be 29 chars + "…" = 30 runes; must be valid UTF-8
	if !utf8.ValidString(got) {
		t.Errorf("truncateForLog produced invalid UTF-8: %x", []byte(got))
	}
	if len([]rune(got)) != 30 {
		t.Errorf("got %d runes, want 30", len([]rune(got)))
	}
}
