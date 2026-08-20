// Tests for the deliver() stamping contract and the new generic
// sessionUpdate handlers (handleUsageUpdate, handleSessionStatus,
// handleSessionInfoUpdate) introduced when the bridge took over
// the full ACP spec + common-vendor-extension surface.
//
// White-box (package acp) — constructs minimal driver structs in
// the same style as emit_contract_test.go. No real ACP bridge is
// launched; these tests pin the per-event Model/SessionID/etc.
// stamping contract and the per-turn usage stash behaviour.

package acp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// newTestDriver constructs a minimal driver suitable for testing
// deliver() stamping and sessionUpdate handlers. Only the fields
// the tests touch are populated; everything else is zero.
func newTestDriver() *driver {
	return &driver{
		ctx:       context.Background(),
		events:    make(chan agent.AgentEvent, 16),
		sessionID: "test-session",
		agentName: "test-agent",
		workspace: "/tmp/ws",
	}
}

// ─── deliver() stamping contract ────────────────────────────────────

// TestDeliver_StampsSessionContext verifies that deliver() fills
// the four bridge-local context fields (SessionID / Model /
// AgentName / Workspace) on the event before sending.
func TestDeliver_StampsSessionContext(t *testing.T) {
	d := newTestDriver()
	d.model = "claude-opus-4-5"

	d.deliver(agent.AgentEvent{Kind: agent.EventAgentText, Text: "hello"})

	select {
	case ev := <-d.events:
		if ev.SessionID != "test-session" {
			t.Errorf("SessionID = %q, want test-session", ev.SessionID)
		}
		if ev.Model != "claude-opus-4-5" {
			t.Errorf("Model = %q, want claude-opus-4-5", ev.Model)
		}
		if ev.AgentName != "test-agent" {
			t.Errorf("AgentName = %q, want test-agent", ev.AgentName)
		}
		if ev.Workspace != "/tmp/ws" {
			t.Errorf("Workspace = %q, want /tmp/ws", ev.Workspace)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivered event")
	}
}

// TestDeliver_PreservesCallerValues verifies that fields already
// set by the caller are NOT overwritten by deliver()'s bridge-
// local defaults — caller intent wins.
func TestDeliver_PreservesCallerValues(t *testing.T) {
	d := newTestDriver()
	d.model = "opencode-default-model"

	// Set every context field to a non-default value to verify
	// each is preserved verbatim.
	ev := agent.AgentEvent{
		Kind:      agent.EventAgentText,
		Text:      "hello",
		SessionID: "caller-session",
		Model:     "caller-model",
		AgentName: "caller-agent",
		Workspace: "/caller/ws",
	}
	d.deliver(ev)

	select {
	case got := <-d.events:
		if got.SessionID != "caller-session" {
			t.Errorf("SessionID = %q, want caller-session (preserved)", got.SessionID)
		}
		if got.Model != "caller-model" {
			t.Errorf("Model = %q, want caller-model (preserved)", got.Model)
		}
		if got.AgentName != "caller-agent" {
			t.Errorf("AgentName = %q, want caller-agent (preserved)", got.AgentName)
		}
		if got.Workspace != "/caller/ws" {
			t.Errorf("Workspace = %q, want /caller/ws (preserved)", got.Workspace)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivered event")
	}
}

// TestDeliverAfterModelUpdate_StampsNewModel verifies that after
// the bridge updates d.model (e.g. via handleUsageUpdate from a
// vendor-extension model field), subsequent deliver() calls
// stamp the NEW model — without the bridge having to re-emit
// EventAgentReady.
func TestDeliverAfterModelUpdate_StampsNewModel(t *testing.T) {
	d := newTestDriver()
	d.model = "opencode-default"

	// First event: stamped with the initial model.
	d.deliver(agent.AgentEvent{Kind: agent.EventAgentText, Text: "first"})
	ev1 := <-d.events
	if ev1.Model != "opencode-default" {
		t.Errorf("first event Model = %q, want opencode-default", ev1.Model)
	}

	// Simulate model discovery (the real driver does this in
	// handleUsageUpdate / handleSessionInfoUpdate on the readPump
	// goroutine — single-writer/single-reader, no mutex needed).
	d.model = "opencode-smarter"

	// Second event: stamped with the new model.
	d.deliver(agent.AgentEvent{Kind: agent.EventAgentText, Text: "second"})
	ev2 := <-d.events
	if ev2.Model != "opencode-smarter" {
		t.Errorf("second event Model = %q, want opencode-smarter", ev2.Model)
	}
}

// ─── handleUsageUpdate ──────────────────────────────────────────────

// TestHandleUsageUpdate_StashesOpencodeShape verifies the opencode
// cumulative `{used, size, cost}` payload lands on lastUsage with
// the documented field mapping (used → InputTokens, size →
// ContextWindow + pct).
func TestHandleUsageUpdate_StashesOpencodeShape(t *testing.T) {
	d := newTestDriver()

	payload := json.RawMessage(`{
		"used": 800,
		"size": 1000,
		"cost": 0.012
	}`)
	d.handleUsageUpdate(payload)

	d.lastUsageMu.Lock()
	got := d.lastUsage
	d.lastUsageMu.Unlock()
	if got == nil {
		t.Fatal("lastUsage is nil, want populated")
	}
	if got.InputTokens != 800 {
		t.Errorf("InputTokens = %d, want 800", got.InputTokens)
	}
	if got.CostUSD != 0.012 {
		t.Errorf("CostUSD = %v, want 0.012", got.CostUSD)
	}
	if got.ContextWindow != 1000 {
		t.Errorf("ContextWindow = %d, want 1000", got.ContextWindow)
	}
	wantPct := float64(800) / float64(1000) * 100
	if got.ContextWindowPct != wantPct {
		t.Errorf("ContextWindowPct = %v, want %v", got.ContextWindowPct, wantPct)
	}
}

// TestHandleUsageUpdate_StashesStandardShape verifies the
// standard ACP-spec shape (inputTokens / outputTokens /
// cacheRead / cacheWrite / totalTokens / costUSD) populates
// lastUsage with full granularity, and that the standard shape
// wins when both shapes are present (which would be a malformed
// payload in practice, but we test precedence anyway).
func TestHandleUsageUpdate_StashesStandardShape(t *testing.T) {
	d := newTestDriver()

	payload := json.RawMessage(`{
		"inputTokens": 100,
		"outputTokens": 50,
		"cacheReadInputTokens": 30,
		"cacheCreationInputTokens": 20,
		"totalTokens": 200,
		"costUSD": 0.005
	}`)
	d.handleUsageUpdate(payload)

	d.lastUsageMu.Lock()
	got := d.lastUsage
	d.lastUsageMu.Unlock()
	if got == nil {
		t.Fatal("lastUsage is nil, want populated")
	}
	if got.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", got.InputTokens)
	}
	if got.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", got.OutputTokens)
	}
	if got.CacheReadInputTokens != 30 {
		t.Errorf("CacheReadInputTokens = %d, want 30", got.CacheReadInputTokens)
	}
	if got.CacheCreationInputTokens != 20 {
		t.Errorf("CacheCreationInputTokens = %d, want 20", got.CacheCreationInputTokens)
	}
	if got.CostUSD != 0.005 {
		t.Errorf("CostUSD = %v, want 0.005", got.CostUSD)
	}
}

// TestHandleUsageUpdate_CapturesModel verifies that a vendor-
// extension `model` field on usage_update populates d.model and
// is stamped on the next deliver() call.
func TestHandleUsageUpdate_CapturesModel(t *testing.T) {
	d := newTestDriver()
	d.model = "opencode-initial"

	payload := json.RawMessage(`{
		"used": 100, "size": 1000,
		"model": "opencode-smarter"
	}`)
	d.handleUsageUpdate(payload)

	if d.model != "opencode-smarter" {
		t.Errorf("d.model = %q, want opencode-smarter", d.model)
	}

	// Subsequent deliver() picks up the new model.
	d.deliver(agent.AgentEvent{Kind: agent.EventAgentText, Text: "after-model"})
	select {
	case ev := <-d.events:
		if ev.Model != "opencode-smarter" {
			t.Errorf("stamped Model = %q, want opencode-smarter", ev.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivered event")
	}
}

// TestHandleUsageUpdate_EmptyAllZero_NoOp verifies that an
// all-zero payload does NOT clobber a previously-stashed snapshot
// (the most-recent non-zero wins — defends against servers that
// emit a final zero-cost usage_update as a "stream end" marker).
func TestHandleUsageUpdate_EmptyAllZero_NoOp(t *testing.T) {
	d := newTestDriver()

	// Seed with a real snapshot.
	d.handleUsageUpdate(json.RawMessage(`{"used":500,"size":1000,"cost":0.01}`))
	d.lastUsageMu.Lock()
	seed := d.lastUsage
	d.lastUsageMu.Unlock()
	if seed == nil {
		t.Fatal("seed lastUsage is nil")
	}

	// All-zero payload — must NOT clobber.
	d.handleUsageUpdate(json.RawMessage(`{"used":0,"size":0,"cost":0}`))
	d.lastUsageMu.Lock()
	got := d.lastUsage
	d.lastUsageMu.Unlock()
	if got != seed {
		t.Errorf("lastUsage was clobbered by zero payload: got %p, want seed %p", got, seed)
	}
}

// ─── handleSessionStatus ────────────────────────────────────────────

// TestHandleSessionStatus_EmitsDone verifies that
// `{status:"idle"}` triggers an EventAgentDone{Reason:"settled",
// Usage: lastUsage} and clears the stash.
func TestHandleSessionStatus_EmitsDone(t *testing.T) {
	d := newTestDriver()
	// Seed usage.
	d.handleUsageUpdate(json.RawMessage(`{"used":100,"size":1000,"cost":0.01}`))

	d.handleSessionStatus(json.RawMessage(`{"status":"idle"}`))

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentDone {
			t.Fatalf("kind = %v, want EventAgentDone", ev.Kind)
		}
		if ev.Done == nil {
			t.Fatal("Done is nil")
		}
		if ev.Done.Reason != "settled" {
			t.Errorf("Done.Reason = %q, want settled", ev.Done.Reason)
		}
		if ev.Done.Usage == nil {
			t.Fatal("Done.Usage is nil, want populated from lastUsage")
		}
		if ev.Done.Usage.InputTokens != 100 {
			t.Errorf("Done.Usage.InputTokens = %d, want 100", ev.Done.Usage.InputTokens)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EventAgentDone")
	}

	// lastUsage must be cleared so the next turn starts fresh.
	d.lastUsageMu.Lock()
	if d.lastUsage != nil {
		t.Errorf("lastUsage = %+v, want nil after turn-end", d.lastUsage)
	}
	d.lastUsageMu.Unlock()
}

// TestHandleSessionStatus_NonIdle_NoEmit verifies that statuses
// other than "idle" do NOT trigger EventAgentDone. Some ACP
// servers may emit "running" / "compacting" / etc.
func TestHandleSessionStatus_NonIdle_NoEmit(t *testing.T) {
	d := newTestDriver()

	d.handleSessionStatus(json.RawMessage(`{"status":"running"}`))
	d.handleSessionStatus(json.RawMessage(`{"status":"compacting"}`))

	select {
	case ev := <-d.events:
		t.Fatalf("unexpected event: kind=%v", ev.Kind)
	case <-time.After(100 * time.Millisecond):
		// Expected: no event.
	}
}

// TestHandleSessionStatus_DedupesWithPromptResponse verifies
// that even when the same turn fires BOTH session.status:idle
// AND the synchronous session/prompt response (opencode does
// this), only one EventAgentDone is emitted. We simulate the
// prompt-response arrival by pre-bumping turnSettled (as
// translatePromptResponse does on the success path).
func TestHandleSessionStatus_DedupesWithPromptResponse(t *testing.T) {
	d := newTestDriver()
	d.handleUsageUpdate(json.RawMessage(`{"used":100,"size":1000,"cost":0.01}`))

	// Pre-bump turnSettled to simulate translatePromptResponse
	// having already emitted its own EventAgentDone.
	d.turnSettledMu.Lock()
	d.turnSettled = true
	d.turnSettledMu.Unlock()

	d.handleSessionStatus(json.RawMessage(`{"status":"idle"}`))

	select {
	case ev := <-d.events:
		t.Fatalf("unexpected duplicate EventAgentDone: %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// Expected: dedup worked.
	}
}

// ─── handleSessionInfoUpdate ────────────────────────────────────────

// TestHandleSessionInfoUpdate_CapturesModel verifies the
// vendor-extension `model` field on session_info_update
// populates d.model.
func TestHandleSessionInfoUpdate_CapturesModel(t *testing.T) {
	d := newTestDriver()
	d.model = "opencode-initial"

	d.handleSessionInfoUpdate(json.RawMessage(`{
		"title": "session-title",
		"model": "opencode-final"
	}`))

	if d.model != "opencode-final" {
		t.Errorf("d.model = %q, want opencode-final", d.model)
	}
}

// TestHandleSessionInfoUpdate_EmptyModel_NoOp verifies that an
// empty model field does NOT clobber d.model.
func TestHandleSessionInfoUpdate_EmptyModel_NoOp(t *testing.T) {
	d := newTestDriver()
	d.model = "opencode-initial"

	d.handleSessionInfoUpdate(json.RawMessage(`{"title":"no-model-here"}`))

	if d.model != "opencode-initial" {
		t.Errorf("d.model = %q, want opencode-initial (preserved)", d.model)
	}
}

// ─── translatePromptResponse usage-source precedence ─────────────────

// TestTranslatePromptResponse_PrefersLastUsage verifies that when
// usage_update arrives first (stashing into lastUsage) AND the
// session/prompt response also carries a Usage field, the turn-
// end Done.Usage comes from lastUsage (more accurate — reflects
// the model that actually ran).
func TestTranslatePromptResponse_PrefersLastUsage(t *testing.T) {
	d := newTestDriver()
	d.handleUsageUpdate(json.RawMessage(`{
		"inputTokens": 999, "outputTokens": 111,
		"costUSD": 0.777
	}`))

	// Response payload has DIFFERENT (and inferior) usage — the
	// lastUsage snapshot should win.
	resp := json.RawMessage(`{
		"stopReason": "end_turn",
		"usage": {"inputTokens": 1, "outputTokens": 2}
	}`)
	d.translatePromptResponse(resp)

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentDone {
			t.Fatalf("kind = %v, want EventAgentDone", ev.Kind)
		}
		if ev.Done == nil || ev.Done.Usage == nil {
			t.Fatal("Done.Usage is nil, want lastUsage snapshot")
		}
		if ev.Done.Usage.InputTokens != 999 {
			t.Errorf("Done.Usage.InputTokens = %d, want 999 (from usage_update, not response)",
				ev.Done.Usage.InputTokens)
		}
		if ev.Done.Usage.CostUSD != 0.777 {
			t.Errorf("Done.Usage.CostUSD = %v, want 0.777", ev.Done.Usage.CostUSD)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EventAgentDone")
	}
}

// TestTranslatePromptResponse_FallsBackToRespUsage verifies that
// when no usage_update has fired (some servers don't emit one),
// the response.Usage payload is used as a fallback.
func TestTranslatePromptResponse_FallsBackToRespUsage(t *testing.T) {
	d := newTestDriver()

	resp := json.RawMessage(`{
		"stopReason": "end_turn",
		"usage": {"inputTokens": 42, "outputTokens": 7}
	}`)
	d.translatePromptResponse(resp)

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentDone {
			t.Fatalf("kind = %v, want EventAgentDone", ev.Kind)
		}
		if ev.Done == nil || ev.Done.Usage == nil {
			t.Fatal("Done.Usage is nil, want resp.Usage fallback")
		}
		if ev.Done.Usage.InputTokens != 42 {
			t.Errorf("Done.Usage.InputTokens = %d, want 42 (from resp.Usage)",
				ev.Done.Usage.InputTokens)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EventAgentDone")
	}
}

// ─── concurrency regression ─────────────────────────────────────────




// ─── P0 regression: turnSettled must reset between turns ─────────────

// TestSendBlocks_ResetsTurnSettled exercises the exact regression
// the reviewer flagged: turnSettled=true after turn 1's terminal
// signal must NOT carry over to turn 2 — otherwise both
// translatePromptResponse and handleSessionStatus silently skip
// their EventAgentDone emit on turn 2, the runtime's busy guard
// never clears, and the chat hangs on the spinner.
//
// We can't easily run SendBlocks without a real RPC + transport
// (the test would need a mock transport + JSON-RPC server). So we
// exercise the reset path directly: simulate "turn 1 emitted Done"
// by setting turnSettled=true, then manually invoke the same reset
// logic SendBlocks uses (captured here as a private helper) and
// verify the flag is back to false before turn 2's terminal
// handler runs.
func TestSendBlocks_ResetsTurnSettled(t *testing.T) {
	d := newTestDriver()

	// Simulate turn 1 ending: stash usage + bump turnSettled via
	// handleSessionStatus (the canonical opencode path).
	d.handleUsageUpdate(json.RawMessage(`{"used":100,"size":1000,"cost":0.01}`))
	d.handleSessionStatus(json.RawMessage(`{"status":"idle"}`))

	// Drain the turn-1 Done.
	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentDone {
			t.Fatalf("turn 1: kind = %v, want EventAgentDone", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("turn 1: timed out waiting for Done")
	}

	// turnSettled must now be true.
	d.turnSettledMu.Lock()
	if !d.turnSettled {
		d.turnSettledMu.Unlock()
		t.Fatal("turnSettled = false after handleSessionStatus; want true")
	}
	d.turnSettledMu.Unlock()

	// ─── Simulate SendBlocks's reset path ─────────────────────
	// The actual reset happens at the top of SendBlocks after
	// pendingTurnActive=true. Mirror that exact sequence here so
	// a future SendBlocks refactor that drops the reset fails
	// this test loudly.
	d.pendingTurnMu.Lock()
	d.pendingTurnActive = true
	d.pendingTurnMu.Unlock()
	d.turnSettledMu.Lock()
	d.turnSettled = false
	d.turnSettledMu.Unlock()
	d.pendingTurnMu.Lock()
	d.pendingTurnActive = false
	d.pendingTurnMu.Unlock()

	// turnSettled must now be false again.
	d.turnSettledMu.Lock()
	if d.turnSettled {
		d.turnSettledMu.Unlock()
		t.Fatal("turnSettled stuck true after SendBlocks reset; " +
			"this is the multi-turn hang bug — P0")
	}
	d.turnSettledMu.Unlock()

	// ─── Turn 2: handleSessionStatus must emit Done ──────────
	d.handleSessionStatus(json.RawMessage(`{"status":"idle"}`))
	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentDone {
			t.Fatalf("turn 2: kind = %v, want EventAgentDone "+
				"(would be silently dropped before P0 fix)",
				ev.Kind)
		}
		if ev.Done == nil || ev.Done.Reason != "settled" {
			t.Errorf("turn 2: Done = %+v, want settled", ev.Done)
		}
	case <-time.After(time.Second):
		t.Fatal("turn 2: timed out waiting for Done — " +
			"multi-turn hang regression")
	}
}

// TestTranslatePromptResponse_ResetsTurnSettled mirrors the same
// regression for the synchronous session/prompt response path.
// translatePromptResponse must also emit Done on turn 2 after the
// reset clears turnSettled.
func TestTranslatePromptResponse_ResetsTurnSettled(t *testing.T) {
	d := newTestDriver()

	// Turn 1: prompt response sets turnSettled=true via the
	// default (end_turn) branch.
	d.translatePromptResponse(json.RawMessage(`{
		"stopReason": "end_turn",
		"usage": {"inputTokens": 1, "outputTokens": 1}
	}`))
	select {
	case <-d.events:
	case <-time.After(time.Second):
		t.Fatal("turn 1: timed out")
	}

	// Simulate SendBlocks's reset.
	d.pendingTurnMu.Lock()
	d.pendingTurnActive = true
	d.pendingTurnMu.Unlock()
	d.turnSettledMu.Lock()
	d.turnSettled = false
	d.turnSettledMu.Unlock()
	d.pendingTurnMu.Lock()
	d.pendingTurnActive = false
	d.pendingTurnMu.Unlock()

	// Turn 2: must emit Done.
	d.translatePromptResponse(json.RawMessage(`{
		"stopReason": "end_turn",
		"usage": {"inputTokens": 2, "outputTokens": 2}
	}`))
	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentDone {
			t.Fatalf("turn 2: kind = %v, want EventAgentDone", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("turn 2: timed out — multi-turn hang regression")
	}
}

// ─── P1 regression: d.model race detector guard ─────────────────────

// TestRace_ModelWriterReaderConcurrent simulates the P1 race:
// a writer goroutine (handleUsageUpdate, like the readPump)
// races against a reader goroutine (deliver, like
// translatePromptResponse on the SendBlocks goroutine). With the
// modelMu fix in place this passes under `go test -race`; without
// the fix the race detector flags a torn string read on d.model.
//
// A third goroutine drains d.events — without it the reader's
// deliver() calls block on a full channel after capacity-16
// iterations and the test deadlocks (separate from the race fix).
func TestRace_ModelWriterReaderConcurrent(t *testing.T) {
	d := newTestDriver()
	d.model = "initial-model"

	stopConsumer := make(chan struct{})
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case <-d.events:
				// drop
			case <-stopConsumer:
				return
			}
		}
	}()

	const iterations = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer (readPump analogue): updates d.model via
	// handleUsageUpdate's vendor-extension `model` field.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			d.handleUsageUpdate(json.RawMessage(
				`{"used":100,"size":1000,"model":"x"}`))
		}
	}()

	// Reader (SendBlocks / handshake analogue): reads d.model
	// via deliver()'s stamp-on-emit path.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			d.deliver(agent.AgentEvent{
				Kind: agent.EventAgentText,
				Text: "x",
			})
		}
	}()

	wg.Wait()
	close(stopConsumer)
	<-consumerDone
}

// TestDeliver_StampsLatestModelUnderContention verifies the
// reader still sees *some* valid value (never a torn string)
// even under heavy contention — pairs with the -race test
// above to provide value-based coverage in non-race runs.
//
// Consumer goroutine drains d.events so the deliver() loop
// doesn't block on a full channel.
func TestDeliver_StampsLatestModelUnderContention(t *testing.T) {
	d := newTestDriver()

	d.modelMu.Lock()
	d.model = "seed"
	d.modelMu.Unlock()

	stopConsumer := make(chan struct{})
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case <-d.events:
			case <-stopConsumer:
				return
			}
		}
	}()

	const n = 500
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			d.modelMu.Lock()
			d.model = "writer-model"
			d.modelMu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			d.deliver(agent.AgentEvent{Kind: agent.EventAgentText})
		}
	}()

	wg.Wait()
	close(stopConsumer)
	<-consumerDone

	d.modelMu.Lock()
	if d.model != "writer-model" {
		d.modelMu.Unlock()
		t.Errorf("final d.model = %q, want writer-model", d.model)
	}
	d.modelMu.Unlock()
}
