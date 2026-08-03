package gateway

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestTranslate covers the AgentEvent → OutboundMessage mapping for
// every EventKind that has its own OutboundKind (i.e. events that
// actually emit something). Terminal / dropped kinds
// (EventDone, EventError, default) are tested implicitly by the
// existing tests in cmd/gateway_test.go.
//
// Tests follow the pattern: build AgentEvent, call Translate, assert
// Kind + Text + key Meta fields. We don't assert on every Meta entry —
// the bridge / translator contract is that producers + consumers
// agree on key names; consumers are the source of truth for which
// keys are mandatory (see feishu/adapter_test.go).

func TestTranslate_EventText(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventText, Text: "hello"})
	if !ok || msg.Kind != OutText || msg.Text != "hello" {
		t.Errorf("got (%v, %v) text=%q, want (OutText=true, hello)", msg.Kind, ok, msg.Text)
	}
}

func TestTranslate_EventText_ThinkingPrefix(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventText, Text: "[思考] thinking…"})
	if !ok || msg.Kind != OutThinking || msg.Text != "thinking…" {
		t.Errorf("got kind=%v text=%q, want OutThinking / thinking…", msg.Kind, msg.Text)
	}
}

func TestTranslate_EventText_EmptyDropped(t *testing.T) {
	_, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventText, Text: "  "})
	if ok {
		t.Error("empty / whitespace-only text should drop, not emit")
	}
}

func TestTranslate_EventResult(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventResult,
		Result: &agent.ResultEvent{
			Text:       "完成",
			DurationMs: 1234,
			IsError:    false,
			Subtype:    "success",
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("expected translate to emit")
	}
	if msg.Kind != OutResult {
		t.Errorf("Kind = %v, want OutResult", msg.Kind)
	}
	if msg.Text != "完成" {
		t.Errorf("Text = %q, want 完成", msg.Text)
	}
	if msg.Meta["duration_ms"] != int64(1234) {
		t.Errorf("duration_ms = %v, want 1234", msg.Meta["duration_ms"])
	}
	if msg.Meta["subtype"] != "success" {
		t.Errorf("subtype = %v, want success", msg.Meta["subtype"])
	}
}

func TestTranslate_EventResult_EmptyDropped(t *testing.T) {
	// Empty text + IsError=false → no useful content; drop.
	in := agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "", IsError: false, Subtype: "success"},
	}
	if _, ok := Translate("chat1", in); ok {
		t.Error("empty Result with !IsError should drop")
	}
}

func TestTranslate_EventResult_ErrorKept(t *testing.T) {
	// Empty text + IsError=true → kept so channels can flip header
	// to error state.
	in := agent.AgentEvent{
		Kind:   agent.EventResult,
		Result: &agent.ResultEvent{Text: "", IsError: true, Subtype: "error_max_turns"},
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != OutResult {
		t.Errorf("IsError=true should keep the event; got kind=%v ok=%v", msg.Kind, ok)
	}
}

func TestTranslate_EventUsage(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventUsage,
		Usage: &agent.UsageEvent{
			InputTokens:  100,
			OutputTokens: 200,
			CostUSD:      0.001,
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != OutUsage {
		t.Fatalf("got (%v, %v), want (OutUsage, true)", msg.Kind, ok)
	}
	if msg.Meta["input_tokens"] != 100 {
		t.Errorf("input_tokens = %v, want 100", msg.Meta["input_tokens"])
	}
	if msg.Meta["cost_usd"] != 0.001 {
		t.Errorf("cost_usd = %v, want 0.001", msg.Meta["cost_usd"])
	}
	// Text should mention token count.
	if msg.Text == "" {
		t.Error("Text is empty; expected a one-line summary")
	}
}

func TestTranslate_EventUsage_NilDropped(t *testing.T) {
	if _, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventUsage}); ok {
		t.Error("nil Usage should drop")
	}
}

func TestTranslate_EventToolEnd_CarriesArgs(t *testing.T) {
	// F-34: ToolEndEvent.Args flows through the Gateway into
	// OutboundMessage.Meta["args"] so channel renderers can
	// produce type-aware thread-reply summaries without
	// re-parsing the tool_result content.
	in := agent.AgentEvent{
		Kind: agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{
			Name:   "Read",
			Args:   "/foo.go",
			Output: "47 lines",
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != OutToolEnd {
		t.Fatalf("got (%v, %v), want (OutToolEnd, true)", msg.Kind, ok)
	}
	if got := msg.Meta["args"]; got != "/foo.go" {
		t.Errorf("Meta[args] = %v, want /foo.go", got)
	}
}

func TestTranslate_EventCompaction(t *testing.T) {
	in := agent.AgentEvent{
		Kind:       agent.EventCompaction,
		Compaction: &agent.CompactionEvent{Subtype: "compact"},
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != OutCompaction {
		t.Fatalf("got (%v, %v), want (OutCompaction, true)", msg.Kind, ok)
	}
	if msg.Text == "" {
		t.Error("Text is empty; expected the Compacting… indicator")
	}
	if msg.Meta["subtype"] != "compact" {
		t.Errorf("subtype = %v, want 'compact'", msg.Meta["subtype"])
	}
}

func TestTranslate_EventInit(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventInit,
		Init: &agent.InitEvent{SessionID: "s_001", Model: "claude-sonnet-4-5"},
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != OutInit {
		t.Fatalf("got (%v, %v), want (OutInit, true)", msg.Kind, ok)
	}
	if msg.Meta["session_id"] != "s_001" {
		t.Errorf("session_id = %v, want 's_001'", msg.Meta["session_id"])
	}
	if msg.Meta["model"] != "claude-sonnet-4-5" {
		t.Errorf("model = %v, want 'claude-sonnet-4-5'", msg.Meta["model"])
	}
}

func TestTranslate_EventInit_NilDropped(t *testing.T) {
	if _, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventInit}); ok {
		t.Error("nil Init should drop")
	}
}

func TestTranslate_EventDone_Dropped(t *testing.T) {
	// EventDone is reflected in the receipt's terminal header — no
	// separate outbound message.
	if _, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventDone, Done: &agent.DoneEvent{ExitCode: 0}}); ok {
		t.Error("EventDone should drop (no OutboundMessage)")
	}
}
