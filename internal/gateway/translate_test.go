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
	// §1.4 cleanup: Result fields flow through the typed
	// OutboundMessage.Result payload, not Meta.
	if msg.Result == nil {
		t.Fatal("msg.Result is nil; Gateway should populate the typed ResultEvent payload")
	}
	if msg.Result.DurationMs != 1234 {
		t.Errorf("Result.DurationMs = %v, want 1234", msg.Result.DurationMs)
	}
	if msg.Result.Subtype != "success" {
		t.Errorf("Result.Subtype = %v, want success", msg.Result.Subtype)
	}
	if msg.Result.IsError {
		t.Error("Result.IsError = true; want false")
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
	// §1.4 cleanup: token counts flow through the typed
	// OutboundMessage.Usage payload, not Meta.
	if msg.Usage == nil {
		t.Fatal("msg.Usage is nil; Gateway should populate the typed UsageInfo payload")
	}
	if msg.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %v, want 100", msg.Usage.InputTokens)
	}
	if msg.Usage.CostUSD != 0.001 {
		t.Errorf("Usage.CostUSD = %v, want 0.001", msg.Usage.CostUSD)
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
	// F-34 review fix (architecture feedback 2026-08-04):
	// ToolEndEvent.Args flows through the Gateway into
	// OutboundMessage.Tool.Args (typed ToolInfo) so channel
	// renderers can produce type-aware thread-reply summaries
	// without mining Meta for implicit per-tool keys.
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
	if msg.Tool == nil {
		t.Fatal("msg.Tool is nil; Gateway should populate the unified ToolInfo")
	}
	if msg.Tool.Name != "Read" {
		t.Errorf("Tool.Name = %q, want %q", msg.Tool.Name, "Read")
	}
	if msg.Tool.Args != "/foo.go" {
		t.Errorf("Tool.Args = %q, want %q", msg.Tool.Args, "/foo.go")
	}
	if msg.Tool.Output != "47 lines" {
		t.Errorf("Tool.Output = %q, want %q", msg.Tool.Output, "47 lines")
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
	// §1.4 cleanup: init fields flow through the typed
	// OutboundMessage.Init payload, not Meta.
	if msg.Init == nil {
		t.Fatal("msg.Init is nil; Gateway should populate the typed InitEvent payload")
	}
	if msg.Init.SessionID != "s_001" {
		t.Errorf("Init.SessionID = %v, want 's_001'", msg.Init.SessionID)
	}
	if msg.Init.Model != "claude-sonnet-4-5" {
		t.Errorf("Init.Model = %v, want 'claude-sonnet-4-5'", msg.Init.Model)
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
