// Translation table tests for the Pi event -> agent.AgentEvent
// mapper. Each table-driven case feeds one raw event JSON into the
// translator and asserts the resulting AgentEvent stream matches
// the contract documented in docs/feat/F-32-pi-rpc-bridge.md §2.3.

package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
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

// TestTranslate_ToolExecutionEnd_FillsNameAndArgs verifies P0:
// start→end correlation populates ToolEnd.Name + ToolEnd.Args from
// the in-flight pendingTools map (recorded in tool_execution_start).
// Without this, the renderer falls back to "🔧 tool → N bytes"
// and loses the type-aware summary.
//
// Mirrors the claudecode streamState.pendingTools contract — see
// internal/bridge/claudecode/stream.go. The argument order matches
// the wire: tool_execution_end carries toolName as a redundant
// signal, but args only travel on start; the bridge has to glue
// them back together.
func TestTranslate_ToolExecutionEnd_FillsNameAndArgs(t *testing.T) {
	tr := newTestTranslator()
	start := []byte(`{"type":"tool_execution_start","toolCallId":"c-1","toolName":"bash","args":{"command":"ls -la"}}`)
	if _, err := tr.translate(start, nil); err != nil {
		t.Fatalf("start translate: %v", err)
	}

	end := []byte(`{"type":"tool_execution_end","toolCallId":"c-1","toolName":"bash","result":[{"type":"text","text":"total 4"}],"isError":false}`)
	events, err := tr.translate(end, nil)
	if err != nil {
		t.Fatalf("end translate: %v", err)
	}
	if len(events) != 1 || events[0].Kind != agent.EventToolEnd {
		t.Fatalf("events = %+v", events)
	}
	toolEnd := events[0].ToolEnd
	if toolEnd.ID != "c-1" {
		t.Errorf("ID = %q, want c-1", toolEnd.ID)
	}
	if toolEnd.Name != "bash" {
		t.Errorf("Name = %q, want bash (from start)", toolEnd.Name)
	}
	if !strings.Contains(toolEnd.Args, "ls -la") {
		t.Errorf("Args = %q, want to contain ls -la (from start)", toolEnd.Args)
	}
	if !strings.Contains(toolEnd.Output, "total 4") {
		t.Errorf("Output = %q", toolEnd.Output)
	}
	if toolEnd.Err != nil {
		t.Errorf("Err = %v, want nil", toolEnd.Err)
	}

	// Pending entry must be cleared so a later tool with the same
	// id (Pi reusing toolCallIds across turns is unlikely but
	// defensible) does not pick up stale args.
	if _, ok := tr.pendingTools["c-1"]; ok {
		t.Errorf("pendingTools[c-1] = %+v, want removed", tr.pendingTools["c-1"])
	}
}

// TestTranslate_ToolExecutionEnd_WireToolNameFallback covers the
// orphan end path: a tool_execution_end arrives without a matching
// start (e.g. tool started before the bridge attached, or pump
// reordered). The wire still carries toolName, so the renderer
// gets a usable Name; Args stays empty by design (we have no source
// of truth). The renderer falls back to displaying "(no args)" for
// empty Args, which is preferable to mis-attributes via a stale
// pending entry.
func TestTranslate_ToolExecutionEnd_WireToolNameFallback(t *testing.T) {
	tr := newTestTranslator()
	end := []byte(`{"type":"tool_execution_end","toolCallId":"c-orphan","toolName":"write","result":[{"type":"text","text":"ok"}],"isError":false}`)
	events, err := tr.translate(end, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 1 || events[0].Kind != agent.EventToolEnd {
		t.Fatalf("events = %+v", events)
	}
	toolEnd := events[0].ToolEnd
	if toolEnd.Name != "write" {
		t.Errorf("Name = %q, want write (from wire)", toolEnd.Name)
	}
	if toolEnd.Args != "" {
		t.Errorf("Args = %q, want empty (no pending)", toolEnd.Args)
	}
	if !strings.Contains(toolEnd.Output, "ok") {
		t.Errorf("Output = %q, want to contain ok", toolEnd.Output)
	}
}

// TestTranslate_AgentSettled_ClearsPendingTools verifies the
// defensive cleanup: agent_settled must drop any in-flight
// pending entries so a missed tool_execution_end (e.g. Pi aborted
// mid-call, a future wire event class we don't yet handle) cannot
// leak across turns and cause a later tool's end to inherit
// stale Name/Args from the prior turn.
func TestTranslate_AgentSettled_ClearsPendingTools(t *testing.T) {
	tr := newTestTranslator()
	start := []byte(`{"type":"tool_execution_start","toolCallId":"c-1","toolName":"bash","args":{"command":"sleep 9999"}}`)
	if _, err := tr.translate(start, nil); err != nil {
		t.Fatalf("start translate: %v", err)
	}
	if len(tr.pendingTools) != 1 {
		t.Fatalf("pendingTools len = %d, want 1", len(tr.pendingTools))
	}

	if _, err := tr.translate([]byte(`{"type":"agent_settled"}`), nil); err != nil {
		t.Fatalf("settled translate: %v", err)
	}
	if len(tr.pendingTools) != 0 {
		t.Errorf("pendingTools len after settled = %d, want 0", len(tr.pendingTools))
	}
}

// TestTranslate_EmptyToolCallId_OrphanPath covers Finding 2 from
// the post-merge code-review: malformed or partial wire events
// with empty toolCallId must not collapse into a single map entry.
// Two empty-ID starts must not overwrite each other; the matching
// empty-ID end uses wire ToolName + empty Args, never inheriting
// Name/Args from a different tool.
func TestTranslate_EmptyToolCallId_OrphanPath(t *testing.T) {
	tr := newTestTranslator()

	// First empty-id start: does NOT record in pendingTools.
	start1 := []byte(`{"type":"tool_execution_start","toolCallId":"","toolName":"bash","args":{"command":"ls"}}`)
	evs1, err := tr.translate(start1, nil)
	if err != nil {
		t.Fatalf("start1 translate: %v", err)
	}
	if len(evs1) != 1 || evs1[0].Kind != agent.EventToolStart {
		t.Fatalf("start1 events = %+v", evs1)
	}
	if evs1[0].ToolStart.Name != "bash" {
		t.Errorf("start1 Name = %q, want bash from wire", evs1[0].ToolStart.Name)
	}
	if _, ok := tr.pendingTools[""]; ok {
		t.Errorf("pendingTools[\"\"] must not be set on empty-id start; got %+v", tr.pendingTools[""])
	}

	// Second empty-id start with a different toolName would have
	// overwritten a stale entry under "" — verify it didn't.
	start2 := []byte(`{"type":"tool_execution_start","toolCallId":"","toolName":"read","args":{"path":"/etc"}}`)
	if _, err := tr.translate(start2, nil); err != nil {
		t.Fatalf("start2 translate: %v", err)
	}
	if _, ok := tr.pendingTools[""]; ok {
		t.Errorf("pendingTools[\"\"] must remain unset on second empty-id start; got %+v", tr.pendingTools[""])
	}

	// Empty-id end falls back to wire ToolName; never inherits the
	// (different) Args from a previous non-empty-id start.
	prior := []byte(`{"type":"tool_execution_start","toolCallId":"c-real","toolName":"write","args":{"path":"/x"}}`)
	if _, err := tr.translate(prior, nil); err != nil {
		t.Fatalf("prior (real id) start translate: %v", err)
	}
	end := []byte(`{"type":"tool_execution_end","toolCallId":"","toolName":"bash","result":"x","isError":false}`)
	evsEnd, err := tr.translate(end, nil)
	if err != nil {
		t.Fatalf("end translate: %v", err)
	}
	if len(evsEnd) != 1 || evsEnd[0].Kind != agent.EventToolEnd {
		t.Fatalf("end events = %+v", evsEnd)
	}
	toolEnd := evsEnd[0].ToolEnd
	if toolEnd.Name != "bash" {
		t.Errorf("end Name = %q, want bash (from wire, orphan end)", toolEnd.Name)
	}
	if toolEnd.Args != "" {
		t.Errorf("end Args = %q, want empty (orphan path did not touch pendingTools)", toolEnd.Args)
	}

	// Sanity: the real-id prior entry must still be in the map
	// (not disturbed by any empty-id events).
	if _, ok := tr.pendingTools["c-real"]; !ok {
		t.Errorf("pendingTools[c-real] = missing, want present (real-id entry should survive)")
	}
}

// TestTranslate_PendingTools_ConcurrentAccess is a -race guard for
// Finding 1: pendingTools is read in translate() and reassigned
// in session.New() (on /new) from different goroutines. Hit the
// translator hard from many goroutines while another calls the
// internal reset path equivalent; -race should produce no report.
// Without pendingMu the runtime would race-panic with
// "concurrent map read and map write".
func TestTranslate_PendingTools_ConcurrentAccess(t *testing.T) {
	tr := newTestTranslator()

	const goroutines = 16
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines + 1)

	// Producer goroutines hammer translate() with start/end pairs.
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				toolID := fmt.Sprintf("g%d-i%d", id, i)
				start := []byte(fmt.Sprintf(
					`{"type":"tool_execution_start","toolCallId":%q,"toolName":"bash","args":{"i":%d}}`,
					toolID, i,
				))
				if _, err := tr.translate(start, nil); err != nil {
					t.Errorf("start translate: %v", err)
					return
				}
				end := []byte(fmt.Sprintf(
					`{"type":"tool_execution_end","toolCallId":%q,"toolName":"bash","result":"ok","isError":false}`,
					toolID,
				))
				if _, err := tr.translate(end, nil); err != nil {
					t.Errorf("end translate: %v", err)
					return
				}
			}
		}(g)
	}

	// Reset goroutine periodically clears the map (simulating
	// /new). It MUST take pendingMu the same way session.New does.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			tr.pendingMu.Lock()
			tr.pendingTools = make(map[string]pendingTool)
			tr.pendingMu.Unlock()
			runtime.Gosched()
		}
	}()

	wg.Wait()
	// After the storm, the map should be empty (every end drained,
	// last reset cleared any leftovers).
	if len(tr.pendingTools) != 0 {
		t.Errorf("after concurrent storm + reset, pendingTools len = %d, want 0", len(tr.pendingTools))
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
