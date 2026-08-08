package gateway

import (
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestTranslate covers the AgentEvent → OutboundMessage mapping for
// every EventKind that has its own OutboundKind (i.e. events that
// actually emit something). Terminal / dropped kinds
// (EventAgentDone, EventAgentError, default) are tested implicitly by the
// existing tests in cmd/gateway_test.go.
//
// Tests follow the pattern: build AgentEvent, call Translate, assert
// Kind + Text + key Meta fields. We don't assert on every Meta entry —
// the bridge / translator contract is that producers + consumers
// agree on key names; consumers are the source of truth for which
// keys are mandatory (see feishu/adapter_test.go).

func TestTranslate_EventText(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentText, Text: "hello"})
	if !ok || msg.Kind != OutReply || msg.Text != "hello" {
		t.Errorf("got (%v, %v) text=%q, want (OutReply=true, hello)", msg.Kind, ok, msg.Text)
	}
}

func TestTranslate_EventText_ThinkingPrefix(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentText, Text: "[思考] thinking…"})
	if !ok || msg.Kind != OutThinking || msg.Text != "thinking…" {
		t.Errorf("got kind=%v text=%q, want OutThinking / thinking…", msg.Kind, msg.Text)
	}
}

func TestTranslate_EventText_EmptyDropped(t *testing.T) {
	_, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentText, Text: "  "})
	if ok {
		t.Error("empty / whitespace-only text should drop, not emit")
	}
}

func TestTranslate_EventResult(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:       "完成",
			DurationMs: 1234,
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
		t.Fatal("msg.Result is nil; Gateway should populate the typed AgentResultEvent payload")
	}
	if msg.Result.DurationMs != 1234 {
		t.Errorf("Result.DurationMs = %v, want 1234", msg.Result.DurationMs)
	}
	if msg.Result.Subtype != "success" {
		t.Errorf("Result.Subtype = %v, want success", msg.Result.Subtype)
	}
	if msg.Err != nil {
		t.Error("Result.IsError = true; want false")
	}
}

func TestTranslate_EventResult_EmptyDropped(t *testing.T) {
	// Empty text + IsError=false → no useful content; drop.
	in := agent.AgentEvent{
		Kind:   agent.EventAgentResult,
		Result: &agent.AgentResultEvent{Text: "", Subtype: "success"},
	}
	if _, ok := Translate("chat1", in); ok {
		t.Error("empty Result with !IsError should drop")
	}
}

func TestTranslate_EventResult_ErrorKept(t *testing.T) {
	// Empty text + IsError=true → kept so channels can flip header
	// to error state.
	in := agent.AgentEvent{
		Kind:   agent.EventAgentResult,
		Err:    errors.New("error"),
		Result: &agent.AgentResultEvent{Text: "", Subtype: "error_max_turns"},
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != OutResult {
		t.Errorf("IsError=true should keep the event; got kind=%v ok=%v", msg.Kind, ok)
	}
}

// TestTranslate_EventResult_CoLocatesUsage verifies that an
// EventAgentResult carrying usage (via AgentResultEvent.Usage, the
// single-event design that replaced the EventAgentResult + EventUsage
// pair) gets Translated into an OutboundMessage with Usage
// populated on the SAME out, not a separate OutUsage kind.
func TestTranslate_EventResult_CoLocatesUsage(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    "完成",
			Subtype: "success",
			Usage: &agent.UsageInfo{
				InputTokens:          100,
				OutputTokens:         200,
				CacheReadInputTokens: 30,
				CostUSD:              0.001,
			},
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("expected translate to emit")
	}
	if msg.Kind != OutResult {
		t.Errorf("Kind = %v, want OutResult", msg.Kind)
	}
	if msg.Usage == nil {
		t.Fatal("msg.Usage is nil; Gateway should populate it from AgentResultEvent.Usage")
	}
	if msg.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %v, want 100", msg.Usage.InputTokens)
	}
	if msg.Usage.OutputTokens != 200 {
		t.Errorf("Usage.OutputTokens = %v, want 200", msg.Usage.OutputTokens)
	}
	if msg.Usage.CacheReadInputTokens != 30 {
		t.Errorf("Usage.CacheReadInputTokens = %v, want 30", msg.Usage.CacheReadInputTokens)
	}
	if msg.Usage.CostUSD != 0.001 {
		t.Errorf("Usage.CostUSD = %v, want 0.001", msg.Usage.CostUSD)
	}
	if msg.Text != "完成" {
		t.Errorf("Text = %q, want 完成", msg.Text)
	}
}

// TestTranslate_EventResult_NilUsageFine: a Result event with no
// usage (zero-usage turn / synthetic message) still translates.
// OutboundMessage.Usage stays nil — the runtime is a passive
// pass-through, so a nil Usage just means the channel footer
// omits Line 2 for this event.
func TestTranslate_EventResult_NilUsageFine(t *testing.T) {
	in := agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text:    "ok",
			Subtype: "success",
			// Usage intentionally nil
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("expected translate to emit")
	}
	if msg.Usage != nil {
		t.Errorf("msg.Usage = %+v, want nil (Result.Usage was nil)", msg.Usage)
	}
}

func TestTranslate_EventToolEnd_CarriesArgs(t *testing.T) {
	// F-34 review fix (architecture feedback 2026-08-04):
	// AgentToolEndEvent.Args flows through the Gateway into
	// OutboundMessage.Tool.Args (typed ToolInfo) so channel
	// renderers can produce type-aware thread-reply summaries
	// without mining Meta for implicit per-tool keys.
	in := agent.AgentEvent{
		Kind: agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{
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

// TestTranslate_EventCompaction removed: F-49 compaction tracking
// was deleted across the runtime. EventAgentCompaction kind no
// longer exists; no Translate case remains.

func TestTranslate_EventAgentConnected(t *testing.T) {
	in := agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: "s_001",
		Model:     "claude-sonnet-4-5",
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != OutInit {
		t.Fatalf("got (%v, %v), want (OutInit, true)", msg.Kind, ok)
	}
	// §1.4 cleanup: init fields flow through the typed
	// OutboundMessage.SessionID/Model/... flat fields (was
	// msg.Ready before event flattening).
	if msg.SessionID == "" {
		t.Fatal("msg.SessionID is empty; Gateway should populate from AgentEvent.SessionID")
	}
	if msg.SessionID != "s_001" {
		t.Errorf("Init.SessionID = %v, want 's_001'", msg.SessionID)
	}
	if msg.Model != "claude-sonnet-4-5" {
		t.Errorf("Init.Model = %v, want 'claude-sonnet-4-5'", msg.Model)
	}
}

// TestTranslate_EventAgentConnected_NilDropped removed: every
// EventAgentReady now stamps all 5 context fields at the bridge
// layer, so there is no "nil Ready payload" case to drop. An event
// with empty SessionID/Model is still rendered as OutInit
// (channel may want to surface "unknown model" gracefully).

func TestTranslate_EventDone_Dropped(t *testing.T) {
	// EventAgentDone is reflected in the receipt's terminal header — no
	// separate outbound message.
	if _, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}}); ok {
		t.Error("EventAgentDone should drop (no OutboundMessage)")
	}
}
