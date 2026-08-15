// handle_mux_test.go — direct coverage for the mux-frame switch in
// handle_mux.go. Previously the mux-frame layer was tested only
// indirectly via dispatcher (which feeds it session/event) and
// e2e tests (which require real dsh web).
//
// These tests pin the bookkeeping behavior added in F-DSH-CHAT-001
// P4: every mux frame must be recorded in the wire ring buffer,
// and unknown methods must bump the unknown counter + log at Warn
// level.
//
// The routed cases (approval/requested, question/requested,
// approval/asked) delegate to other driver methods that require
// real WebSocket/permission setup — they remain e2e-tested via
// auto_resume_e2e_test.go and friends.

package dsh

import (
	"encoding/json"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// makeMinimalDriverForMuxTest builds a *driver with just enough
// state to drive handleMuxFrame: wireState + events chan + closed
// chan. We do NOT start lifecycle / pumps; handleMuxFrame is a
// pure method on driver state for the bookkeeping cases.
//
// dispatcher is wired (for session/event routing) with a no-op
// deliver closure. Tests that exercise non-session/event paths
// simply don't trigger dispatcher.
func makeMinimalDriverForMuxTest(t *testing.T) *driver {
	t.Helper()
	tr := newTranslator("dsh-test", "/tmp")
	st := newWireState()
	d := &driver{
		sessionID: "test-session",
		agentName: "dsh-test",
		workspace: "/tmp",
		wireState: st,
		translate: tr,
		events:    make(chan agent.AgentEvent, 64),
		closed:    make(chan struct{}),
		exitDone:  make(chan struct{}),
	}
	d.dispatcher = newDispatcher(tr, st, nil, func(agent.AgentEvent) {})
	return d
}

// TestHandleMuxFrame_RecordsAllKnownMethods
// P4 contract: every mux frame method (known or unknown) must be
// recorded in the wire ring buffer for ops triage via DumpWireStats.
func TestHandleMuxFrame_RecordsAllKnownMethods(t *testing.T) {
	cases := []struct {
		method   string
		payload  string
		envType  string // expected EnvelopeType in ring buffer ("" for non-session/event)
	}{
		{"session/subscribed", `{"sessionId":"s","lastSeq":0}`, ""},
		{"session/projection", `{"projection":"todo","value":{"items":[]}}`, ""},
		{"approval/resolved", `{"approvalId":"a","outcome":"approved"}`, ""},
		{"question/resolved", `{"questionRpcId":"q","outcome":"answered"}`, ""},
		{"session/queue", `{"items":[]}`, ""},
		{"session/jobs", `{"jobs":[]}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			d := makeMinimalDriverForMuxTest(t)
			d.handleMuxFrame(tc.method, "rpc-1", []byte(tc.payload))

			_, frames := d.wireState.DumpWireStats()
			if len(frames) != 1 {
				t.Fatalf("len(frames) = %d, want 1", len(frames))
			}
			if frames[0].Method != tc.method {
				t.Errorf("frames[0].Method = %q, want %q", frames[0].Method, tc.method)
			}
			if frames[0].EnvelopeType != tc.envType {
				t.Errorf("frames[0].EnvelopeType = %q, want %q", frames[0].EnvelopeType, tc.envType)
			}
		})
	}
}

// TestHandleMuxFrame_UnknownMethod_CountsAndWarns
// P4 contract: unknown mux method bumps the unknown counter and
// the recent ring buffer frame is observable. (slog.Warn output
// is not asserted — only the side effects.)
func TestHandleMuxFrame_UnknownMethod_CountsAndWarns(t *testing.T) {
	d := makeMinimalDriverForMuxTest(t)
	d.handleMuxFrame("future/method/that/dsh/will/invent", "rpc-x",
		[]byte(`{"some":"payload"}`))

	unknownTotal, frames := d.wireState.DumpWireStats()
	if unknownTotal != 1 {
		t.Errorf("unknownTotal = %d, want 1", unknownTotal)
	}
	if len(frames) != 1 {
		t.Fatalf("len(frames) = %d, want 1", len(frames))
	}
	if frames[0].Method != "future/method/that/dsh/will/invent" {
		t.Errorf("frames[0].Method = %q, want unknown method name", frames[0].Method)
	}
	if frames[0].EnvelopeType != "" {
		t.Errorf("frames[0].EnvelopeType = %q, want empty (mux-level, not session/event)", frames[0].EnvelopeType)
	}
}

// TestHandleMuxFrame_SessionEvent_RecordsEnvelopeType
// session/event frames must be recorded with the envelope Type
// populated (other mux methods record with empty EnvelopeType).
func TestHandleMuxFrame_SessionEvent_RecordsEnvelopeType(t *testing.T) {
	d := makeMinimalDriverForMuxTest(t)
	// Wire a minimal valid session/event payload. We don't expect
	// events to flow (the dispatcher handler runs but no deliver
	// channel reader is consuming in this test).
	muxPayload := []byte(`{
		"sessionId":"s-1",
		"event":{"type":"tool/call","data":{"callId":"c-1","name":"Read","arguments":"{}"}},
		"view":null
	}`)
	d.handleMuxFrame("session/event", "rpc-y", muxPayload)

	_, frames := d.wireState.DumpWireStats()
	if len(frames) != 1 {
		t.Fatalf("len(frames) = %d, want 1", len(frames))
	}
	// Note: dispatcher also records its own frame internally,
	// but that's also "session/event" + EnvelopeType="tool/call".
	// We expect 2 frames here.
	if frames[0].Method != "session/event" {
		t.Errorf("frames[0].Method = %q, want session/event", frames[0].Method)
	}
	if frames[0].EnvelopeType != "tool/call" {
		t.Errorf("frames[0].EnvelopeType = %q, want tool/call", frames[0].EnvelopeType)
	}
}

// TestHandleMuxFrame_SessionProjection_UpdatesWireState
// Projection frames feed wireState.applyProjection. We don't assert
// on the produced event (covered by TestWireState_ApplyProjection_*),
// just that the frame is recorded.
func TestHandleMuxFrame_SessionProjection_UpdatesWireState(t *testing.T) {
	d := makeMinimalDriverForMuxTest(t)
	projPayload := []byte(`{
		"projection":"todo",
		"value":{"items":[{"id":"p-1","content":"Test","status":"completed"}]}
	}`)
	d.handleMuxFrame("session/projection", "rpc-z", projPayload)

	// wireState.tasks should have p-1.
	d.wireState.mu.Lock()
	got, ok := d.wireState.tasks["p-1"]
	d.wireState.mu.Unlock()
	if !ok {
		t.Fatal("wireState.tasks[p-1] not populated")
	}
	if got.Content != "Test" {
		t.Errorf("tasks[p-1].Content = %q, want Test", got.Content)
	}

	// And the frame is recorded.
	_, frames := d.wireState.DumpWireStats()
	if len(frames) != 1 {
		t.Fatalf("len(frames) = %d, want 1", len(frames))
	}
	if frames[0].Method != "session/projection" {
		t.Errorf("frames[0].Method = %q, want session/projection", frames[0].Method)
	}
}

// TestHandleMuxFrame_MultipleFrames_Cumulative
// Ring buffer accumulates across multiple frames; unknownCount
// only bumps for unknown methods (known methods record but don't
// count).
func TestHandleMuxFrame_MultipleFrames_Cumulative(t *testing.T) {
	d := makeMinimalDriverForMuxTest(t)

	// 5 session/subscribed + 2 unknown = 7 frames, 2 unknown.
	for range 5 {
		d.handleMuxFrame("session/subscribed", "rpc", []byte(`{}`))
	}
	d.handleMuxFrame("dsh/will/invent/X", "rpc", []byte(`{}`))
	d.handleMuxFrame("dsh/will/invent/Y", "rpc", []byte(`{}`))

	unknownTotal, frames := d.wireState.DumpWireStats()
	if unknownTotal != 2 {
		t.Errorf("unknownTotal = %d, want 2", unknownTotal)
	}
	if len(frames) != 7 {
		t.Errorf("len(frames) = %d, want 7", len(frames))
	}
}

// TestHandleMuxFrame_InvalidJSON_DoesNotPanic
// Defensive: a mux frame with malformed JSON must not panic.
// session/subscribed, session/projection, etc. all handle decode
// errors by dLog-ing and returning.
func TestHandleMuxFrame_InvalidJSON_DoesNotPanic(t *testing.T) {
	cases := []string{
		"session/subscribed",
		"session/projection",
		"approval/resolved",
		"question/resolved",
	}
	for _, method := range cases {
		t.Run(method, func(t *testing.T) {
			d := makeMinimalDriverForMuxTest(t)
			// Not valid JSON. Should dLog + return, no panic.
			d.handleMuxFrame(method, "rpc", []byte(`not valid json`))
			// Frame should still be recorded (P4 fires before decode).
			_, frames := d.wireState.DumpWireStats()
			if len(frames) != 1 {
				t.Errorf("method %s: frame should be recorded even on decode failure, got %d frames", method, len(frames))
			}
		})
	}
}

// TestHandleMuxFrame_DispatchesSessionEventToDispatcher
// session/event must reach dispatcher.dispatch — verified by
// checking that an unknown envelope type bumps unknownCount (the
// dispatcher's path, not handle_mux's).
func TestHandleMuxFrame_DispatchesSessionEventToDispatcher(t *testing.T) {
	d := makeMinimalDriverForMuxTest(t)
	// Wire dispatcher to wireState + translator.
	d.dispatcher = newDispatcher(d.translate, d.wireState, d, func(agent.AgentEvent) {})

	// Send a session/event with an unknown envelope Type — should
	// bump unknownCount via dispatcher (not handle_mux's default).
	muxPayload := []byte(`{
		"sessionId":"s-1",
		"event":{"type":"dsh/future/event/type","data":{}}
	}`)
	d.handleMuxFrame("session/event", "rpc", muxPayload)

	unknownTotal, frames := d.wireState.DumpWireStats()
	if unknownTotal != 1 {
		t.Errorf("unknownTotal = %d, want 1 (dispatcher should bump for unknown envelope type)", unknownTotal)
	}
	// The ring buffer should have 2 frames: dispatcher's session/event
	// record + the actual frame record from handleMuxFrame's case.
	// Actually handle_mux's session/event case does NOT record (only
	// dispatcher does). So we expect 1 frame.
	if len(frames) != 1 {
		t.Errorf("len(frames) = %d, want 1", len(frames))
	}
	if frames[0].EnvelopeType != "dsh/future/event/type" {
		t.Errorf("frames[0].EnvelopeType = %q, want dsh/future/event/type", frames[0].EnvelopeType)
	}
	_ = json.RawMessage{} // keep encoding/json import for future tests
}