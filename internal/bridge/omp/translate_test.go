// Translation table tests for the omp event -> agent.AgentEvent
// mapper. Each table-driven case feeds one raw event JSON
// into the translator and asserts the resulting AgentEvent
// stream matches the contract documented in issue #443
// (R5/R6/R7/R8).

package omp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func newTestTranslator() *translator {
	return newTranslator("omp", "/tmp/ws", "main")
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

func mustTranslate(t *testing.T, tr *translator, raw string) []agent.AgentEvent {
	t.Helper()
	events, err := tr.translate([]byte(raw), nil)
	if err != nil {
		t.Fatalf("translate(%s): %v", raw, err)
	}
	return events
}

func drive(t *testing.T, tr *translator, raws ...string) []agent.AgentEvent {
	t.Helper()
	var out []agent.AgentEvent
	for _, raw := range raws {
		out = append(out, mustTranslate(t, tr, raw)...)
	}
	return out
}

func findResult(t *testing.T, events []agent.AgentEvent) *agent.AgentResultEvent {
	t.Helper()
	var found *agent.AgentResultEvent
	for _, ev := range events {
		if ev.Kind != agent.EventAgentResult {
			continue
		}
		if found != nil {
			t.Fatalf("more than one EventAgentResult in %v", kinds(events))
		}
		if ev.Result == nil {
			t.Fatal("EventAgentResult with nil Result payload")
		}
		found = ev.Result
	}
	if found == nil {
		t.Fatalf("no EventAgentResult in %v", kinds(events))
	}
	return found
}

func texts(events []agent.AgentEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind == agent.EventAgentText {
			out = append(out, ev.Text)
		}
	}
	return out
}

func textDeltas(idx int, chunks ...string) []string {
	out := make([]string, 0, len(chunks)+2)
	out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":%d}}`, idx))
	for _, c := range chunks {
		out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":%d,"delta":%q}}`, idx, c))
	}
	out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":%d}}`, idx))
	return out
}

func thinkingDeltas(idx int, chunks ...string) []string {
	out := make([]string, 0, len(chunks)+3)
	out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_start","contentIndex":%d}}`, idx))
	for _, c := range chunks {
		out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":%d,"delta":%q}}`, idx, c))
	}
	out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_end","contentIndex":%d}}`, idx))
	return out
}

func assistantMessageEndWithUsage(t *testing.T, input, output, cacheRead, cacheWrite int) string {
	t.Helper()
	msg := map[string]any{
		"role":    "assistant",
		"model":   "claude-sonnet-4-5",
		"content": []map[string]any{},
		"usage": map[string]any{
			"input":       input,
			"output":      output,
			"cacheRead":   cacheRead,
			"cacheWrite":  cacheWrite,
			"totalTokens": input + output + cacheRead + cacheWrite,
			"cost":        map[string]any{"input": 0.001, "output": 0.002, "cacheRead": 0, "cacheWrite": 0, "total": 0.003},
		},
	}
	return string(mustMarshal(t, map[string]any{"type": "message_end", "message": msg}))
}

// TestTranslate_AgentEndTerminal asserts R8: agent_end with
// isTerminal == true produces exactly one EventAgentResult
// followed by exactly one EventAgentDone (Reason: settled).
func TestTranslate_AgentEndTerminal(t *testing.T) {
	tr := newTestTranslator()
	events := drive(t, tr, append(textDeltas(0, "ok"), `{"type":"agent_end","isTerminal":true}`)...)

	if len(events) != 2 {
		t.Fatalf("events = %v, want [result done]", kinds(events))
	}
	if events[0].Kind != agent.EventAgentResult {
		t.Errorf("events[0] kind = %s, want result", events[0].Kind)
	}
	if events[1].Kind != agent.EventAgentDone {
		t.Errorf("events[1] kind = %s, want done", events[1].Kind)
	}
	if events[1].Done == nil || events[1].Done.Reason != "settled" {
		t.Errorf("Done.Reason = %q, want settled", events[1].Done.Reason)
	}
}

// TestTranslate_AgentEndIsTerminalFalse asserts R8: an
// agent_end with isTerminal == false is ignored entirely.
func TestTranslate_AgentEndIsTerminalFalse(t *testing.T) {
	tr := newTestTranslator()
	frames := []string{}
	frames = append(frames, textDeltas(0, "first")...)
	frames = append(frames,
		`{"type":"agent_end","isTerminal":false}`,
	)
	frames = append(frames, textDeltas(1, "tail")...)
	frames = append(frames,
		`{"type":"agent_end","isTerminal":true}`,
	)
	events := drive(t, tr, frames...)

	// exactly one Result + one Done
	resultCount, doneCount := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentResult:
			resultCount++
		case agent.EventAgentDone:
			doneCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("EventAgentResult count = %d, want 1", resultCount)
	}
	if doneCount != 1 {
		t.Errorf("EventAgentDone count = %d, want 1", doneCount)
	}
}

// TestTranslate_AgentEndDefaultIsTerminal asserts that an
// agent_end event without an isTerminal field is treated as
// terminal (R8 contract: "non-false == terminal").
func TestTranslate_AgentEndDefaultIsTerminal(t *testing.T) {
	tr := newTestTranslator()
	frames := append(textDeltas(0, "ok"), `{"type":"agent_end"}`)
	events := drive(t, tr, frames...)

	if len(events) != 2 {
		t.Fatalf("events = %v, want [result done]", kinds(events))
	}
}

// TestTranslate_TextStartResetsBuffer ensures text_start
// discards any stale buffer that might have leaked from a
// previous text_end that was missed.
func TestTranslate_TextStartResetsBuffer(t *testing.T) {
	tr := newTestTranslator()
	tr.turn.textBuf[0] = &strings.Builder{}
	tr.turn.textBuf[0].WriteString("old ")
	events := mustTranslate(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`)
	if len(events) != 0 {
		t.Errorf("text_start emitted %v, want no events", kinds(events))
	}
	if _, ok := tr.turn.textBuf[0]; ok {
		t.Errorf("text_start did not clear textBuf[0]")
	}
}

// TestTranslate_TextDeltaAccumulates asserts F-52-style
// accumulation: text_delta emits no events (buffers only).
func TestTranslate_TextDeltaAccumulates(t *testing.T) {
	tr := newTestTranslator()
	events := mustTranslate(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hello "}}`)
	if len(events) != 0 {
		t.Fatalf("text_delta emitted %v, want no events", kinds(events))
	}
	if got := tr.turn.textBuf[0].String(); got != "hello " {
		t.Errorf("textBuf[0] = %q, want %q", got, "hello ")
	}
}

// TestTranslate_TextEndMovesToPending asserts that text_end
// moves the accumulated buffer into pendingText, awaiting
// either the tool boundary or the terminal event.
func TestTranslate_TextEndMovesToPending(t *testing.T) {
	tr := newTestTranslator()
	drive(t, tr, textDeltas(0, "hello ", "world")...)
	if got := tr.turn.pendingText; got != "hello world" {
		t.Errorf("pendingText = %q, want %q", got, "hello world")
	}
	if tr.turn.pendingText == "" {
		t.Fatal("pendingText empty after text_end")
	}
	if _, ok := tr.turn.textBuf[0]; ok {
		t.Errorf("textBuf[0] should be removed after text_end")
	}
}

// TestTranslate_UnknownBetweenTextDeltas asserts R5:
// an unknown event inserted between two text_deltas does not
// split the buffered text. (Without this invariant, an
// unknown event type would corrupt reply rendering.)
func TestTranslate_UnknownBetweenTextDeltas(t *testing.T) {
	tr := newTestTranslator()
	drive(t, tr,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"abc"}}`,
		`{"type":"some_unknown_thing"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"def"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":0}}`,
	)
	if got := tr.turn.pendingText; got != "abcdef" {
		t.Errorf("pendingText = %q, want %q", got, "abcdef")
	}
}

// TestTranslate_ThinkingEnd emits EventAgentThinking.
func TestTranslate_ThinkingEnd(t *testing.T) {
	tr := newTestTranslator()
	events := drive(t, tr, thinkingDeltas(0, "reasoning...")...)
	if len(events) != 1 || events[0].Kind != agent.EventAgentThinking {
		t.Fatalf("events = %v, want [thinking]", kinds(events))
	}
	if events[0].Text != "reasoning..." {
		t.Errorf("Text = %q, want %q", events[0].Text, "reasoning...")
	}
}

// TestTranslate_ThinkingAndTextIndependent asserts R6:
// text_buf and think_buf are independent — neither flushes
// the other on *end.
func TestTranslate_ThinkingAndTextIndependent(t *testing.T) {
	tr := newTestTranslator()
	// Drive thinking first.
	drive(t, tr, thinkingDeltas(0, "thinking")...)
	// Then text.
	drive(t, tr, textDeltas(1, "reply")...)
	if tr.turn.pendingText != "reply" {
		t.Errorf("pendingText = %q, want %q", tr.turn.pendingText, "reply")
	}
}

// TestTranslate_ToolStartFlushesPending asserts R5 / F-52: a
// tool_execution_start flushes the buffered narration as a
// single EventAgentText so the user sees what the agent is
// about to do.
func TestTranslate_ToolStartFlushesPending(t *testing.T) {
	tr := newTestTranslator()
	drive(t, tr, textDeltas(0, "let me think...")...)
	events := drive(t, tr, `{"type":"tool_execution_start","toolCallId":"c1","toolName":"read","intent":"Reading notes.txt","args":{"path":"/x"}}`)
	if len(events) != 2 {
		t.Fatalf("events = %v, want [text tool_start]", kinds(events))
	}
	if events[0].Kind != agent.EventAgentText || events[0].Text != "let me think..." {
		t.Errorf("events[0] = %s/%q, want text/let me think...", events[0].Kind, events[0].Text)
	}
	if events[1].Kind != agent.EventAgentToolStart {
		t.Errorf("events[1] = %s, want tool_start", events[1].Kind)
	}
	if events[1].ToolStart.Name != "Reading notes.txt" {
		t.Errorf("ToolStart.Name = %q, want intent-preferred", events[1].ToolStart.Name)
	}
	if tr.turn.pendingText != "" {
		t.Errorf("pendingText = %q, want empty after flush", tr.turn.pendingText)
	}
}

// TestTranslate_ToolEndCorrelatesArgs verifies tool
// correlation: a tool_execution_end that follows a recorded
// tool_execution_start picks up the start-time args (omp's
// wire does not echo args on end). The wire's own ToolName
// wins on the end event — the issue spec's R5 deliberately
// keeps the end payload tool-name, not the intent-derived
// title from the start.
func TestTranslate_ToolEndCorrelatesArgs(t *testing.T) {
	tr := newTestTranslator()
	drive(t, tr,
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"read","intent":"Reading foo","args":{"path":"/x"}}`,
	)
	events := drive(t, tr,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"read","result":"ok"}`,
	)
	if len(events) != 1 || events[0].Kind != agent.EventAgentToolEnd {
		t.Fatalf("events = %v, want [tool_end]", kinds(events))
	}
	if events[0].ToolEnd.Name != "read" {
		t.Errorf("ToolEnd.Name = %q, want wire ToolName", events[0].ToolEnd.Name)
	}
	if events[0].ToolEnd.Args != `{"path":"/x"}` {
		t.Errorf("ToolEnd.Args = %q, want raw start args (correlation)", events[0].ToolEnd.Args)
	}
}

// TestTranslate_ToolEndIsError verifies isError -> Err
// propagation.
func TestTranslate_ToolEndIsError(t *testing.T) {
	tr := newTestTranslator()
	events := drive(t, tr,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"read","result":"oops","isError":true}`,
	)
	if len(events) != 1 {
		t.Fatalf("events = %v, want 1 event", kinds(events))
	}
	if events[0].Err == nil {
		t.Errorf("Err = nil, want non-nil tool error")
	}
}

// TestTranslate_MessageEndUsageOverwrites asserts R7: usage
// is an overwriting snapshot. The second message_end's usage
// replaces (not adds) the first.
func TestTranslate_MessageEndUsageOverwrites(t *testing.T) {
	tr := newTestTranslator()
	// First message_end: small usage.
	drive(t, tr, assistantMessageEndWithUsage(t, 100, 50, 0, 0))
	if tr.turn.lastUsage == nil || tr.turn.lastUsage.InputTokens != 100 {
		t.Fatalf("first usage not recorded: %+v", tr.turn.lastUsage)
	}
	// Second message_end: different usage.
	drive(t, tr, assistantMessageEndWithUsage(t, 7000, 4000, 100, 200))
	if tr.turn.lastUsage == nil {
		t.Fatal("second usage not recorded")
	}
	if tr.turn.lastUsage.InputTokens != 7000 {
		t.Errorf("InputTokens = %d, want 7000 (overwrite, not sum)", tr.turn.lastUsage.InputTokens)
	}
	if tr.turn.lastUsage.OutputTokens != 4000 {
		t.Errorf("OutputTokens = %d, want 4000 (overwrite)", tr.turn.lastUsage.OutputTokens)
	}
	if tr.turn.lastUsage.CacheReadInputTokens != 100 {
		t.Errorf("CacheReadInputTokens = %d, want 100", tr.turn.lastUsage.CacheReadInputTokens)
	}
	if tr.turn.lastUsage.CacheCreationInputTokens != 200 {
		t.Errorf("CacheCreationInputTokens = %d, want 200", tr.turn.lastUsage.CacheCreationInputTokens)
	}
}

// TestTranslate_MessageEndUsageCostUSD asserts that the
// result.usage.CostUSD carries the wire's cost.total.
func TestTranslate_MessageEndUsageCostUSD(t *testing.T) {
	tr := newTestTranslator()
	drive(t, tr, assistantMessageEndWithUsage(t, 10, 20, 0, 0))
	if tr.turn.lastUsage == nil || tr.turn.lastUsage.CostUSD != 0.003 {
		t.Fatalf("CostUSD = %v, want 0.003", tr.turn.lastUsage.CostUSD)
	}
}

// TestTranslate_SingleTurnSingleResult asserts the F-52
// invariant: one agent_end{isTerminal:true} per turn yields
// exactly one EventAgentResult and one EventAgentDone, even
// when the turn saw multiple text segments + multiple tool
// calls + multiple message_ends.
func TestTranslate_SingleTurnSingleResult(t *testing.T) {
	tr := newTestTranslator()
	// Build the whole frame list first then drive; the
	// variadic + spread pattern keeps the call readable.
	frames := []string{}
	frames = append(frames, textDeltas(0, "let me check...")...)
	frames = append(frames, `{"type":"message_end","message":{"role":"assistant","model":"m","content":[]}}`)
	frames = append(frames,
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"read","intent":"Reading foo"}`,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"read"}`,
	)
	frames = append(frames, textDeltas(1, "now I will write")...)
	frames = append(frames,
		`{"type":"message_end","message":{"role":"assistant","model":"m","content":[]}}`,
		`{"type":"tool_execution_start","toolCallId":"c2","toolName":"write","intent":"Writing bar"}`,
		`{"type":"tool_execution_end","toolCallId":"c2","toolName":"write"}`,
	)
	frames = append(frames, textDeltas(2, "all done")...)
	frames = append(frames,
		`{"type":"message_end","message":{"role":"assistant","model":"m","content":[]}}`,
		`{"type":"agent_end","isTerminal":true}`,
	)
	events := drive(t, tr, frames...)

	resultCount, doneCount := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentResult:
			resultCount++
		case agent.EventAgentDone:
			doneCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("EventAgentResult count = %d, want 1", resultCount)
	}
	if doneCount != 1 {
		t.Errorf("EventAgentDone count = %d, want 1", doneCount)
	}
}

// TestTranslate_MultipleTurnEndNoSettle asserts R8:
// agent_end without isTerminal:true, in any number, must NOT
// settle the turn. Only the subsequent
// isTerminal:true frame settles.
func TestTranslate_MultipleTurnEndNoSettle(t *testing.T) {
	tr := newTestTranslator()
	frames := []string{}
	frames = append(frames, textDeltas(0, "phase 1")...)
	frames = append(frames,
		`{"type":"agent_end","isTerminal":false}`,
		`{"type":"turn_end"}`,
		`{"type":"turn_start"}`,
	)
	frames = append(frames, textDeltas(1, "phase 2")...)
	frames = append(frames, `{"type":"agent_end","isTerminal":true}`)
	events := drive(t, tr, frames...)

	resultCount, doneCount := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentResult:
			resultCount++
		case agent.EventAgentDone:
			doneCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("EventAgentResult count = %d, want 1", resultCount)
	}
	if doneCount != 1 {
		t.Errorf("EventAgentDone count = %d, want 1", doneCount)
	}
}

// TestTranslate_UntouchedSettleEmitsOnlyDone asserts that an
// agent_end{isTerminal:true} fired without any preceding turn
// activity emits the EventAgentDone (so the runtime flips
// IsReady) but suppresses the (empty) EventAgentResult so the
// UI does not render a spurious empty card.
func TestTranslate_UntouchedSettleEmitsOnlyDone(t *testing.T) {
	tr := newTestTranslator()
	events := drive(t, tr, `{"type":"agent_end","isTerminal":true}`)
	if len(events) != 1 {
		t.Fatalf("events = %v, want [done]", kinds(events))
	}
	if events[0].Kind != agent.EventAgentDone {
		t.Errorf("events[0].Kind = %s, want done", events[0].Kind)
	}
}

// TestTranslate_SessionSettledIgnored asserts that the
// print-mode / RPC housekeeping events are dropped without
// double-settling the turn.
func TestTranslate_SessionSettledIgnored(t *testing.T) {
	tr := newTestTranslator()
	frames := append(textDeltas(0, "ok"),
		`{"type":"agent_end","isTerminal":true}`,
		`{"type":"prompt_result","status":"success"}`,
		`{"type":"session_settled"}`,
	)
	events := drive(t, tr, frames...)
	resultCount, doneCount := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentResult:
			resultCount++
		case agent.EventAgentDone:
			doneCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("EventAgentResult count = %d, want 1 (no double-result from prompt_result)", resultCount)
	}
	if doneCount != 1 {
		t.Errorf("EventAgentDone count = %d, want 1 (no double-done from session_settled)", doneCount)
	}
}

// TestTranslate_ExtensionUIRequestIgnored asserts R17: an
// unanswered extension_ui_request does not break the turn
// (no events emitted, pending state intact).
func TestTranslate_ExtensionUIRequestIgnored(t *testing.T) {
	tr := newTestTranslator()
	frames := []string{}
	frames = append(frames, textDeltas(0, "hello")...)
	frames = append(frames,
		`{"type":"extension_ui_request","id":"ui-1","method":"approval","params":{}}`,
	)
	frames = append(frames, textDeltas(1, "world")...)
	frames = append(frames, `{"type":"agent_end","isTerminal":true}`)
	events := drive(t, tr, frames...)

	resultCount, doneCount := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentResult:
			resultCount++
		case agent.EventAgentDone:
			doneCount++
		}
	}
	if resultCount != 1 {
		t.Errorf("EventAgentResult count = %d, want 1 (extension_ui_request did not break turn)", resultCount)
	}
	if doneCount != 1 {
		t.Errorf("EventAgentDone count = %d, want 1", doneCount)
	}
}

// TestEmitConnected_PopulatesReady asserts the boot handshake
// emits an EventAgentReady carrying model + sessionId +
// agent name + workspace + branch.
func TestEmitConnected_PopulatesReady(t *testing.T) {
	tr := newTestTranslator()
	state := &getStateResult{
		SessionID: "session-x",
		Model: &getStateModel{
			ID:            "claude-opus-4-7",
			Name:          "Opus",
			Provider:      "anthropic",
			ContextWindow: 200_000,
		},
	}
	events := tr.emitConnected(state)
	if len(events) != 1 {
		t.Fatalf("events = %v, want 1", kinds(events))
	}
	ev := events[0]
	if ev.Kind != agent.EventAgentReady {
		t.Fatalf("Kind = %s, want agent_ready", ev.Kind)
	}
	if ev.SessionID != "session-x" {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, "session-x")
	}
	if !strings.Contains(ev.Model, "Opus") || !strings.Contains(ev.Model, "claude-opus-4-7") {
		t.Errorf("Model = %q, want contains both name and id", ev.Model)
	}
	if ev.AgentName != "omp" {
		t.Errorf("AgentName = %q, want %q", ev.AgentName, "omp")
	}
	if ev.Workspace != "/tmp/ws" {
		t.Errorf("Workspace = %q, want %q", ev.Workspace, "/tmp/ws")
	}
}

// TestEmitConnected_Idempotent asserts that subsequent emits
// are no-ops once connectedSent is true (so a second
// get_state / new_session doesn't re-fire the receipt).
func TestEmitConnected_Idempotent(t *testing.T) {
	tr := newTestTranslator()
	first := tr.emitConnected(&getStateResult{SessionID: "a"})
	if len(first) != 1 {
		t.Fatalf("first = %v, want 1", kinds(first))
	}
	second := tr.emitConnected(&getStateResult{SessionID: "b"})
	if len(second) != 0 {
		t.Errorf("second = %v, want no events (connectedSent)", kinds(second))
	}
}

// TestDecodeMessageUsage_NilYieldsNil ensures the decoder
// returns nil rather than an empty struct when no usage was
// reported (avoids zero-value contamination on the Result).
func TestDecodeMessageUsage_NilYieldsNil(t *testing.T) {
	if got := decodeMessageUsage(nil, 0); got != nil {
		t.Errorf("decodeMessageUsage(nil, 0) = %+v, want nil", got)
	}
}
