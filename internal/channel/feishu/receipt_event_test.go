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
	e, ok := eventToEntry(agent.AgentEvent{Kind: agent.EventText, Text: "hello"}, at(), nil)
	if !ok || e.Icon != "💬" || e.Text != "hello" || e.Kind != "reply" {
		t.Errorf("got %+v ok=%v, want 💬 hello/reply", e, ok)
	}
}

func TestEventToEntry_Text_ThinkingPrefix(t *testing.T) {
	e, ok := eventToEntry(agent.AgentEvent{Kind: agent.EventText, Text: "[思考] step 1"}, at(), nil)
	if !ok || e.Icon != "💭" || !strings.Contains(e.Text, "step 1") {
		t.Errorf("got %+v ok=%v, want 💭 'step 1'", e, ok)
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
	e, ok := eventToEntry(agent.AgentEvent{
		Kind:       agent.EventCompaction,
		Compaction: &agent.CompactionEvent{Subtype: "compact"},
	}, at(), nil)
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
