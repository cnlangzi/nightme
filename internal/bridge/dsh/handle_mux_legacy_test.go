// handle_mux_legacy_test.go — pins the mux top-level legacy
// fallback path added when dsh 0.1.2-rc.1 folded approval +
// question onto the host $events waterfall. If a future dsh rc
// reverts to the mux frame names, the bridge must surface a
// visible warning (not silently drop, not crash) so ops can
// catch the regression via DumpWireStats.
package dsh

import (
	"testing"
	"time"
)

// TestHandleMuxFrame_LegacyApprovalRequested_LogsAndDrops asserts
// that pushing a mux approval/requested frame through
// handleMuxFrame after the dsh 0.1.2-rc.1 wire shift is recorded
// as an unknown method (incremented in the wire-stats unknown
// counter) and produces no EventAgentPermission.
func TestHandleMuxFrame_LegacyApprovalRequested_LogsAndDrops(t *testing.T) {
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/legacy")
	d.sessionID = "session-legacy-mux"
	t.Cleanup(func() { close(d.closed) })

	before, _ := d.wireState.DumpWireStats()

	payload := mustJSON(t, map[string]any{
		"sessionId":  d.sessionID,
		"approvalId": "audit-only",
		"toolName":   "Bash",
		"reason":     "legacy mux path",
	})
	d.handleMuxFrame("approval/requested", "rpc-legacy-1", []byte(payload))

	after, _ := d.wireState.DumpWireStats()
	if after != before+1 {
		t.Errorf("unknown counter delta = %d, want 1 (before=%d after=%d)",
			after-before, before, after)
	}

	select {
	case ev := <-d.events:
		t.Fatalf("legacy mux frame produced event: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestHandleMuxFrame_LegacyQuestionRequested_LogsAndDrops mirrors
// the approval fallback test for the question path. Same
// expectation: recorded + counted + warn-logged, no event
// emitted, no /api/respond fired.
func TestHandleMuxFrame_LegacyQuestionRequested_LogsAndDrops(t *testing.T) {
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/legacy-q")
	d.sessionID = "session-legacy-mux-q"
	t.Cleanup(func() { close(d.closed) })

	before, _ := d.wireState.DumpWireStats()

	payload := mustJSON(t, map[string]any{
		"sessionId": d.sessionID,
		"questions": []map[string]any{
			{"id": "q1", "question": "?", "options": []map[string]any{{"label": "A"}}},
		},
	})
	d.handleMuxFrame("question/requested", "rpc-legacy-q-1", []byte(payload))

	after, _ := d.wireState.DumpWireStats()
	if after != before+1 {
		t.Errorf("unknown counter delta = %d, want 1 (before=%d after=%d)",
			after-before, before, after)
	}

	select {
	case ev := <-d.events:
		t.Fatalf("legacy mux question frame produced event: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
	if mock.count.Load() != 0 {
		t.Errorf("respond mock fired on legacy mux frame, count = %d", mock.count.Load())
	}

	// Confirm a real (non-legacy) mux method still works after
	// the legacy fallback fired — proves the unknown branch does
	// not poison subsequent dispatch. session/snapshot decodes
	// records (may be empty for this synthetic input) without
	// surfacing an unknown-method warning; we just need to verify
	// the frame is consumed without a panic.
	snapshot := []byte(mustJSON(t, map[string]any{
		"type":   "snapshot",
		"cursor": int64(0),
	}))
	d.handleMuxFrame("session/snapshot", "rpc-1", snapshot)
}
