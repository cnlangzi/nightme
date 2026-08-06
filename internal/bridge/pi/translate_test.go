// Translation table tests for the Pi event -> agent.AgentEvent
// mapper. Each table-driven case feeds one raw event JSON into the
// translator and asserts the resulting AgentEvent stream matches
// the contract documented in docs/feat/F-32-pi-rpc-bridge.md §2.3.

package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func newTestTranslator() *translator {
	return newTranslator("pi", "/tmp/ws", "main")
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func kinds(events []agent.AgentEvent) []agent.EventKind {
	out := make([]agent.EventKind, len(events))
	for i, ev := range events {
		out[i] = ev.Kind
	}
	return out
}

// TestTranslate_AgentSettled verifies that the F-32 turn-end
// marker emits exactly one EventDone with Reason:"settled" and
// does NOT terminate the session (the runtime contract is that
// the events channel stays open across many turns).
func TestTranslate_AgentSettled(t *testing.T) {
	tr := newTestTranslator()
	events, err := tr.translate([]byte(`{"type":"agent_settled"}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Kind != agent.EventDone {
		t.Errorf("kind = %s, want done", events[0].Kind)
	}
	if events[0].Done == nil || events[0].Done.Reason != "settled" {
		t.Errorf("Done.Reason = %q, want settled", events0Reason(events[0]))
	}
}

// TestTranslate_AgentEndNotTerminal verifies that the lower-level
// "agent_end" event is NOT translated to a terminal event. Per
// Pi's docs, agent_end signals one low-level run; the session
// may retry / compact / continue. Only "agent_settled" marks a
// turn as fully done.
func TestTranslate_AgentEndNotTerminal(t *testing.T) {
	tr := newTestTranslator()
	events, err := tr.translate([]byte(`{"type":"agent_end","willRetry":false}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0; got %v", len(events), kinds(events))
	}
}

// TestTranslate_TextDelta verifies that text_delta events produce
// EventText payloads with the raw delta.
func TestTranslate_TextDelta(t *testing.T) {
	tr := newTestTranslator()
	events, err := tr.translate([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hello "}}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 1 || events[0].Kind != agent.EventText {
		t.Fatalf("unexpected events: %+v", events)
	}
	if events[0].Text != "hello " {
		t.Errorf("text = %q, want %q", events[0].Text, "hello ")
	}
}

// TestTranslate_ThinkingDelta verifies the "[思考] " prefix
// convention is applied to thinking deltas so the receipt renderer
// can branch on the leading bracket.
func TestTranslate_ThinkingDelta(t *testing.T) {
	tr := newTestTranslator()
	events, err := tr.translate([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"analyzing"}}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 1 || events[0].Text != "[思考] analyzing" {
		t.Errorf("thinking delta produced %+v", events)
	}
}

// TestTranslate_ToolExecutionStartEnd covers the happy-path
// start->end pair and confirms the IDs line up.
func TestTranslate_ToolExecutionStartEnd(t *testing.T) {
	tr := newTestTranslator()

	start, err := tr.translate([]byte(`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"command":"ls"}}`), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(start) != 1 || start[0].Kind != agent.EventToolStart {
		t.Fatalf("start = %+v", start)
	}
	if start[0].ToolStart.ID != "call-1" || start[0].ToolStart.Name != "bash" {
		t.Errorf("start payload: %+v", start[0].ToolStart)
	}

	end, err := tr.translate([]byte(`{"type":"tool_execution_end","toolCallId":"call-1","result":{"content":[{"type":"text","text":"file.txt"}]},"isError":false}`), nil)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if len(end) != 1 || end[0].Kind != agent.EventToolEnd {
		t.Fatalf("end = %+v", end)
	}
	if end[0].ToolEnd.ID != "call-1" {
		t.Errorf("end id = %q", end[0].ToolEnd.ID)
	}
	if end[0].ToolEnd.Err != nil {
		t.Errorf("err = %v, want nil", end[0].ToolEnd.Err)
	}
}

// TestTranslate_ToolErrorIsError verifies that isError=true on
// tool_execution_end sets ToolEnd.Err to a non-nil sentinel.
func TestTranslate_ToolErrorIsError(t *testing.T) {
	tr := newTestTranslator()
	events, err := tr.translate([]byte(`{"type":"tool_execution_end","toolCallId":"call-x","result":null,"isError":true}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if events[0].ToolEnd.Err == nil {
		t.Errorf("expected non-nil Err on isError=true")
	}
}

// TestTranslate_AssistantMessageResult verifies that the
// message_end assistant role produces ONE EventResult whose
// ResultEvent carries Usage inline (co-located usage instead of
// the legacy EventResult + EventUsage pair).
func TestTranslate_AssistantMessageResult(t *testing.T) {
	tr := newTestTranslator()
	raw := mustMarshal(t, map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content": []map[string]any{
				{"type": "text", "text": "hi"},
			},
			"usage": map[string]any{
				"input": 10, "output": 5, "cacheRead": 1, "cacheWrite": 0, "totalTokens": 16,
				"cost": map[string]any{"input": 0.01, "output": 0.02, "total": 0.03},
			},
		},
	})
	events, err := tr.translate(raw, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	// Single event — usage rides on the ResultEvent, not as a
	// peer. The bridge-layer "split then rejoin at runtime" trick
	// is gone.
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (Result with co-located Usage): %v", len(events), kinds(events))
	}
	if events[0].Kind != agent.EventResult {
		t.Errorf("kind = %s, want result", events[0].Kind)
	}
	if events[0].Result.Text != "hi" || events[0].Result.IsError {
		t.Errorf("result = %+v", events[0].Result)
	}
	u := events[0].Result.Usage
	if u == nil {
		t.Fatal("ResultEvent.Usage is nil; bridge should populate from message_end.usage")
	}
	if u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Errorf("usage = %+v", u)
	}
	if u.CostUSD != 0.03 {
		t.Errorf("CostUSD = %f, want 0.03", u.CostUSD)
	}
}

// TestTranslate_EmptyUsageStaysNil verifies that a zero-totals
// usage block (synthetic messages, etc.) does not produce a
// non-nil-but-all-zero Usage — the runtime skips AccumulateUsage
// on nil and the channel renders no footer.
func TestTranslate_EmptyUsageStaysNil(t *testing.T) {
	tr := newTestTranslator()
	raw := mustMarshal(t, map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content":    []map[string]any{{"type": "text", "text": "ok"}},
		},
	})
	events, err := tr.translate(raw, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (no usage field): %v", len(events), kinds(events))
	}
	if events[0].Kind != agent.EventResult {
		t.Errorf("kind = %s, want result", events[0].Kind)
	}
	if events[0].Result.Usage != nil {
		t.Errorf("ResultEvent.Usage = %+v, want nil (no usage section on the wire)", events[0].Result.Usage)
	}
}

// TestTranslate_CompactionEndOnly verifies F-49 bridge abstraction:
// a complete Pi compaction cycle (start+end) yields exactly one
// EventCompaction on the wire. The start event is silently
// suppressed (the runtime would otherwise double-count). See
// docs/feat/F-49-compaction-counter.md §1.3 + §1.7.
func TestTranslate_CompactionEndOnly(t *testing.T) {
	tr := newTestTranslator()

	// compaction_start → suppressed (returns nil, nil).
	start, err := tr.translate([]byte(`{"type":"compaction_start","reason":"threshold"}`), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if start != nil {
		t.Errorf("compaction_start should be suppressed, got %v events", len(start))
	}

	// compaction_end → one EventCompaction (empty marker payload).
	end, err := tr.translate([]byte(`{"type":"compaction_end","reason":"threshold","result":{"aborted":false}}`), nil)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if len(end) != 1 {
		t.Fatalf("compaction_end produced %d events, want 1", len(end))
	}
	if end[0].Kind != agent.EventCompaction {
		t.Errorf("end kind = %s, want EventCompaction", end[0].Kind)
	}
}

// TestTranslate_ExtensionUIRequestIgnored verifies that the F-32
// MVP does not produce any AgentEvent for an extension_ui_request
// (the session auto-replies cancelled on a different path).
func TestTranslate_ExtensionUIRequestIgnored(t *testing.T) {
	tr := newTestTranslator()
	events, err := tr.translate([]byte(`{"type":"extension_ui_request","id":"u1","method":"select","title":"pick","options":["a","b"]}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("events = %v, want none (auto-cancelled path handles replies)", kinds(events))
	}
}

// TestTranslate_UnknownEventIgnored verifies that an unknown event
// type does not produce an error or an event. The bridge stays
// robust against upstream additions.
func TestTranslate_UnknownEventIgnored(t *testing.T) {
	tr := newTestTranslator()
	events, err := tr.translate([]byte(`{"type":"future_event","payload":{"x":1}}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("events = %v, want none for unknown type", kinds(events))
	}
}

// TestTranslate_MalformedJSON verifies that a non-JSON frame is
// reported as an error so the session can tear down.
func TestTranslate_MalformedJSON(t *testing.T) {
	tr := newTestTranslator()
	_, err := tr.translate([]byte(`not json`), nil)
	if err == nil {
		t.Errorf("expected error on malformed JSON")
	}
}

// TestEmitInit_Once verifies that emitInit only fires the first
// time. Subsequent get_state responses (e.g. after a model switch
// in a future MVP) do not re-emit EventInit and corrupt the
// receipt header.
func TestEmitInit_Once(t *testing.T) {
	tr := newTestTranslator()
	state := &getStateResult{
		SessionID: "sess-1",
		Model:     &getStateModel{ID: "m1", Name: "M1"},
	}
	first := tr.emitInit(state)
	if len(first) != 1 {
		t.Fatalf("first emitInit = %d, want 1", len(first))
	}
	if first[0].Init.SessionID != "sess-1" || first[0].Init.AgentName != "pi" {
		t.Errorf("init = %+v", first[0].Init)
	}
	if !strings.Contains(first[0].Init.Model, "M1") {
		t.Errorf("Model = %q, want to contain M1", first[0].Init.Model)
	}
	second := tr.emitInit(state)
	if second != nil {
		t.Errorf("second emitInit = %+v, want nil", second)
	}
}

// TestEmitInit_NoModel verifies that init still fires with empty
// model + session id when the get_state response was empty (Pi
// in early-startup state).
func TestEmitInit_NoModel(t *testing.T) {
	tr := newTestTranslator()
	first := tr.emitInit(nil)
	if len(first) != 1 {
		t.Fatalf("emitInit(nil) = %d, want 1", len(first))
	}
	if first[0].Init.Model != "" || first[0].Init.SessionID != "" {
		t.Errorf("empty init = %+v", first[0].Init)
	}
	if first[0].Init.AgentName != "pi" || first[0].Init.Workspace != "/tmp/ws" {
		t.Errorf("agent/workspace = %+v", first[0].Init)
	}
}

// TestTranslate_SilentDropLogging verifies that ignored events are
// not surfaced as AgentEvents but are logged at debug so users
// with --verbose can see them in the bridge log.
func TestTranslate_SilentDropLogging(t *testing.T) {
	tr := newTestTranslator()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	events, err := tr.translate([]byte(`{"type":"agent_start"}`), logger)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("agent_start must be ignored, got %v", kinds(events))
	}
	if !strings.Contains(buf.String(), "pi event ignored") {
		t.Errorf("expected debug log, got: %q", buf.String())
	}
}

// events0Reason is a tiny helper that returns the Reason of the
// first event or "<nil>" if Done is nil.
func events0Reason(ev agent.AgentEvent) string {
	if ev.Done == nil {
		return "<nil>"
	}
	return ev.Done.Reason
}

// TestTranslate_ToolCallStartNoop verifies the F-32 design
// decision: toolcall_start is a no-op because the canonical
// EventToolStart is emitted at tool_execution_start, which
// arrives later in the event stream with the full name + args.
// Emitting twice would surface a phantom "starting" line
// between two genuine tool invocations.
func TestTranslate_ToolCallStartNoop(t *testing.T) {
	tr := newTestTranslator()
	raw := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_start","toolCallId":"c-1","name":"read","partial":{"id":"c-1","name":"read","arguments":{"path":"/etc"}}}}`)
	events, err := tr.translate(raw, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("toolcall_start must be no-op, got %v", events)
	}
}

// TestTranslate_ToolExecutionStartArgs verifies that the
// canonical EventToolStart is emitted from tool_execution_start
// with the full name and args (the slot the renderer listens on
// for "tool starting" lines).
func TestTranslate_ToolExecutionStartArgs(t *testing.T) {
	tr := newTestTranslator()
	raw := []byte(`{"type":"tool_execution_start","toolCallId":"c-1","toolName":"read","args":{"path":"/etc"}}`)
	events, err := tr.translate(raw, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 1 || events[0].Kind != agent.EventToolStart {
		t.Fatalf("events = %+v", events)
	}
	if events[0].ToolStart.ID != "c-1" || events[0].ToolStart.Name != "read" {
		t.Errorf("payload = %+v", events[0].ToolStart)
	}
	if !strings.Contains(events[0].ToolStart.Args, "/etc") {
		t.Errorf("Args = %q", events[0].ToolStart.Args)
	}
}

// TestTranslate_RejectsMalformed is the negative counterpart of
// TestTranslate_MalformedJSON: nested malformed payloads from
// message_update must also surface as errors.
func TestTranslate_RejectsMalformed(t *testing.T) {
	tr := newTestTranslator()
	// assistantMessageEvent is a JSON string instead of object.
	_, err := tr.translate([]byte(`{"type":"message_update","assistantMessageEvent":"oops"}`), nil)
	if err == nil {
		t.Errorf("expected error on nested malformed payload")
	}
}

// silence unused import warnings if all errors-typed helpers are
// trimmed in future revisions.
var _ = errors.New

// TestTranslate_StateUpdate_EmitsEventInit verifies F-34 §3.2.2:
// when pi emits state_update after a new_session RPC, the translator
// surfaces an EventInit carrying the new sessionId. The runtime's
// eventHandler (cmd/nightme/run.go newEventHandler) picks it up
// via SetResumeID.
func TestTranslate_StateUpdate_EmitsEventInit(t *testing.T) {
	tr := newTestTranslator()
	raw := []byte(`{"type":"state_update","sessionId":"new-sess-1","modelId":"m1","modelName":"M1"}`)
	events, err := tr.translate(raw, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Kind != agent.EventInit {
		t.Fatalf("Kind = %s, want EventInit", events[0].Kind)
	}
	if events[0].Init.SessionID != "new-sess-1" {
		t.Errorf("SessionID = %q, want new-sess-1", events[0].Init.SessionID)
	}
	if !strings.Contains(events[0].Init.Model, "M1") {
		t.Errorf("Model = %q, want to contain M1", events[0].Init.Model)
	}
	if events[0].Init.AgentName != "pi" {
		t.Errorf("AgentName = %q, want pi", events[0].Init.AgentName)
	}
}

// TestTranslate_StateUpdate_NoSessionID_Ignored verifies that
// state_update events without a sessionId are silently dropped
// (defensive: pi may emit various state_update flavors).
func TestTranslate_StateUpdate_NoSessionID_Ignored(t *testing.T) {
	tr := newTestTranslator()
	raw := []byte(`{"type":"state_update","modelId":"m1"}`)
	events, err := tr.translate(raw, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want 0", len(events))
	}
}
