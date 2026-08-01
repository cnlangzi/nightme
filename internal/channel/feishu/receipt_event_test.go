package feishu

import (
	"strings"
	"testing"
	"time"

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
	e, ok := eventToEntry(agent.AgentEvent{Kind: agent.EventText, Text: "hello"}, at())
	if !ok || e.Icon != "💬" || e.Text != "hello" || e.Kind != "reply" {
		t.Errorf("got %+v ok=%v, want 💬 hello/reply", e, ok)
	}
}

func TestEventToEntry_Text_ThinkingPrefix(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{Kind: agent.EventText, Text: "[思考] step 1"}, at())
	if !ok || e.Icon != "💭" || !strings.Contains(e.Text, "step 1") {
		t.Errorf("got %+v ok=%v, want 💭 'step 1'", e, ok)
	}
}

// --- New kinds (P1 follow-up). ---

func TestEventToEntry_Result(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "完成", Subtype: "success"},
	}, at())
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
	}, at())
	if !ok || e.Icon != "⚠️" {
		t.Errorf("Error result should use ⚠️ icon; got %+v ok=%v", e, ok)
	}
}

func TestEventToEntry_Result_EmptyDropped(t *testing.T) {
	_, ok := eventToEntry(agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "", IsError: false},
	}, at())
	if ok {
		t.Error("Empty Result + !IsError should drop")
	}
}

func TestEventToEntry_Usage(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{
		Kind: agent.EventUsage,
		Usage: &agent.UsageEvent{
			InputTokens:              800,
			OutputTokens:             400,
			CacheReadInputTokens:     500,
			CacheCreationInputTokens: 100,
			CostUSD:                  0.0123,
		},
	}, at())
	if !ok {
		t.Fatal("Usage event should produce an entry")
	}
	if e.Icon != "📊" {
		t.Errorf("Icon = %q, want 📊", e.Icon)
	}
	for _, want := range []string{"1800 tokens", "in 800", "out 400", "cache read 500", "cache create 100", "$0.0123"} {
		if !strings.Contains(e.Text, want) {
			t.Errorf("Text %q missing %q", e.Text, want)
		}
	}
}

func TestEventToEntry_Usage_NoCost(t *testing.T) {
	// CostUSD=0 should be omitted (channels must NOT render "$0.00").
	e, ok := eventToEntry(agent.AgentEvent{
		Kind:  agent.EventUsage,
		Usage: &agent.UsageEvent{InputTokens: 100, OutputTokens: 50},
	}, at())
	if !ok {
		t.Fatal("Usage event should produce an entry")
	}
	if strings.Contains(e.Text, "$") {
		t.Errorf("Text %q should not contain $ when CostUSD=0", e.Text)
	}
}

func TestEventToEntry_Usage_ZeroDropped(t *testing.T) {
	// All-zero usage is indistinguishable from "absent" → drop.
	_, ok := eventToEntry(agent.AgentEvent{
		Kind:  agent.EventUsage,
		Usage: &agent.UsageEvent{},
	}, at())
	if ok {
		t.Error("All-zero Usage should drop")
	}
}

func TestEventToEntry_Compaction(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{
		Kind:       agent.EventCompaction,
		Compaction: &agent.CompactionEvent{Subtype: "compact"},
	}, at())
	if !ok {
		t.Fatal("Compaction event should produce an entry")
	}
	if e.Icon != "✶" {
		t.Errorf("Icon = %q, want ✶", e.Icon)
	}
	if !strings.Contains(e.Text, "Compacting") {
		t.Errorf("Text = %q, want to contain 'Compacting'", e.Text)
	}
}

func TestEventToEntry_Init(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{
		Kind: agent.EventInit,
		Init: &agent.InitEvent{SessionID: "s_001", Model: "claude-sonnet-4-5"},
	}, at())
	if !ok {
		t.Fatal("Init event should produce an entry")
	}
	if e.Icon != "🆔" {
		t.Errorf("Icon = %q, want 🆔", e.Icon)
	}
	for _, want := range []string{"s_001", "claude-sonnet-4-5"} {
		if !strings.Contains(e.Text, want) {
			t.Errorf("Text %q missing %q", e.Text, want)
		}
	}
}

func TestEventToEntry_Init_NoSessionID_Dropped(t *testing.T) {
	_, ok := eventToEntry(agent.AgentEvent{
		Kind: agent.EventInit,
		Init: &agent.InitEvent{SessionID: "", Model: "claude-sonnet-4-5"},
	}, at())
	if ok {
		t.Error("Init with empty SessionID should drop")
	}
}

func TestEventToEntry_Done_Dropped(t *testing.T) {
	// EventDone is reflected in the receipt's terminal header — no
	// per-event entry needed.
	_, ok := eventToEntry(agent.AgentEvent{Kind: agent.EventDone, Done: &agent.DoneEvent{}}, at())
	if ok {
		t.Error("EventDone should drop")
	}
}