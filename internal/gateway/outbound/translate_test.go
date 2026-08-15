package outbound

import (
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestTranslate covers the AgentEvent → messages.OutboundMessage mapping
// for every EventKind that has its own OutboundKind (i.e. events
// that actually emit something). Terminal / dropped kinds
// (EventAgentDone, EventAgentError, default) are tested implicitly
// by the existing tests in cmd/gateway_test.go.
//
// Tests follow the pattern: build AgentEvent, call Translate,
// assert Kind + Text + key fields. We don't assert on every field —
// the bridge / translator contract is that producers + consumers
// agree on field names; consumers are the source of truth for which
// fields are mandatory (see feishu/adapter_test.go).

func TestTranslate_EventText(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentText, Text: "hello"})
	if !ok || msg.Kind != messages.OutReply || msg.Text != "hello" {
		t.Errorf("got (%v, %v) text=%q, want (OutReply=true, hello)", msg.Kind, ok, msg.Text)
	}
}

func TestTranslate_EventText_ThinkingPrefix(t *testing.T) {
	msg, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentText, Text: "[思考] thinking…"})
	if !ok || msg.Kind != messages.OutThinking || msg.Text != "thinking…" {
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
	if msg.Kind != messages.OutResult {
		t.Errorf("Kind = %v, want OutResult", msg.Kind)
	}
	if msg.Text != "完成" {
		t.Errorf("Text = %q, want 完成", msg.Text)
	}
	// §1.4 cleanup: Result fields flow through the typed
	// OutboundMessage.Result payload, not Meta.
	if msg.Result == nil {
		t.Fatal("msg.Result is nil; outbound should populate the typed AgentResultEvent payload")
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
	if !ok || msg.Kind != messages.OutResult {
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
	if msg.Kind != messages.OutResult {
		t.Errorf("Kind = %v, want OutResult", msg.Kind)
	}
	if msg.Usage == nil {
		t.Fatal("msg.Usage is nil; outbound should populate it from AgentResultEvent.Usage")
	}
	if msg.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %v, want 100", msg.Usage)
	}
	if msg.Usage.OutputTokens != 200 {
		t.Errorf("Usage.OutputTokens = %v, want 200", msg.Usage)
	}
	if msg.Usage.CacheReadInputTokens != 30 {
		t.Errorf("Usage.CacheReadInputTokens = %v, want 30", msg.Usage)
	}
	if msg.Usage.CostUSD != 0.001 {
		t.Errorf("Usage.CostUSD = %v, want 0.001", msg.Usage)
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
	// AgentToolEndEvent.Args flows through outbound into
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
	if !ok || msg.Kind != messages.OutToolEnd {
		t.Fatalf("got (%v, %v), want (OutToolEnd, true)", msg.Kind, ok)
	}
	if msg.Tool == nil {
		t.Fatal("msg.Tool is nil; outbound should populate the unified ToolInfo")
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

// TestTranslate_EventAgentConnected verifies that an EventAgentReady
// translates to OutInit with the 5 context fields populated.
func TestTranslate_EventAgentConnected(t *testing.T) {
	in := agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: "s_001",
		Model:     "claude-sonnet-4-5",
	}
	msg, ok := Translate("chat1", in)
	if !ok || msg.Kind != messages.OutInit {
		t.Fatalf("got (%v, %v), want (OutInit, true)", msg.Kind, ok)
	}
	// §1.4 cleanup: init fields flow through the typed
	// OutboundMessage.SessionID/Model/... flat fields (was
	// msg.Ready before event flattening).
	if msg.SessionID == "" {
		t.Fatal("msg.SessionID is empty; outbound should populate from AgentEvent.SessionID")
	}
	if msg.SessionID != "s_001" {
		t.Errorf("Init.SessionID = %v, want 's_001'", msg.SessionID)
	}
	if msg.Model != "claude-sonnet-4-5" {
		t.Errorf("Init.Model = %v, want 'claude-sonnet-4-5'", msg.Model)
	}
}

func TestTranslate_EventDone_Dropped(t *testing.T) {
	// EventAgentDone is reflected in the receipt's terminal header — no
	// separate outbound message.
	if _, ok := Translate("chat1", agent.AgentEvent{Kind: agent.EventAgentDone, Done: &agent.AgentDoneEvent{ExitCode: 0}}); ok {
		t.Error("EventAgentDone should drop (no OutboundMessage)")
	}
}
func TestTranslate_EventError_WithDiagnostic_EmitsOutError(t *testing.T) {
	// Bridge death with a populated Diagnostic must surface as a
	// dedicated OutError card so the user sees a clear "dsh
	// died because X" signal — pre-fix this was silently dropped.
	in := agent.AgentEvent{
		Kind:      agent.EventAgentError,
		AgentName: "dsh",
		Workspace: "/code",
		Err:       errors.New("dsh: lifecycle exit signal_killed: signal: killed\n--- stderr tail ---\nfoo bar"),
		Diagnostic: &agent.BridgeDiagnostic{
			ExitKind:   agent.BridgeExitSignalKilled,
			WaitErr:    errors.New("signal: killed"),
			StderrTail: "foo bar",
			SessionID:  "session-x",
			AgentName:  "dsh",
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("EventAgentError with Diagnostic should emit, got dropped")
	}
	if msg.Kind != messages.OutError {
		t.Errorf("kind = %v, want OutError", msg.Kind)
	}
	// Body should be the FIRST line of Err — long stderr tails
	// go via Diagnostic, not Text, so the card stays scannable.
	if msg.Text == "" {
		t.Error("OutError Text must be non-empty")
	}
	if strings.Contains(msg.Text, "stderr tail") {
		t.Errorf("OutError Text should be first-line only, got %q", msg.Text)
	}
	if msg.Diagnostic == nil || msg.Diagnostic.ExitKind != agent.BridgeExitSignalKilled {
		t.Errorf("Diagnostic should be propagated, got %+v", msg.Diagnostic)
	}
	if msg.AgentName != "dsh" {
		t.Errorf("AgentName = %q, want dsh (channel uses it for the card title)", msg.AgentName)
	}
}

func TestTranslate_EventError_NoDiagnostic_Drops(t *testing.T) {
	// Pre-Diagnostic-era bridges (and EventAgentError events
	// where the lifecycle couldn't classify the exit) keep the
	// legacy silent-drop behavior — we don't want to start
	// surfacing blank "bridge died" cards without the exit
	// kind.
	in := agent.AgentEvent{
		Kind: agent.EventAgentError,
		Err:  errors.New("plain error"),
	}
	if _, ok := Translate("chat1", in); ok {
		t.Error("EventAgentError without Diagnostic should drop (legacy behavior)")
	}
}

func TestTranslate_EventError_FallbackBodyFromDiagnostic(t *testing.T) {
	// Err is nil but Diagnostic is present — we synthesize a
	// short body from AgentName + ExitKind so the card is
	// never blank.
	in := agent.AgentEvent{
		Kind:      agent.EventAgentError,
		AgentName: "dsh",
		Diagnostic: &agent.BridgeDiagnostic{
			ExitKind:  agent.BridgeExitNonZeroExit,
			AgentName: "dsh",
		},
	}
	msg, ok := Translate("chat1", in)
	if !ok {
		t.Fatal("EventAgentError with Diagnostic should emit")
	}
	if msg.Text == "" {
		t.Error("body must be synthesized from Diagnostic when Err is nil")
	}
	if !strings.Contains(msg.Text, "dsh") || !strings.Contains(msg.Text, "non-zero-exit") {
		t.Errorf("synthesized body should mention agent + exit kind, got %q", msg.Text)
	}
}
