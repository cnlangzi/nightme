
package codex

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── helpers ───

// captureTranslator wires a translator + a fake deliver that
// captures every emitted event. Returns the translator and the
// captured events channel.
func captureTranslator() (*translator, *capturedEvents) {
	cap := &capturedEvents{ch: make(chan agent.AgentEvent, 32)}
	deliver := func(ev agent.AgentEvent) agent.AgentEvent {
		cap.ch <- ev
		return ev
	}
	t := newTranslator(deliver, "codex", "/tmp/ws", "main", agent.NewStderrRingBuffer(agent.StderrTailBytes), nil)
	return t, cap
}

type capturedEvents struct {
	mu  sync.Mutex
	ch  chan agent.AgentEvent
	got []agent.AgentEvent
}

func (c *capturedEvents) drain(t *testing.T, n int, timeout time.Duration) []agent.AgentEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var out []agent.AgentEvent
	for len(out) < n {
		select {
		case ev := <-c.ch:
			out = append(out, ev)
		case <-time.After(time.Until(deadline)):
			return out
		}
	}
	return out
}

func (c *capturedEvents) one(t *testing.T, timeout time.Duration) agent.AgentEvent {
	t.Helper()
	select {
	case ev := <-c.ch:
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for event")
		return agent.AgentEvent{}
	}
}

// ─── tests ───

func TestTranslate_AgentMessageBuffersUntilToolOrTurnEnd(t *testing.T) {
	tr, cap := captureTranslator()

	// Start a turn so active=true.
	tr.notify("turn/started", json.RawMessage(`{}`))

	// item/started agentMessage — buffered, NOT emitted.
	tr.handleItemStarted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-1","type":"agentMessage","text":"hello "}
	}`))

	// item/completed agentMessage — still buffered.
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-1","type":"agentMessage","text":"hello "}
	}`))

	// No event should have been emitted yet.
	select {
	case ev := <-cap.ch:
		t.Fatalf("expected NO event before tool boundary, got %+v", ev)
	default:
	}

	// A tool item/started flushes the buffered text first.
	tr.handleItemStarted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-2","type":"commandExecution","command":"ls","cwd":"/tmp"}
	}`))

	text := cap.one(t, 1*time.Second)
	if text.Kind != agent.EventAgentText {
		t.Fatalf("first emitted event kind = %v, want EventAgentText", text.Kind)
	}
	if text.Text != "hello " {
		t.Errorf("flushed text = %q, want %q", text.Text, "hello ")
	}

	tool := cap.one(t, 1*time.Second)
	if tool.Kind != agent.EventAgentToolStart {
		t.Fatalf("second event kind = %v, want EventAgentToolStart", tool.Kind)
	}
	if tool.ToolStart.ID != "it-2" {
		t.Errorf("tool ID = %q, want it-2", tool.ToolStart.ID)
	}
	if tool.ToolStart.Name != "Bash" {
		t.Errorf("tool Name = %q, want Bash", tool.ToolStart.Name)
	}
	if tool.ToolStart.Args != "ls\n(in /tmp)" {
		t.Errorf("tool Args = %q, want %q", tool.ToolStart.Args, "ls\n(in /tmp)")
	}
}

func TestTranslate_TurnCompletedFlushesRemainingText(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))

	// agentMessage text with no tool boundary.
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-1","type":"agentMessage","text":"final reply"}
	}`))

	// turn/completed — should flush pending text + emit Result + Done.
	tr.notify("turn/completed", json.RawMessage(`{
		"turnId":"trn-1",
		"status":"completed",
		"usage":{"inputTokens":10,"outputTokens":5,"cachedInputTokens":3}
	}`))

	// Expected order: EventAgentText("final reply"), EventAgentResult,
	// EventAgentDone.
	ev1 := cap.one(t, 1*time.Second)
	if ev1.Kind != agent.EventAgentText || ev1.Text != "final reply" {
		t.Fatalf("first event = %+v, want EventAgentText 'final reply'", ev1)
	}
	ev2 := cap.one(t, 1*time.Second)
	if ev2.Kind != agent.EventAgentResult {
		t.Fatalf("second event kind = %v, want EventAgentResult", ev2.Kind)
	}
	if ev2.Result == nil {
		t.Fatal("Result is nil")
	}
	// Text-dedup contract: the closing flush just emitted
	// 'final reply' as EventAgentText; Result.Text is empty so
	// channels PATCH the receipt footer instead of re-shipping
	// the same content as a standalone card.
	if ev2.Result.Text != "" {
		t.Errorf("Result.Text = %q, want empty (already streamed as EventAgentText)", ev2.Result.Text)
	}
	if ev2.Result.Usage == nil {
		t.Fatal("Result.Usage is nil")
	}
	if ev2.Result.Usage.InputTokens != 10 {
		t.Errorf("Usage.InputTokens = %d, want 10", ev2.Result.Usage.InputTokens)
	}
	if ev2.Result.Usage.OutputTokens != 5 {
		t.Errorf("Usage.OutputTokens = %d, want 5", ev2.Result.Usage.OutputTokens)
	}
	if ev2.Result.Usage.CacheReadInputTokens != 3 {
		t.Errorf("Usage.CacheRead = %d, want 3", ev2.Result.Usage.CacheReadInputTokens)
	}
	ev3 := cap.one(t, 1*time.Second)
	if ev3.Kind != agent.EventAgentDone {
		t.Fatalf("third event kind = %v, want EventAgentDone", ev3.Kind)
	}
	if ev3.Done == nil || ev3.Done.Reason != "settled" {
		t.Errorf("Done.Reason = %q, want 'settled'", ev3.Done.Reason)
	}
	if ev3.Done.Usage == nil {
		t.Error("Done.Usage is nil")
	}
}

func TestTranslate_ReasoningIsDeliveredWithThinkingPrefix(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))

	// Reasoning item/started accumulates into thinkBuf.
	tr.handleItemStarted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"r-1","type":"reasoning","summary":"thinking about it"}
	}`))

	// No event yet — reasoning flushes on item/completed.
	select {
	case ev := <-cap.ch:
		t.Fatalf("expected NO event before reasoning item/completed, got %+v", ev)
	default:
	}

	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"r-1","type":"reasoning","summary":"thinking about it"}
	}`))

	ev := cap.one(t, 1*time.Second)
	if ev.Kind != agent.EventAgentText {
		t.Fatalf("kind = %v, want EventAgentText", ev.Kind)
	}
	if ev.Text != thinkingPrefix+"thinking about it" {
		t.Errorf("text = %q, want %q", ev.Text, thinkingPrefix+"thinking about it")
	}
}

func TestTranslate_ToolEndCarriesStatus(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))

	// Tool start.
	tr.handleItemStarted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"c-1","type":"commandExecution","command":"false"}
	}`))
	// Drain the text-flush + tool-start events.
	cap.drain(t, 2, 500*time.Millisecond)

	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"c-1","type":"commandExecution","exitCode":1,"status":"failed"}
	}`))

	ev := cap.one(t, 1*time.Second)
	if ev.Kind != agent.EventAgentToolEnd {
		t.Fatalf("kind = %v, want EventAgentToolEnd", ev.Kind)
	}
	if ev.ToolEnd.ID != "c-1" {
		t.Errorf("ToolEnd.ID = %q, want c-1", ev.ToolEnd.ID)
	}
	if ev.ToolEnd.Name != "Bash" {
		t.Errorf("ToolEnd.Name = %q, want Bash", ev.ToolEnd.Name)
	}
	if ev.Err == nil {
		t.Error("ev.Err is nil; want error for failed tool")
	}
}

func TestTranslate_ThreadStatusChangedIdleIsIdempotent(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-1","type":"agentMessage","text":"hi"}
	}`))

	// First: turn/completed → flush + Result + Done.
	tr.notify("turn/completed", json.RawMessage(`{
		"turnId":"trn-1","status":"completed"
	}`))
	cap.drain(t, 3, 500*time.Millisecond)

	// Second: thread/status/changed idle — must NOT re-emit.
	tr.notify("thread/status/changed", json.RawMessage(`{
		"status":{"type":"idle"}
	}`))

	select {
	case ev := <-cap.ch:
		t.Fatalf("thread/status/changed.idle emitted duplicate event: %+v", ev)
	case <-time.After(150 * time.Millisecond):
		// expected: no event
	}
}

func TestTranslate_EmptyTurnProducesNoResultOrDone(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))
	// No items.
	tr.notify("turn/completed", json.RawMessage(`{
		"turnId":"trn-1","status":"completed"
	}`))

	select {
	case ev := <-cap.ch:
		t.Fatalf("empty turn emitted event: %+v", ev)
	case <-time.After(150 * time.Millisecond):
		// expected: no event
	}
}

func TestTranslate_MultiSegmentTurnEmitsTextAtBoundariesAndEmptyResultText(t *testing.T) {
	// Multi-segment reply: text₁ → tool → text₂ → tool → text₃ (final,
	// no tool after). F-52 contract: each tool start flushes the
	// preceding text as one EventAgentText; turn-end flushes the
	// final segment as EventAgentText. Result.Text must be empty
	// so channels PATCH the receipt footer instead of opening a
	// duplicate standalone card.
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))

	// segment 1
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-1","type":"agentMessage","text":"first "}
	}`))
	// tool boundary — flushes segment 1
	tr.handleItemStarted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"t-1","type":"commandExecution","command":"ls"}
	}`))

	ev := cap.one(t, 1*time.Second)
	if ev.Kind != agent.EventAgentText || ev.Text != "first " {
		t.Fatalf("first flushed event = %+v, want EventAgentText 'first '", ev)
	}
	cap.drain(t, 1, 500*time.Millisecond) // EventAgentToolStart

	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"t-1","type":"commandExecution","exitCode":0,"status":"completed"}
	}`))
	cap.drain(t, 1, 500*time.Millisecond) // EventAgentToolEnd

	// segment 2 (final, no tool after)
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-2","type":"agentMessage","text":"final"}
	}`))

	tr.notify("turn/completed", json.RawMessage(`{
		"turnId":"trn-1","status":"completed",
		"usage":{"inputTokens":42,"outputTokens":7,"cachedInputTokens":0}
	}`))

	// Expected: EventAgentText("final"), EventAgentResult{Text:"", Usage}, EventAgentDone.
	evText := cap.one(t, 1*time.Second)
	if evText.Kind != agent.EventAgentText || evText.Text != "final" {
		t.Fatalf("turn-end flush = %+v, want EventAgentText 'final'", evText)
	}
	res := cap.one(t, 1*time.Second)
	if res.Kind != agent.EventAgentResult {
		t.Fatalf("Result kind = %v, want EventAgentResult", res.Kind)
	}
	if res.Result.Text != "" {
		t.Errorf("Result.Text = %q, want empty (segments already streamed as EventAgentText)", res.Result.Text)
	}
	if res.Result.Usage == nil || res.Result.Usage.InputTokens != 42 {
		t.Errorf("Result.Usage = %+v, want InputTokens=42 (footer must still flow)", res.Result.Usage)
	}
	done := cap.one(t, 1*time.Second)
	if done.Kind != agent.EventAgentDone {
		t.Errorf("final kind = %v, want EventAgentDone", done.Kind)
	}
}

func TestTranslate_ResultTextEmptyWithUsagePassesThrough(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))

	// A tool item followed by a turn/completed with no agentMessage.
	tr.handleItemStarted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"c-1","type":"commandExecution","command":"ls"}
	}`))
	cap.drain(t, 1, 500*time.Millisecond) // EventAgentToolStart (no text flush since pendingMsgs is empty)
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"c-1","type":"commandExecution","exitCode":0,"status":"completed"}
	}`))
	cap.drain(t, 1, 500*time.Millisecond) // EventAgentToolEnd

	tr.notify("turn/completed", json.RawMessage(`{
		"turnId":"trn-1","status":"completed",
		"usage":{"inputTokens":100,"outputTokens":50,"cachedInputTokens":0}
	}`))

	// Should emit: Result{Text: "", Usage: populated}, Done.
	// Text-dedup contract: closing flush already emitted the last
	// agentMessage segment as EventAgentText; Result carries
	// metadata only so channels PATCH the receipt footer.
	res := cap.one(t, 1*time.Second)
	if res.Kind != agent.EventAgentResult {
		t.Fatalf("kind = %v, want EventAgentResult", res.Kind)
	}
	if res.Result == nil {
		t.Fatal("Result is nil")
	}
	if res.Result.Text != "" {
		t.Errorf("Result.Text = %q, want empty (already streamed as EventAgentText)", res.Result.Text)
	}
	if res.Result.Usage == nil || res.Result.Usage.InputTokens != 100 {
		t.Errorf("Result.Usage = %+v, want InputTokens=100 (footer must still flow)", res.Result.Usage)
	}
}

func TestTranslate_ContextCompactionEmitsSentinel(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"x-1","type":"contextCompaction"}
	}`))

	ev := cap.one(t, 1*time.Second)
	if ev.Kind != agent.EventAgentText || ev.Text != "[context 已压缩]" {
		t.Fatalf("event = %+v, want EventAgentText '[context 已压缩]'", ev)
	}
}

func TestTranslate_UnknownNotificationIsDropped(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("something/weird", json.RawMessage(`{}`))

	select {
	case ev := <-cap.ch:
		t.Fatalf("unknown notification emitted event: %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// expected: no event
	}
}

func TestTranslate_UsageOverwrittenNotSummed(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))

	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-1","type":"agentMessage","text":"x"}
	}`))

	// First turn/completed with usage {10, 5}.
	tr.notify("turn/completed", json.RawMessage(`{
		"turnId":"trn-1","status":"completed",
		"usage":{"inputTokens":10,"outputTokens":5}
	}`))
	cap.drain(t, 3, 500*time.Millisecond)

	// Second turn.
	tr.notify("turn/started", json.RawMessage(`{}`))
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-2",
		"item":{"id":"it-2","type":"agentMessage","text":"y"}
	}`))

	// Second turn/completed with usage {100, 50}. Should overwrite,
	// not sum to {110, 55}.
	tr.notify("turn/completed", json.RawMessage(`{
		"turnId":"trn-2","status":"completed",
		"usage":{"inputTokens":100,"outputTokens":50}
	}`))
	cap.drain(t, 1, 500*time.Millisecond) // EventAgentText

	res := cap.one(t, 1*time.Second)
	if res.Result.Usage.InputTokens != 100 || res.Result.Usage.OutputTokens != 50 {
		t.Errorf("Usage = %+v, want overwritten {100, 50}", res.Result.Usage)
	}
}

func TestTranslate_TurnFailedSurfacesErr(t *testing.T) {
	tr, cap := captureTranslator()

	tr.notify("turn/started", json.RawMessage(`{}`))
	tr.handleItemCompleted(json.RawMessage(`{
		"threadId":"thr-1","turnId":"trn-1",
		"item":{"id":"it-1","type":"agentMessage","text":"oops"}
	}`))

	tr.notify("turn/failed", json.RawMessage(`{
		"turnId":"trn-1","status":"failed","error":"rate limited"
	}`))

	cap.drain(t, 1, 500*time.Millisecond) // EventAgentText
	res := cap.one(t, 1*time.Second)
	if res.Kind != agent.EventAgentResult {
		t.Fatalf("kind = %v, want EventAgentResult", res.Kind)
	}
	if res.Err == nil {
		t.Error("Result.Err is nil; want error")
	}
	if res.Err.Error() != "rate limited" {
		t.Errorf("Err = %q, want 'rate limited'", res.Err.Error())
	}
}

// ─── marshalling round-trip ───

func TestAppServerUsageToUsageInfo(t *testing.T) {
	got := appServerUsageToUsageInfo(&appServerUsage{
		InputTokens:       100,
		OutputTokens:      50,
		CachedInputTokens: 30,
	})
	if got == nil {
		t.Fatal("got nil")
	}
	if got.InputTokens != 100 || got.OutputTokens != 50 || got.CacheReadInputTokens != 30 {
		t.Errorf("Usage = %+v", got)
	}
	// codex doesn't report ContextWindow; should stay zero.
	if got.ContextWindow != 0 || got.ContextWindowPct != 0 {
		t.Errorf("ContextWindow fields should be zero, got %+v", got)
	}
}

func TestAppServerUsageToUsageInfo_Nil(t *testing.T) {
	if got := appServerUsageToUsageInfo(nil); got != nil {
		t.Errorf("nil in → nil out, got %+v", got)
	}
}


// ─── thread/tokenUsage/updated (codex ≥0.125 usage channel) ───

func TestTranslate_TokenUsageUpdatedStoresLastAsUsage(t *testing.T) {
	var captured []agent.AgentEvent
	deliver := func(ev agent.AgentEvent) agent.AgentEvent {
		captured = append(captured, ev)
		return ev
	}
	tr := newTranslator(deliver, "codex", "/tmp/ws", "main", nil, nil)

	// 1500 input + 0 output + 200 cached; modelContextWindow 200000.
	params := []byte(`{
		"threadId": "th-1",
		"turnId": "turn-1",
		"tokenUsage": {
			"last": {"inputTokens": 1500, "outputTokens": 0, "cachedInputTokens": 200, "reasoningOutputTokens": 0, "totalTokens": 1500},
			"total": {"inputTokens": 1500, "outputTokens": 0, "cachedInputTokens": 200, "reasoningOutputTokens": 0, "totalTokens": 1500},
			"modelContextWindow": 200000
		}
	}`)
	tr.notify("thread/tokenUsage/updated", params)

	if tr.turn.lastUsage == nil {
		t.Fatalf("lastUsage is nil after thread/tokenUsage/updated")
	}
	u := tr.turn.lastUsage
	if u.InputTokens != 1500 {
		t.Errorf("lastUsage.InputTokens = %d, want 1500", u.InputTokens)
	}
	if u.CacheReadInputTokens != 200 {
		t.Errorf("lastUsage.CacheReadInputTokens = %d, want 200", u.CacheReadInputTokens)
	}
	if u.ContextWindow != 200000 {
		t.Errorf("lastUsage.ContextWindow = %d, want 200000", u.ContextWindow)
	}
	// Formula: (InputTokens + OutputTokens + CacheCreation +
	// CacheRead) / contextWindow * 100. Here that's
	// (1500 + 0 + 0 + 200) / 200000 * 100 = 0.85.
	if u.ContextWindowPct < 0.84 || u.ContextWindowPct > 0.86 {
		t.Errorf("lastUsage.ContextWindowPct = %v, want ~0.85", u.ContextWindowPct)
	}
	// Notifier must NOT emit anything itself — usage is buffered for
	// the turn-end signal to consume.
	if len(captured) != 0 {
		t.Errorf("thread/tokenUsage/updated emitted %d events; want 0", len(captured))
	}
}

func TestTranslate_TokenUsageUpdatedFallsBackToTotalWhenLastEmpty(t *testing.T) {
	deliver := func(ev agent.AgentEvent) agent.AgentEvent { return ev }
	tr := newTranslator(deliver, "codex", "/tmp/ws", "main", nil, nil)

	params := []byte(`{
		"threadId": "th-1",
		"turnId": "turn-1",
		"tokenUsage": {
			"last": {"inputTokens": 0, "outputTokens": 0, "cachedInputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 0},
			"total": {"inputTokens": 500, "outputTokens": 80, "cachedInputTokens": 50, "reasoningOutputTokens": 0, "totalTokens": 580},
			"modelContextWindow": 100000
		}
	}`)
	tr.notify("thread/tokenUsage/updated", params)

	if tr.turn.lastUsage == nil {
		t.Fatalf("lastUsage is nil; expected fallback to total")
	}
	if tr.turn.lastUsage.InputTokens != 500 || tr.turn.lastUsage.OutputTokens != 80 {
		t.Errorf("lastUsage = %+v; want input=500 output=80 from total fallback", tr.turn.lastUsage)
	}
}

func TestTranslate_TokenUsageUpdatedIgnoresZeroes(t *testing.T) {
	deliver := func(ev agent.AgentEvent) agent.AgentEvent { return ev }
	tr := newTranslator(deliver, "codex", "/tmp/ws", "main", nil, nil)

	// Both last and total are all zeros — defensive guard against
	// malformed envelopes polluting the turn state.
	params := []byte(`{
		"threadId": "th-1",
		"turnId": "turn-1",
		"tokenUsage": {
			"last": {"inputTokens": 0, "outputTokens": 0, "cachedInputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 0},
			"total": {"inputTokens": 0, "outputTokens": 0, "cachedInputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 0},
			"modelContextWindow": 0
		}
	}`)
	tr.notify("thread/tokenUsage/updated", params)

	if tr.turn.lastUsage != nil {
		t.Errorf("lastUsage should stay nil for all-zero envelope, got %+v", tr.turn.lastUsage)
	}
}

func TestTranslate_CompleteTurnUsesTokenUsageWhenParamsNil(t *testing.T) {
	var captured []agent.AgentEvent
	deliver := func(ev agent.AgentEvent) agent.AgentEvent {
		captured = append(captured, ev)
		return ev
	}
	tr := newTranslator(deliver, "codex", "/tmp/ws", "main", nil, nil)

	// Flip the turn active by simulating an agentMessage
	// item/completed (translate.go sets active=true on the
	// completed-side for agentMessage; item/started is just a hint).
	completed := []byte(`{"threadId":"th-1","turnId":"turn-1","item":{"id":"i-1","type":"agentMessage","text":"hi"}}`)
	tr.notify("item/completed", completed)

	// Then thread/tokenUsage/updated arrives.
	usage := []byte(`{"threadId":"th-1","turnId":"turn-1","tokenUsage":{"last":{"inputTokens":900,"outputTokens":40,"cachedInputTokens":0,"reasoningOutputTokens":0,"totalTokens":940},"total":{"inputTokens":900,"outputTokens":40,"cachedInputTokens":0,"reasoningOutputTokens":0,"totalTokens":940},"modelContextWindow":128000}}`)
	tr.notify("thread/tokenUsage/updated", usage)

	// Now thread/status/changed.idle fires (params=nil → old code
	// emitted Result+Done without usage). The fix must use the
	// stored lastUsage instead.
	idle := []byte(`{"threadId":"th-1","status":{"type":"idle"}}`)
	tr.notify("thread/status/changed", idle)

	var doneEv agent.AgentEvent
	for _, ev := range captured {
		if ev.Kind == agent.EventAgentDone {
			doneEv = ev
		}
	}
	if doneEv.Done == nil {
		t.Fatalf("EventAgentDone not emitted")
	}
	if doneEv.Done.Usage == nil {
		t.Fatalf("Done.Usage is nil; expected fallback to lastUsage from thread/tokenUsage/updated")
	}
	if doneEv.Done.Usage.InputTokens != 900 || doneEv.Done.Usage.OutputTokens != 40 {
		t.Errorf("Done.Usage = %+v; want input=900 output=40", doneEv.Done.Usage)
	}
}
