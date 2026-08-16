// translate_test.go — per-turn usage wiring tests
// (F-DSH-DASHBOARD-PARITY, 2026-08-16).
//
// Locks the contract that per-turn usage flows from dsh's
// `assistant/message.usage` payload into the existing
// EventAgentResult.Usage / EventAgentDone.Usage fields — the
// receipt footer's "Line 2" already renders from these via
// gateway/outbound/translate.go → messages.OutboundMessage.Usage
// → channel/feishu/usage_footer.go.
//
// No new fields, no new types. The path is:
//
//	assistant/message{usage}  ─►  tr.lastUsage (per-turn)
//	                           ─►  turn/end: EventAgentResult.Usage
//	                           ─►  turn/end: EventAgentDone.Usage

package dsh

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestUsageToAgent_NilSafe verifies the conversion handles a nil
// pointer without panic (callers pass `data.Usage` blindly).
func TestUsageToAgent_NilSafe(t *testing.T) {
	if got := usageToAgent(nil); got != nil {
		t.Errorf("usageToAgent(nil) = %+v, want nil", got)
	}
}

// TestUsageToAgent_FieldNameGapBridged verifies that the dsh
// vocabulary (CacheCreationTokens / CacheReadTokens) is bridged
// to agent.UsageInfo's longer field names
// (CacheCreationInputTokens / CacheReadInputTokens).
//
// Per [[no-type-aliases]]: not silently aliased — the bridge
// boundary is the single point where dsh's vocabulary meets
// agent's vocabulary.
func TestUsageToAgent_FieldNameGapBridged(t *testing.T) {
	in := &usageInfo{
		InputTokens:         100,
		OutputTokens:        50,
		CacheCreationTokens: 200,
		CacheReadTokens:     800,
		CostUSD:             0.01,
	}
	got := usageToAgent(in)
	if got == nil {
		t.Fatal("usageToAgent returned nil for non-nil input")
	}
	if got.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", got.InputTokens)
	}
	if got.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", got.OutputTokens)
	}
	if got.CacheCreationInputTokens != 200 {
		t.Errorf("CacheCreationInputTokens = %d, want 200", got.CacheCreationInputTokens)
	}
	if got.CacheReadInputTokens != 800 {
		t.Errorf("CacheReadInputTokens = %d, want 800", got.CacheReadInputTokens)
	}
	if got.CostUSD != 0.01 {
		t.Errorf("CostUSD = %v, want 0.01", got.CostUSD)
	}
}

// TestHandleAssistantMessage_UsageFlowsIntoLastUsage verifies that
// the per-turn usage block from assistant/message lands in
// tr.lastUsage. Pre-fix this was dropped on the floor — every
// EventAgentResult.Usage arrived as nil at gateway/outbound.
func TestHandleAssistantMessage_UsageFlowsIntoLastUsage(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	mux := makeMuxEvent(t, "assistant/message", `{
		"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},
		"usage":{"inputTokens":100,"outputTokens":50,"cacheCreationTokens":200,"cacheReadTokens":800}
	}`)
	env, _ := decodeMuxEvent(t, mux)
	dispatcher.dispatch(env, nil)

	if tr.lastUsage == nil {
		t.Fatal("tr.lastUsage should be set after assistant/message with usage")
	}
	if tr.lastUsage.InputTokens != 100 {
		t.Errorf("lastUsage.InputTokens = %d, want 100", tr.lastUsage.InputTokens)
	}
	if tr.lastUsage.CacheReadInputTokens != 800 {
		t.Errorf("lastUsage.CacheReadInputTokens = %d, want 800", tr.lastUsage.CacheReadInputTokens)
	}

	// turn/start clears lastUsage (per-turn reset; cumulative is
	// the runtime adapter's job to roll up, NOT the bridge's).
	muxTurnStart := makeMuxEvent(t, "turn/start", `{"turn":2}`)
	envTurn, _ := decodeMuxEvent(t, muxTurnStart)
	dispatcher.dispatch(envTurn, nil)
	if tr.lastUsage != nil {
		t.Errorf("tr.lastUsage should be cleared on turn/start, got %+v", tr.lastUsage)
	}
}

// TestHandleTurnEnd_EmitsUsageOnResultAndDone
// Locks that turn/end carries lastUsage onto BOTH
// EventAgentResult.Usage AND EventAgentDone.Usage — the existing
// fields that drive the receipt footer Line 2.
func TestHandleTurnEnd_EmitsUsageOnResultAndDone(t *testing.T) {
	tr := newTranslator("test-agent", "/tmp/test")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)

	// Seed tr.lastUsage
	tr.lastUsage = &agent.UsageInfo{
		InputTokens: 42, OutputTokens: 7,
		CacheReadInputTokens: 100,
	}
	// F-52 guard: turn/end emits EventAgentResult only when the
	// turn was active (text/chunk/tool_call observed). Set it so
	// the F-52 phantom-Done guard doesn't suppress our Result.Usage
	// assertion.
	tr.active = true

	mux := makeMuxEvent(t, "turn/end", `{"stopReason":"stop"}`)
	env, _ := decodeMuxEvent(t, mux)
	dispatcher.dispatch(env, nil)

	if len(c.events) < 2 {
		t.Fatalf("expected >= 2 events from turn/end, got %d", len(c.events))
	}

	// First event: EventAgentResult with Usage populated
	if c.events[0].Kind != agent.EventAgentResult {
		t.Fatalf("events[0].Kind = %v, want EventAgentResult", c.events[0].Kind)
	}
	if c.events[0].Result == nil || c.events[0].Result.Usage == nil {
		t.Fatal("EventAgentResult.Usage should be populated from tr.lastUsage")
	}
	if c.events[0].Result.Usage.InputTokens != 42 {
		t.Errorf("Result.Usage.InputTokens = %d, want 42", c.events[0].Result.Usage.InputTokens)
	}

	// Second event: EventAgentDone with Usage populated
	if c.events[1].Kind != agent.EventAgentDone {
		t.Fatalf("events[1].Kind = %v, want EventAgentDone", c.events[1].Kind)
	}
	if c.events[1].Done == nil || c.events[1].Done.Usage == nil {
		t.Fatal("EventAgentDone.Usage should be populated from tr.lastUsage")
	}
	if c.events[1].Done.Usage.CacheReadInputTokens != 100 {
		t.Errorf("Done.Usage.CacheReadInputTokens = %d, want 100", c.events[1].Done.Usage.CacheReadInputTokens)
	}
}

func TestStopReasonToSubtype_AbortIsInterrupted(t *testing.T) {
	if got := stopReasonToSubtype("abort"); got != "interrupted" {
		t.Fatalf("abort → %q, want interrupted (dashboard stop button)", got)
	}
	if got := stopReasonToSubtype("stop"); got != "completed" {
		t.Fatalf("stop → %q, want completed", got)
	}
}

func TestHandleTurnEnd_Abort_Interrupted(t *testing.T) {
	tr := newTranslator("dsh", "/tmp")
	st := newWireState()
	c := &collectDeliver{}
	dispatcher := newDispatcher(tr, st, nil, c.deliver)
	tr.active = true

	env, _ := decodeMuxEvent(t, makeMuxEvent(t, "turn/end", `{"turn":1,"stopReason":"abort"}`))
	dispatcher.dispatch(env, nil)

	if len(c.events) < 2 {
		t.Fatalf("got %d events, want Result+Done", len(c.events))
	}
	if c.events[0].Kind != agent.EventAgentResult || c.events[0].Result == nil {
		t.Fatalf("events[0] = %+v, want Result", c.events[0])
	}
	if c.events[0].Result.Subtype != "interrupted" {
		t.Errorf("Result.Subtype = %q, want interrupted", c.events[0].Result.Subtype)
	}
	if c.events[0].Result.Text != "Stopped." {
		t.Errorf("Result.Text = %q, want Stopped.", c.events[0].Result.Text)
	}
	if c.events[1].Kind != agent.EventAgentDone || c.events[1].Done == nil {
		t.Fatalf("events[1] = %+v, want Done", c.events[1])
	}
	if c.events[1].Done.Reason != "interrupted" {
		t.Errorf("Done.Reason = %q, want interrupted", c.events[1].Done.Reason)
	}
}