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

// mustTranslate feeds one raw wire event and fails the test on a
// translation error. Returns the emitted events so callers can chain
// a whole turn without an err check per line.
func mustTranslate(t *testing.T, tr *translator, raw string) []agent.AgentEvent {
	t.Helper()
	events, err := tr.translate([]byte(raw), nil)
	if err != nil {
		t.Fatalf("translate(%s): %v", raw, err)
	}
	return events
}

// drive feeds a whole sequence of wire events and returns the
// concatenated event stream — the shape most F-52 turn-level tests
// assert on.
func drive(t *testing.T, tr *translator, raws ...string) []agent.AgentEvent {
	t.Helper()
	var out []agent.AgentEvent
	for _, raw := range raws {
		out = append(out, mustTranslate(t, tr, raw)...)
	}
	return out
}

// findResult returns the single EventResult in events, failing if
// there is not exactly one. "Exactly one EventResult per turn" is the
// core F-52 invariant, so most tests want the strict form.
func findResult(t *testing.T, events []agent.AgentEvent) *agent.ResultEvent {
	t.Helper()
	var found *agent.ResultEvent
	for _, ev := range events {
		if ev.Kind != agent.EventResult {
			continue
		}
		if found != nil {
			t.Fatalf("more than one EventResult in %v", kinds(events))
		}
		if ev.Result == nil {
			t.Fatal("EventResult with nil Result payload")
		}
		found = ev.Result
	}
	if found == nil {
		t.Fatalf("no EventResult in %v", kinds(events))
	}
	return found
}

// texts collects the payloads of every EventText in order, so tests
// can assert on the narration stream without index arithmetic.
func texts(events []agent.AgentEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind == agent.EventText {
			out = append(out, ev.Text)
		}
	}
	return out
}

// textDeltas expands a string into the token-granularity wire events
// Pi actually emits, so turn-level tests exercise the real
// accumulation path rather than a single synthetic delta.
func textDeltas(idx int, chunks ...string) []string {
	out := make([]string, 0, len(chunks)+2)
	out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":%d}}`, idx))
	for _, c := range chunks {
		out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":%d,"delta":%q}}`, idx, c))
	}
	out = append(out, fmt.Sprintf(`{"type":"message_update","assistantMessageEvent":{"type":"text_end","contentIndex":%d}}`, idx))
	return out
}

// assistantMessageEnd builds a message_end(assistant) wire event with
// the given final text and optional usage numbers.
func assistantMessageEnd(t *testing.T, text string, input, output int) string {
	t.Helper()
	msg := map[string]any{
		"role":       "assistant",
		"stopReason": "stop",
		"content":    []map[string]any{{"type": "text", "text": text}},
	}
	if input > 0 || output > 0 {
		msg["usage"] = map[string]any{
			"input": input, "output": output,
			"cacheRead": 0, "cacheWrite": 0,
			"totalTokens": input + output,
		}
	}
	return string(mustMarshal(t, map[string]any{"type": "message_end", "message": msg}))
}

// TestTranslate_AgentSettled verifies that the F-32 turn-end marker
// emits the turn's single EventResult followed by exactly one
// EventDone with Reason:"settled", and does NOT terminate the session
// (the runtime contract is that the events channel stays open across
// many turns).
//
// F-52: the result must come FIRST — the runtime's readpump flips to
// Idle and flushes the queued prompts on EventDone, so a result
// emitted afterwards would race the next turn.
//
// A minimal real turn is driven rather than a bare agent_settled,
// because an untouched turn deliberately produces no result at all —
// see TestTranslate_UntouchedSettleEmitsNoResult.
func TestTranslate_AgentSettled(t *testing.T) {
	tr := newTestTranslator()
	events := drive(t, tr, append(textDeltas(0, "ok"), `{"type":"agent_settled"}`)...)

	if len(events) != 2 {
		t.Fatalf("events = %v, want [result done]", kinds(events))
	}
	if events[0].Kind != agent.EventResult {
		t.Errorf("events[0] kind = %s, want result", events[0].Kind)
	}
	if events[1].Kind != agent.EventDone {
		t.Errorf("events[1] kind = %s, want done", events[1].Kind)
	}
	if events[1].Done == nil || events[1].Done.Reason != "settled" {
		t.Errorf("Done.Reason = %q, want settled", events0Reason(events[1]))
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

// TestTranslate_TextDelta verifies the F-52 contract: a text_delta
// emits NOTHING. Pi streams at token granularity, so emitting per
// delta shattered one sentence into ~20 OutReply bubbles (and ~20
// Feishu card PATCHes). Deltas now accumulate and surface at a
// semantic boundary — see TestTranslate_SimpleTurn_SingleResult.
func TestTranslate_TextDelta(t *testing.T) {
	tr := newTestTranslator()
	events, err := tr.translate([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hello "}}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("text_delta emitted %v, want no events (F-52 accumulates)", kinds(events))
	}
	if got := tr.turn.textBuf[0].String(); got != "hello " {
		t.Errorf("textBuf[0] = %q, want %q", got, "hello ")
	}
}

// TestTranslate_ThinkingDelta verifies reasoning accumulates across
// deltas and flushes as ONE EventText on thinking_end, carrying the
// "[思考] " prefix the receipt renderer branches on.
//
// Reasoning flushes on its own boundary rather than at the tool
// boundary because it is a distinct surface (💭 vs 💬) and must never
// land inside the reply's EventResult.
func TestTranslate_ThinkingDelta(t *testing.T) {
	tr := newTestTranslator()

	for _, chunk := range []string{"ana", "lyz", "ing"} {
		events, err := tr.translate([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"`+chunk+`"}}`), nil)
		if err != nil {
			t.Fatalf("thinking_delta %q: %v", chunk, err)
		}
		if len(events) != 0 {
			t.Fatalf("thinking_delta %q emitted %v, want no events", chunk, kinds(events))
		}
	}

	events, err := tr.translate([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_end"}}`), nil)
	if err != nil {
		t.Fatalf("thinking_end: %v", err)
	}
	if len(events) != 1 || events[0].Kind != agent.EventText {
		t.Fatalf("thinking_end produced %+v, want one EventText", events)
	}
	if events[0].Text != "[思考] analyzing" {
		t.Errorf("text = %q, want %q", events[0].Text, "[思考] analyzing")
	}
}

// TestTranslate_ThinkingDoesNotLeakIntoResult locks the separation
// between the reasoning surface and the reply surface: a turn whose
// only content is reasoning must NOT put that reasoning in the
// EventResult.
func TestTranslate_ThinkingDoesNotLeakIntoResult(t *testing.T) {
	tr := newTestTranslator()

	mustTranslate(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"secret reasoning"}}`)
	mustTranslate(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_end"}}`)

	events := mustTranslate(t, tr, `{"type":"agent_settled"}`)
	result := findResult(t, events)
	if strings.Contains(result.Text, "secret reasoning") {
		t.Errorf("EventResult.Text = %q, must not contain the reasoning text", result.Text)
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

// TestTranslate_AssistantMessageResult verifies that a message_end
// assistant role records the turn's text / usage WITHOUT emitting,
// and that agent_settled then delivers ONE EventResult whose
// ResultEvent carries Usage inline (co-located usage instead of the
// legacy EventResult + EventUsage pair).
//
// F-52 moved the emit point from message_end to agent_settled: a turn
// can contain several assistant messages (text -> toolCall ->
// toolResult -> next assistant message), so emitting per message gave
// one result per message rather than one per turn.
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
	if len(events) != 0 {
		t.Fatalf("message_end emitted %v, want no events (agent_settled delivers)", kinds(events))
	}

	settled := mustTranslate(t, tr, `{"type":"agent_settled"}`)
	if len(settled) != 2 {
		t.Fatalf("agent_settled = %v, want [result done]", kinds(settled))
	}
	result := findResult(t, settled)
	if result.Text != "hi" || result.IsError {
		t.Errorf("result = %+v", result)
	}
	if result.Subtype != "stop" {
		t.Errorf("Subtype = %q, want %q", result.Subtype, "stop")
	}
	u := result.Usage
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
// non-nil-but-all-zero Usage — the channel renders no footer
// when Usage is nil.
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
	if events, err := tr.translate(raw, nil); err != nil {
		t.Fatalf("translate: %v", err)
	} else if len(events) != 0 {
		t.Fatalf("message_end emitted %v, want no events", kinds(events))
	}

	settled := mustTranslate(t, tr, `{"type":"agent_settled"}`)
	result := findResult(t, settled)
	if result.Text != "ok" {
		t.Errorf("Text = %q, want %q", result.Text, "ok")
	}
	if result.Usage != nil {
		t.Errorf("ResultEvent.Usage = %+v, want nil (no usage section on the wire)", result.Usage)
	}
}

// TestTranslate_UsageSurvivesZeroTotalTokens locks the F-52
// "usage 100% flows through" promise for a wire-format corner case
// where Pi populates the per-field breakdown but leaves
// `totalTokens` at 0 (early releases, schema variants, synthetic
// messages). The previous asymmetric gate
// `u.Total == 0 && (cost nil/zero)` would silently drop these —
// mirroring the symmetric check in
// internal/bridge/claudecode/stream.go:decodeUsage closes the gap.
func TestTranslate_UsageSurvivesZeroTotalTokens(t *testing.T) {
	tr := newTestTranslator()
	raw := mustMarshal(t, map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content":    []map[string]any{{"type": "text", "text": "ok"}},
			"usage": map[string]any{
				"input": 10, "output": 5, "cacheRead": 0, "cacheWrite": 0,
				// totalTokens intentionally 0 — Pi wire variant.
				"totalTokens": 0,
			},
		},
	})
	if _, err := tr.translate(raw, nil); err != nil {
		t.Fatalf("translate: %v", err)
	}

	settled := mustTranslate(t, tr, `{"type":"agent_settled"}`)
	u := findResult(t, settled).Usage
	if u == nil {
		t.Fatal("Usage = nil, want non-nil (breakdown is non-zero)")
	}
	if u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Errorf("Usage = {in:%d out:%d}, want {in:10 out:5}", u.InputTokens, u.OutputTokens)
	}
}

// TestTranslate_UsageDropsAllZeroIncludingZeroCost locks the
// counter-positive: when BOTH the breakdown AND the cost are
// zero, the bridge still drops the block (defensive — there's
// nothing to surface). Confirms the symmetric gate does not
// regress TestTranslate_EmptyUsageStaysNil by emitting a
// non-nil-but-all-zero Usage that would render "$0.00" in the
// footer.
func TestTranslate_UsageDropsAllZeroIncludingZeroCost(t *testing.T) {
	tr := newTestTranslator()
	raw := mustMarshal(t, map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content":    []map[string]any{{"type": "text", "text": "ok"}},
			"usage": map[string]any{
				"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0,
				"totalTokens": 0,
				"cost":        map[string]any{"input": 0, "output": 0, "total": 0},
			},
		},
	})
	if _, err := tr.translate(raw, nil); err != nil {
		t.Fatalf("translate: %v", err)
	}

	settled := mustTranslate(t, tr, `{"type":"agent_settled"}`)
	u := findResult(t, settled).Usage
	if u != nil {
		t.Errorf("Usage = %+v, want nil (everything is zero — the symmetric gate keeps the all-zero drop)", u)
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

// TestEmitInit_Once verifies that emitConnected only fires the first
// time. Subsequent get_state responses (e.g. after a model switch
// in a future MVP) do not re-emit EventAgentConnected and corrupt the
// receipt header.
func TestEmitInit_Once(t *testing.T) {
	tr := newTestTranslator()
	state := &getStateResult{
		SessionID: "sess-1",
		Model:     &getStateModel{ID: "m1", Name: "M1"},
	}
	first := tr.emitConnected(state)
	if len(first) != 1 {
		t.Fatalf("first emitConnected = %d, want 1", len(first))
	}
	if first[0].Connected.SessionID != "sess-1" || first[0].Connected.AgentName != "pi" {
		t.Errorf("init = %+v", first[0].Connected)
	}
	if !strings.Contains(first[0].Connected.Model, "M1") {
		t.Errorf("Model = %q, want to contain M1", first[0].Connected.Model)
	}
	second := tr.emitConnected(state)
	if second != nil {
		t.Errorf("second emitConnected = %+v, want nil", second)
	}
}

// TestEmitInit_NoModel verifies that init still fires with empty
// model + session id when the get_state response was empty (Pi
// in early-startup state).
func TestEmitInit_NoModel(t *testing.T) {
	tr := newTestTranslator()
	first := tr.emitConnected(nil)
	if len(first) != 1 {
		t.Fatalf("emitConnected(nil) = %d, want 1", len(first))
	}
	if first[0].Connected.Model != "" || first[0].Connected.SessionID != "" {
		t.Errorf("empty init = %+v", first[0].Connected)
	}
	if first[0].Connected.AgentName != "pi" || first[0].Connected.Workspace != "/tmp/ws" {
		t.Errorf("agent/workspace = %+v", first[0].Connected)
	}
}

// TestEmitInit_CapturesContextWindow pins F-54: the bridge
// caches `data.model.contextWindow` from get_state on the
// translator so subsequent decodeMessageUsage calls can compute
// the per-turn X% without re-querying. The value itself never
// crosses the UsageEvent struct boundary (verified by the
// decodeMessageUsage tests below — they receive ctxWindow as a
// parameter, not from UsageEvent).
func TestEmitInit_CapturesContextWindow(t *testing.T) {
	tr := newTestTranslator()
	if got := tr.contextWindow.Load(); got != 0 {
		t.Fatalf("pre-condition: translator.contextWindow = %d, want 0", got)
	}
	state := &getStateResult{
		SessionID: "sess-1",
		Model: &getStateModel{
			ID:            "claude-sonnet-4-5",
			Name:          "Sonnet 4.5",
			ContextWindow: 200000,
		},
	}
	if events := tr.emitConnected(state); len(events) != 1 {
		t.Fatalf("emitConnected = %d events, want 1", len(events))
	}
	if got := tr.contextWindow.Load(); got != 200000 {
		t.Errorf("translator.contextWindow = %d, want 200000", got)
	}
}

// TestEmitInit_NoContextWindow verifies the zero-window fallback
// path: pi omitting `data.model.contextWindow` (older versions,
// or new models the catalog hasn't registered yet) leaves the
// translator's window at 0 — downstream decodeMessageUsage then
// produces pct=0 and the footer omits X% per F-45 §1.6.
func TestEmitInit_NoContextWindow(t *testing.T) {
	tr := newTestTranslator()
	state := &getStateResult{
		SessionID: "sess-1",
		Model:     &getStateModel{ID: "m1", Name: "M1"}, // no ContextWindow
	}
	tr.emitConnected(state)
	if got := tr.contextWindow.Load(); got != 0 {
		t.Errorf("translator.contextWindow = %d, want 0 (model had no ContextWindow)",
			got)
	}
}

// TestDecodeMessageUsage_ContextWindowPct pins F-54 §2.2: the
// bridge-computed pct follows Doc 1 (used / window * 100) when
// the translator-supplied contextWindow is positive. The
// translator's window is held outside UsageEvent — decodeMessageUsage
// receives it as a parameter — so this test also implicitly
// asserts that pct is the only context-window-derived field on
// the returned UsageEvent (ContextWindow was deleted in F-54).
func TestDecodeMessageUsage_ContextWindowPct(t *testing.T) {
	cases := []struct {
		name      string
		usage     *messageUsage
		ctxWindow int
		wantPct   float64
	}{
		{
			name: "typical — 21100 of 200k → 10.55%",
			usage: &messageUsage{
				Input: 100, Output: 1000,
				CacheRead: 0, CacheWrite: 20000,
				Cost: &usageCost{Total: 0.01},
			},
			ctxWindow: 200000,
			wantPct:   10.55,
		},
		{
			name: "near ceiling — 199200 / 200000 → 99.6%",
			usage: &messageUsage{
				Input: 200, Output: 1000,
				CacheRead: 0, CacheWrite: 198000,
				Cost: &usageCost{Total: 0.5},
			},
			ctxWindow: 200000,
			wantPct:   99.6,
		},
		{
			name: "at ceiling — 200000 / 200000 → 100.0%",
			usage: &messageUsage{
				Input: 200000, Output: 0,
				CacheRead: 0, CacheWrite: 0,
				Cost: &usageCost{Total: 0.5},
			},
			ctxWindow: 200000,
			wantPct:   100.0,
		},
		{
			name: "1M context — 500k of 1M → 50.0%",
			usage: &messageUsage{
				Input: 500000, Output: 0,
				CacheRead: 0, CacheWrite: 0,
				Cost: &usageCost{Total: 1.0},
			},
			ctxWindow: 1000000,
			wantPct:   50.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := decodeMessageUsage(tc.usage, tc.ctxWindow)
			if ev == nil {
				t.Fatalf("decodeMessageUsage returned nil; want non-nil")
			}
			diff := ev.ContextWindowPct - tc.wantPct
			if diff < -0.05 || diff > 0.05 {
				t.Errorf("ContextWindowPct = %.4f, want %.4f (±0.05)",
					ev.ContextWindowPct, tc.wantPct)
			}
		})
	}
}

// TestDecodeMessageUsage_NoContextWindow pins F-54 fallback:
// ctxWindow=0 (pi not yet reported / older version / model not
// in catalog) → ContextWindowPct stays 0 → footer omits X%.
func TestDecodeMessageUsage_NoContextWindow(t *testing.T) {
	cases := []struct {
		name      string
		usage     *messageUsage
		ctxWindow int
	}{
		{
			name: "ctxWindow=0 — get_state not yet arrived",
			usage: &messageUsage{
				Input: 1000, Output: 500,
				CacheRead: 0, CacheWrite: 0,
				Cost: &usageCost{Total: 0.01},
			},
			ctxWindow: 0,
		},
		{
			name: "negative ctxWindow treated as 0",
			usage: &messageUsage{
				Input: 1000, Output: 500,
				CacheRead: 0, CacheWrite: 0,
				Cost: &usageCost{Total: 0.01},
			},
			ctxWindow: -1,
		},
		{
			name: "zero usage — pct stays 0 regardless of window",
			usage: &messageUsage{
				Input: 0, Output: 0,
				CacheRead: 0, CacheWrite: 0,
				Cost: &usageCost{Total: 0},
			},
			ctxWindow: 200000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := decodeMessageUsage(tc.usage, tc.ctxWindow)
			if ev != nil && ev.ContextWindowPct != 0 {
				t.Errorf("ContextWindowPct = %v, want 0", ev.ContextWindowPct)
			}
		})
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
	if _, ok := tr.turn.pendingTools["c-1"]; ok {
		t.Errorf("pendingTools[c-1] = %+v, want removed", tr.turn.pendingTools["c-1"])
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
	if len(tr.turn.pendingTools) != 1 {
		t.Fatalf("pendingTools len = %d, want 1", len(tr.turn.pendingTools))
	}

	if _, err := tr.translate([]byte(`{"type":"agent_settled"}`), nil); err != nil {
		t.Fatalf("settled translate: %v", err)
	}
	if len(tr.turn.pendingTools) != 0 {
		t.Errorf("pendingTools len after settled = %d, want 0", len(tr.turn.pendingTools))
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
	if _, ok := tr.turn.pendingTools[""]; ok {
		t.Errorf("pendingTools[\"\"] must not be set on empty-id start; got %+v", tr.turn.pendingTools[""])
	}

	// Second empty-id start with a different toolName would have
	// overwritten a stale entry under "" — verify it didn't.
	start2 := []byte(`{"type":"tool_execution_start","toolCallId":"","toolName":"read","args":{"path":"/etc"}}`)
	if _, err := tr.translate(start2, nil); err != nil {
		t.Fatalf("start2 translate: %v", err)
	}
	if _, ok := tr.turn.pendingTools[""]; ok {
		t.Errorf("pendingTools[\"\"] must remain unset on second empty-id start; got %+v", tr.turn.pendingTools[""])
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
	if _, ok := tr.turn.pendingTools["c-real"]; !ok {
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

	// Reset goroutine periodically clears the turn state (simulating
	// /new). It goes through the same beginReset/endReset pair
	// session.New() uses, so the test exercises the real locking and
	// the suppression window together.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			tr.beginReset()
			runtime.Gosched()
			tr.endReset()
			runtime.Gosched()
		}
	}()

	wg.Wait()
	// After the storm, the map should be empty (every end drained,
	// last reset cleared any leftovers).
	if len(tr.turn.pendingTools) != 0 {
		t.Errorf("after concurrent storm + reset, pendingTools len = %d, want 0", len(tr.turn.pendingTools))
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

// TestTranslate_StateUpdate_EmitsEventAgentConnected verifies F-34 §3.2.2:
// when pi emits state_update after a new_session RPC, the translator
// surfaces an EventAgentConnected carrying the new sessionId. The runtime's
// eventHandler (cmd/nightme/run.go newEventHandler) picks it up
// via SetResumeID.
func TestTranslate_StateUpdate_EmitsEventAgentConnected(t *testing.T) {
	tr := newTestTranslator()
	raw := []byte(`{"type":"state_update","sessionId":"new-sess-1","modelId":"m1","modelName":"M1"}`)
	events, err := tr.translate(raw, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Kind != agent.EventAgentConnected {
		t.Fatalf("Kind = %s, want EventAgentConnected", events[0].Kind)
	}
	if events[0].Connected.SessionID != "new-sess-1" {
		t.Errorf("SessionID = %q, want new-sess-1", events[0].Connected.SessionID)
	}
	if !strings.Contains(events[0].Connected.Model, "M1") {
		t.Errorf("Model = %q, want to contain M1", events[0].Connected.Model)
	}
	if events[0].Connected.AgentName != "pi" {
		t.Errorf("AgentName = %q, want pi", events[0].Connected.AgentName)
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

// ---------------------------------------------------------------
// F-52: stream aggregation — turn-level contracts.
//
// These drive whole wire sequences rather than single events,
// because the F-52 invariants ("one EventResult per turn", "no
// duplicated text") are only observable across a turn.
// See docs/feat/F-52-pi-stream-aggregation.md.
// ---------------------------------------------------------------

// TestTranslate_SimpleTurn_SingleResult is the regression lock for
// the bug that motivated F-52: a plain reply with no tool calls must
// produce exactly ONE user-visible event — the result — no matter how
// many tokens Pi streamed.
//
// Before F-52 this sequence produced 5 EventText (one per delta) and
// no EventResult at all, which the Feishu adapter rendered as five
// separate 💬 bubbles.
func TestTranslate_SimpleTurn_SingleResult(t *testing.T) {
	tr := newTestTranslator()

	seq := textDeltas(0, "Hello", "! How can ", "I help ", "you ", "today?")
	seq = append(seq,
		assistantMessageEnd(t, "Hello! How can I help you today?", 120, 18),
		`{"type":"agent_settled"}`,
	)
	events := drive(t, tr, seq...)

	if got := kinds(events); len(got) != 2 {
		t.Fatalf("kinds = %v, want exactly [result done]", got)
	}
	if txt := texts(events); len(txt) != 0 {
		t.Errorf("EventText emitted %q, want none for a tool-free turn", txt)
	}

	result := findResult(t, events)
	if want := "Hello! How can I help you today?"; result.Text != want {
		t.Errorf("Text = %q, want %q", result.Text, want)
	}
	if result.Usage == nil || result.Usage.InputTokens != 120 {
		t.Errorf("Usage = %+v, want InputTokens 120", result.Usage)
	}
}

// TestTranslate_ToolTurn_NoDuplicateText is the zero-duplication
// lock. Narration that precedes a tool call is flushed as EventText
// at the tool boundary; the turn's EventResult must then carry ONLY
// the segment written after the tool returned.
//
// If flushPendingTextLocked ever stops clearing pendingText, the
// user would see segment A twice — once as 💬, once inside the final
// 📝 card. That is exactly what this asserts against.
func TestTranslate_ToolTurn_NoDuplicateText(t *testing.T) {
	tr := newTestTranslator()

	var seq []string
	seq = append(seq, textDeltas(0, "Let me ", "check the ", "logs.")...)
	seq = append(seq,
		`{"type":"tool_execution_start","toolCallId":"c-1","toolName":"bash","args":{"command":"tail log"}}`,
		`{"type":"tool_execution_end","toolCallId":"c-1","toolName":"bash","result":"ok","isError":false}`,
	)
	seq = append(seq, textDeltas(1, "Found ", "the problem.")...)
	seq = append(seq,
		assistantMessageEnd(t, "Found the problem.", 300, 12),
		`{"type":"agent_settled"}`,
	)
	events := drive(t, tr, seq...)

	narration := texts(events)
	if len(narration) != 1 || narration[0] != "Let me check the logs." {
		t.Fatalf("narration = %q, want exactly [\"Let me check the logs.\"]", narration)
	}

	result := findResult(t, events)
	if result.Text != "Found the problem." {
		t.Errorf("Text = %q, want %q", result.Text, "Found the problem.")
	}
	if strings.Contains(result.Text, "Let me check") {
		t.Errorf("Text = %q duplicates narration already delivered as EventText", result.Text)
	}

	// Ordering matters for the receipt: narration, then the tool
	// pair, then the result, then done.
	want := []agent.EventKind{
		agent.EventText, agent.EventToolStart, agent.EventToolEnd,
		agent.EventResult, agent.EventDone,
	}
	got := kinds(events)
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}

// TestTranslate_UsageIsLatestSnapshotNotSum locks the context-window
// semantics. Each Pi API call reports an input side that already
// contains the whole conversation, so usage is a snapshot of current
// context occupancy — summing a multi-call turn would overstate it by
// roughly the call count and make the footer read "nearly full" on a
// mostly-empty context.
func TestTranslate_UsageIsLatestSnapshotNotSum(t *testing.T) {
	tr := newTestTranslator()

	events := drive(t, tr,
		assistantMessageEnd(t, "first", 100, 10),
		`{"type":"tool_execution_start","toolCallId":"c-1","toolName":"bash","args":{}}`,
		`{"type":"tool_execution_end","toolCallId":"c-1","toolName":"bash","result":"ok","isError":false}`,
		assistantMessageEnd(t, "second", 250, 20),
		`{"type":"agent_settled"}`,
	)

	u := findResult(t, events).Usage
	if u == nil {
		t.Fatal("Usage = nil, want the last message's snapshot")
	}
	if u.InputTokens != 250 || u.OutputTokens != 20 {
		t.Errorf("Usage = {in:%d out:%d}, want {in:250 out:20} (latest snapshot, NOT 350/30)",
			u.InputTokens, u.OutputTokens)
	}
}

// TestTranslate_EmptyTurnStillCarriesUsage covers the turn that ends
// on a tool call with no closing narration.
//
// The fallback text is load-bearing, not cosmetic: gateway.Translate
// drops an EventResult whose Text is empty and IsError is false, and
// the runtime reads Usage off the translated OutboundMessage — so an
// empty Text would silently take the turn's token counts with it.
func TestTranslate_EmptyTurnStillCarriesUsage(t *testing.T) {
	tr := newTestTranslator()

	raw := mustMarshal(t, map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content":    []map[string]any{{"type": "toolCall", "id": "c-1", "name": "bash"}},
			"usage": map[string]any{
				"input": 77, "output": 3, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 80,
			},
		},
	})
	events := drive(t, tr, string(raw), `{"type":"agent_settled"}`)

	result := findResult(t, events)
	if result.Text == "" {
		t.Fatal("Text is empty; gateway.Translate would drop this result and lose the usage")
	}
	if result.Text != emptyReplyFallback {
		t.Errorf("Text = %q, want the fallback %q", result.Text, emptyReplyFallback)
	}
	if result.Usage == nil || result.Usage.InputTokens != 77 {
		t.Errorf("Usage = %+v, want InputTokens 77", result.Usage)
	}
}

// TestTranslate_MissingTextEndStillFlushes covers the abort / error
// path, where Pi can settle a turn with deltas still buffered and no
// closing text_end. The tail of the reply must not be silently lost.
func TestTranslate_MissingTextEndStillFlushes(t *testing.T) {
	tr := newTestTranslator()

	events := drive(t, tr,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"partial answer"}}`,
		// no text_end, no message_end — the agent was aborted.
		`{"type":"agent_settled"}`,
	)

	result := findResult(t, events)
	if result.Text != "partial answer" {
		t.Errorf("Text = %q, want the buffered tail %q", result.Text, "partial answer")
	}
}

// TestTranslate_InterleavedContentIndexes verifies the per-index
// buffering: two content blocks streaming concurrently must not
// interleave their characters, and must join in ascending index
// order regardless of Go's map iteration order.
func TestTranslate_InterleavedContentIndexes(t *testing.T) {
	tr := newTestTranslator()

	events := drive(t, tr,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":1}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"AAA"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":1,"delta":"BBB"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"aaa"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":1,"delta":"bbb"}}`,
		`{"type":"agent_settled"}`,
	)

	result := findResult(t, events)
	if want := "AAAaaa\nBBBbbb"; result.Text != want {
		t.Errorf("Text = %q, want %q (blocks kept separate, joined in index order)", result.Text, want)
	}
}

// TestTranslate_TextStartDropsStalePartial verifies the defensive
// reset: a text_start on an index that still holds a partial (a
// missed text_end) must discard it rather than let the previous
// block's tail bleed into the new one.
func TestTranslate_TextStartDropsStalePartial(t *testing.T) {
	tr := newTestTranslator()

	events := drive(t, tr,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"stale"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"fresh"}}`,
		`{"type":"agent_settled"}`,
	)

	result := findResult(t, events)
	if result.Text != "fresh" {
		t.Errorf("Text = %q, want %q (stale partial must be dropped)", result.Text, "fresh")
	}
}

// TestTranslate_TurnStateResetsBetweenTurns verifies that
// agent_settled leaves nothing behind. A leaked buffer would replay
// the previous turn's text (or usage) into the next reply.
func TestTranslate_TurnStateResetsBetweenTurns(t *testing.T) {
	tr := newTestTranslator()

	first := drive(t, tr, append(textDeltas(0, "turn one"),
		assistantMessageEnd(t, "turn one", 100, 5),
		`{"type":"agent_settled"}`)...)
	if got := findResult(t, first).Text; got != "turn one" {
		t.Fatalf("turn 1 Text = %q, want %q", got, "turn one")
	}

	// State must be pristine immediately after settle.
	if len(tr.turn.textBuf) != 0 || tr.turn.pendingText != "" ||
		tr.turn.lastMessageText != "" || tr.turn.lastUsage != nil ||
		len(tr.turn.pendingTools) != 0 {
		t.Fatalf("turn state not reset after agent_settled: %+v", tr.turn)
	}

	second := drive(t, tr, append(textDeltas(0, "turn two"),
		assistantMessageEnd(t, "turn two", 200, 7),
		`{"type":"agent_settled"}`)...)
	result := findResult(t, second)
	if result.Text != "turn two" {
		t.Errorf("turn 2 Text = %q, want %q (turn 1 leaked)", result.Text, "turn two")
	}
	if result.Usage == nil || result.Usage.InputTokens != 200 {
		t.Errorf("turn 2 Usage = %+v, want InputTokens 200", result.Usage)
	}
}

// TestTranslate_ResetTurnClearsMidTurnState covers /new landing in
// the middle of a streaming reply: session.New() calls resetTurn(),
// after which the half-streamed text must not surface at all — the
// reset turn is untouched, so settling it yields only EventDone — nor
// leak into the next real turn.
func TestTranslate_ResetTurnClearsMidTurnState(t *testing.T) {
	tr := newTestTranslator()

	drive(t, tr,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":0}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"abandoned"}}`,
	)
	tr.beginReset()
	tr.endReset()

	// The abandoned turn is now untouched: no result, just the marker.
	settled := drive(t, tr, `{"type":"agent_settled"}`)
	if len(settled) != 1 || settled[0].Kind != agent.EventDone {
		t.Fatalf("kinds = %v, want [done] only after /new discarded the turn", kinds(settled))
	}

	// And the next real turn must be clean.
	next := drive(t, tr, append(textDeltas(0, "fresh start"),
		`{"type":"agent_settled"}`)...)
	result := findResult(t, next)
	if strings.Contains(result.Text, "abandoned") {
		t.Errorf("Text = %q, must not contain text abandoned by /new", result.Text)
	}
	if result.Text != "fresh start" {
		t.Errorf("Text = %q, want %q", result.Text, "fresh start")
	}
}

// TestTranslate_StopReasonError surfaces as IsError so the channel
// can flip its header even when the turn produced no text.
func TestTranslate_StopReasonError(t *testing.T) {
	tr := newTestTranslator()

	raw := mustMarshal(t, map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "error",
			"content":    []map[string]any{},
		},
	})
	events := drive(t, tr, string(raw), `{"type":"agent_settled"}`)

	result := findResult(t, events)
	if !result.IsError {
		t.Errorf("IsError = false, want true for stopReason=error")
	}
	if result.Subtype != "error" {
		t.Errorf("Subtype = %q, want %q", result.Subtype, "error")
	}
}

// TestTranslate_CompactionPreservesTurnBuffers verifies that a
// mid-turn compaction cycle does not discard the reply being
// composed. Compaction is orthogonal to the turn boundary.
func TestTranslate_CompactionPreservesTurnBuffers(t *testing.T) {
	tr := newTestTranslator()

	events := drive(t, tr, append(textDeltas(0, "before compaction"),
		`{"type":"compaction_start","reason":"threshold"}`,
		`{"type":"compaction_end","reason":"threshold","result":{"aborted":false}}`,
		`{"type":"agent_settled"}`)...)

	result := findResult(t, events)
	if result.Text != "before compaction" {
		t.Errorf("Text = %q, want %q (compaction must not clear turn buffers)",
			result.Text, "before compaction")
	}
}

// TestTranslate_ToolEndingTurn_NoDuplicate is the regression lock for
// a bug found reviewing F-52's first cut.
//
// Pi emits message_end(assistant) with content[] = [text, toolCall]
// BEFORE the matching tool_execution_start. So a turn that ends on a
// tool call leaves lastMessageText holding the very paragraph that the
// tool boundary then flushed as EventText. The original fallback chain
// ("pendingText, else lastMessageText") re-delivered it as the 📝
// result card — the exact duplication F-52 set out to remove, just in
// a shape the first round of tests did not cover.
//
// turnState.textDelivered is what disarms the fallback.
func TestTranslate_ToolEndingTurn_NoDuplicate(t *testing.T) {
	tr := newTestTranslator()

	var seq []string
	seq = append(seq, textDeltas(0, "Let me ", "check.")...)
	seq = append(seq, string(mustMarshal(t, map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role": "assistant", "stopReason": "toolUse",
			"content": []map[string]any{
				{"type": "text", "text": "Let me check."},
				{"type": "toolCall", "id": "c-1", "name": "bash"},
			},
			"usage": map[string]any{"input": 90, "output": 4, "totalTokens": 94},
		},
	})))
	seq = append(seq,
		`{"type":"tool_execution_start","toolCallId":"c-1","toolName":"bash","args":{}}`,
		`{"type":"tool_execution_end","toolCallId":"c-1","toolName":"bash","result":"ok","isError":false}`,
		`{"type":"agent_settled"}`,
	)
	events := drive(t, tr, seq...)

	narration := texts(events)
	if len(narration) != 1 || narration[0] != "Let me check." {
		t.Fatalf("narration = %q, want exactly [\"Let me check.\"]", narration)
	}
	result := findResult(t, events)
	for _, delivered := range narration {
		if result.Text == delivered {
			t.Fatalf("EventResult.Text = %q duplicates an EventText already delivered", result.Text)
		}
	}
	if result.Text != emptyReplyFallback {
		t.Errorf("Text = %q, want the fallback %q", result.Text, emptyReplyFallback)
	}
	// Usage must still ride out even though the text is a placeholder —
	// that is the whole reason the placeholder exists.
	if result.Usage == nil || result.Usage.InputTokens != 90 {
		t.Errorf("Usage = %+v, want InputTokens 90", result.Usage)
	}
}

// TestTranslate_NonStreamedTurnUsesMessageFallback is the counterpart:
// textDelivered must NOT be armed by a no-op flush, so a turn where Pi
// never streamed still falls back to message_end's content[].
func TestTranslate_NonStreamedTurnUsesMessageFallback(t *testing.T) {
	tr := newTestTranslator()

	events := drive(t, tr,
		// A tool runs first, flushing nothing (no deltas were buffered).
		`{"type":"tool_execution_start","toolCallId":"c-1","toolName":"bash","args":{}}`,
		`{"type":"tool_execution_end","toolCallId":"c-1","toolName":"bash","result":"ok","isError":false}`,
		// Then a replayed (non-streamed) assistant message.
		assistantMessageEnd(t, "replayed answer", 50, 6),
		`{"type":"agent_settled"}`,
	)

	if txt := texts(events); len(txt) != 0 {
		t.Errorf("EventText = %q, want none (nothing was streamed)", txt)
	}
	if got := findResult(t, events).Text; got != "replayed answer" {
		t.Errorf("Text = %q, want %q (fallback must survive a no-op flush)", got, "replayed answer")
	}
}

// TestTranslate_UntouchedSettleEmitsNoResult covers agent_settled
// firing without an accompanying run. Pi settles out-of-band paths
// (e.g. a fire-and-forget compaction) the same way it settles a real
// turn; emitting a result there would drop a spurious "Done." card
// into the user's chat.
func TestTranslate_UntouchedSettleEmitsNoResult(t *testing.T) {
	tr := newTestTranslator()

	events := mustTranslate(t, tr, `{"type":"agent_settled"}`)
	if len(events) != 1 || events[0].Kind != agent.EventDone {
		t.Fatalf("kinds = %v, want [done] only", kinds(events))
	}
}

// TestTranslate_ResetWindowDropsAbandonedTurn is the regression lock
// for the /new race window.
//
// session.New() cannot deliver the new EventAgentConnected until a get_state
// round-trip completes (10s deadline), and readPump keeps translating
// the whole time. /new is reachable mid-turn — nothing gates it on the
// FSM being Idle and slash commands bypass the InputBuffer — so wire
// events from the turn the user just abandoned arrive *after* the
// reset. Clearing the turn state alone does not stop them.
//
// Two concrete harms this locks out:
//   - an old message_end stamping its usage onto the new session
//     (corrupts the context-occupancy figure on the bridge's
//     per-turn snapshot);
//   - an old agent_settled shipping the abandoned reply as the new
//     session's result card.
func TestTranslate_ResetWindowDropsAbandonedTurn(t *testing.T) {
	tr := newTestTranslator()

	// A turn is streaming when the user types /new.
	drive(t, tr, textDeltas(0, "abandoned reply")...)

	tr.beginReset() // session.New() has just been acknowledged by pi.

	// Everything still in the pipe from the old turn must vanish.
	stale := drive(t, tr, append(
		textDeltas(1, "more abandoned text"),
		assistantMessageEnd(t, "abandoned reply", 9999, 777),
		`{"type":"tool_execution_start","toolCallId":"c-old","toolName":"bash","args":{}}`,
		`{"type":"tool_execution_end","toolCallId":"c-old","toolName":"bash","result":"ok","isError":false}`,
		`{"type":"agent_settled"}`,
	)...)
	if len(stale) != 0 {
		t.Fatalf("events during the reset window = %v, want none", kinds(stale))
	}

	// The fresh turn state must be pristine — in particular no usage
	// from the abandoned turn.
	if tr.turn.lastUsage != nil {
		t.Errorf("lastUsage = %+v, want nil (abandoned turn's usage must not carry over)", tr.turn.lastUsage)
	}
	if tr.turn.active || tr.turn.textDelivered {
		t.Errorf("active=%v textDelivered=%v, want both false", tr.turn.active, tr.turn.textDelivered)
	}
	if len(tr.turn.textBuf) != 0 || tr.turn.pendingText != "" || tr.turn.lastMessageText != "" {
		t.Errorf("turn buffers not pristine: %+v", tr.turn)
	}
	if len(tr.turn.pendingTools) != 0 {
		t.Errorf("pendingTools = %v, want empty", tr.turn.pendingTools)
	}

	tr.endReset() // EventAgentConnected delivered; normal translation resumes.

	next := drive(t, tr, append(textDeltas(0, "fresh answer"),
		assistantMessageEnd(t, "fresh answer", 42, 7),
		`{"type":"agent_settled"}`)...)
	result := findResult(t, next)
	if result.Text != "fresh answer" {
		t.Errorf("Text = %q, want %q", result.Text, "fresh answer")
	}
	if result.Usage == nil || result.Usage.InputTokens != 42 {
		t.Errorf("Usage = %+v, want InputTokens 42 (not the abandoned turn's 9999)", result.Usage)
	}
}

// TestTranslate_EndResetRestoresTranslation guards the mute hazard:
// a leaked suppression flag would silence the bridge forever, so
// session.New() defers endReset on every exit path. This locks the
// flag's own behaviour.
func TestTranslate_EndResetRestoresTranslation(t *testing.T) {
	tr := newTestTranslator()

	tr.beginReset()
	if events := drive(t, tr, textDeltas(0, "dropped")...); len(events) != 0 {
		t.Fatalf("events while suppressing = %v, want none", kinds(events))
	}
	tr.endReset()

	events := drive(t, tr, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"back"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"thinking_end"}}`)
	if len(events) != 1 || events[0].Kind != agent.EventText {
		t.Fatalf("after endReset events = %v, want one EventText", kinds(events))
	}
	if events[0].Text != thinkingPrefix+"back" {
		t.Errorf("Text = %q, want %q", events[0].Text, thinkingPrefix+"back")
	}
}
