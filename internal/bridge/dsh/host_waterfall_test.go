// host_waterfall_test.go — unit tests for the dsh 0.1.2-rc.1 host
// waterfall demux and adapter (approval/request,
// user-questions/request). Pinned against the real wire shapes from
// @deepseek-ai/dsh-user-approval/types.d.ts and
// @deepseek-ai/dsh-user-questions/types.d.ts (verified by raw
// read at fix-dsh-ask-question planning time).
package dsh

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestHandleHostFrame_ApprovalRequest_FiresEventAgentPermission
// pins the host waterfall path end-to-end: drive
// handleHostFrame("approval/request") on a registered driver, then
// drain the events chan and assert the EventAgentPermission shape
// matches what Feishu's permission card expects.
func TestHandleHostFrame_ApprovalRequest_FiresEventAgentPermission(t *testing.T) {
	resetHostWaterfallStateForTest(t)
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-host-waterfall-approval"
	t.Cleanup(func() { close(d.closed) })

	registerDriverForWaterfall(d)

	// clientId capture moved to the dispatch path
	// (host/stream.go:case "ready" → host.SetHostClientID) in the
	// F-hostready-install-race fix. In production the dispatch
	// goroutine does this when the mux WS connects; the test
	// stands in for it by setting the slot directly.
	host.SetHostClientID("test-client-id")

	env := waterfallEnvelope{
		AgentID: d.sessionID,
		Request: json.RawMessage(mustJSON(t, map[string]any{
			"agent":    map[string]any{"id": d.sessionID},
			"toolName": "Bash",
			"reason":   "rm -rf",
		})),
	}
	payload := mustJSON(t, env)

	d.handleHostFrame("approval/request", "rpc-host-app-1", []byte(payload))

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentPermission {
			t.Fatalf("kind = %v, want EventAgentPermission", ev.Kind)
		}
		if ev.Permission == nil {
			t.Fatal("Permission is nil")
		}
		if ev.Permission.Kind != agent.PermissionKindApproval {
			t.Errorf("Permission.Kind = %v, want PermissionKindApproval", ev.Permission.Kind)
		}
		if ev.Permission.Tool != "Bash" {
			t.Errorf("Permission.Tool = %q, want Bash", ev.Permission.Tool)
		}
		if ev.Permission.Action != "rm -rf" {
			t.Errorf("Permission.Action = %q, want rm -rf", ev.Permission.Action)
		}
		if len(ev.Permission.Options) != 2 ||
			ev.Permission.Options[0] != approvalAllowOnce ||
			ev.Permission.Options[1] != approvalReject {
			t.Errorf("Options = %v, want [Allow once, Reject]", ev.Permission.Options)
		}
		if ev.Permission.ResponseCh == nil {
			t.Fatal("Permission.ResponseCh is nil")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approval event from host waterfall")
	}

	// SendPermission pops the FIFO + posts /api/respond. Runtime
	// would call this when the user clicks a card button; in the
	// test we drive it directly so the mock fires.
	if err := d.SendPermission(approvalAllowOnce); err != nil {
		t.Fatalf("SendPermission: %v", err)
	}

	if err := waitForNoError(t, 2*time.Second, func() error {
		if mock.count.Load() == 0 {
			return errors.New("no respond call yet")
		}
		return nil
	}); err != nil {
		t.Fatalf("respond mock never fired: %v", err)
	}

	var env2 struct {
		Type    string `json:"type"`
		RPCID   string `json:"rpcId"`
		Method  string `json:"method"`
		Payload struct {
			Args struct {
				ClientID string `json:"clientId"`
				EventID  string `json:"eventId"`
				Outcome  struct {
					Kind  string          `json:"kind"`
					Value json.RawMessage `json:"value"`
				} `json:"outcome"`
			} `json:"args"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(mock.body(), &env2); err != nil {
		t.Fatalf("decode respond body: %v", err)
	}
	if env2.Method != "$events/result" {
		t.Errorf("method = %q, want $events/result", env2.Method)
	}
	if env2.Payload.Args.ClientID != "test-client-id" {
		t.Errorf("clientId = %q, want test-client-id", env2.Payload.Args.ClientID)
	}
	if env2.Payload.Args.EventID != "rpc-host-app-1" {
		t.Errorf("eventId = %q, want rpc-host-app-1 (host waterfall eventId)", env2.Payload.Args.EventID)
	}
	if env2.Payload.Args.Outcome.Kind != "result" {
		t.Errorf("outcome.kind = %q, want result", env2.Payload.Args.Outcome.Kind)
	}
	var outcomeStr string
	if err := json.Unmarshal(env2.Payload.Args.Outcome.Value, &outcomeStr); err != nil {
		t.Fatalf("decode outcome.value: %v", err)
	}
	if outcomeStr != "allowed-once" {
		t.Errorf("outcome.value = %q, want allowed-once", outcomeStr)
	}

	// Pending FIFO must be empty after the answer lands — otherwise
	// SendPermission would route the user's next click to a stale
	// entry.
	d.pendingMu.Lock()
	remaining := len(d.pendingApprovals)
	d.pendingMu.Unlock()
	if remaining != 0 {
		t.Errorf("pendingApprovals still has %d entries after SendPermission", remaining)
	}
}

// TestHandleHostFrame_UserQuestionsRequest_FiresEventAgentPermission
// pins the AskUserQuestion path with a multi-option question batch.
func TestHandleHostFrame_UserQuestionsRequest_FiresEventAgentPermission(t *testing.T) {
	resetHostWaterfallStateForTest(t)
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-host-waterfall-ask"
	t.Cleanup(func() { close(d.closed) })

	registerDriverForWaterfall(d)

	// clientId capture moved to the dispatch path
	// (host/stream.go:case "ready" → host.SetHostClientID) in the
	// F-hostready-install-race fix. In production the dispatch
	// goroutine does this when the mux WS connects; the test
	// stands in for it by setting the slot directly.
	host.SetHostClientID("test-client-id")

	env := waterfallEnvelope{
		AgentID: d.sessionID,
		Request: json.RawMessage(mustJSON(t, map[string]any{
			"questions": []map[string]any{
				{
					"id":       "q1",
					"header":   "DB",
					"question": "Pick a database",
					"options": []map[string]any{
						{"label": "PostgreSQL", "description": "Production"},
						{"label": "MySQL", "description": "Legacy"},
						{"label": "SQLite", "description": "Dev"},
					},
					"multiSelect": false,
				},
			},
			"agent": map[string]any{"id": d.sessionID},
		})),
	}
	payload := mustJSON(t, env)

	d.handleHostFrame("user-questions/request", "rpc-host-q-1", []byte(payload))

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentPermission {
			t.Fatalf("kind = %v, want EventAgentPermission", ev.Kind)
		}
		if ev.Permission == nil {
			t.Fatal("Permission is nil")
		}
		if ev.Permission.Kind != agent.PermissionKindQuestion {
			t.Errorf("Permission.Kind = %v, want PermissionKindQuestion", ev.Permission.Kind)
		}
		if len(ev.Permission.Questions) != 1 {
			t.Fatalf("Questions = %d, want 1", len(ev.Permission.Questions))
		}
		q := ev.Permission.Questions[0]
		if q.ID != "q1" || q.Header != "DB" || q.Question != "Pick a database" {
			t.Errorf("question[0] = %+v", q)
		}
		wantOpts := []string{"PostgreSQL", "MySQL", "SQLite"}
		if len(q.Options) != 3 {
			t.Fatalf("question[0].Options = %v, want %v", q.Options, wantOpts)
		}
		for i, want := range wantOpts {
			if q.Options[i] != want {
				t.Errorf("Options[%d] = %q, want %q", i, q.Options[i], want)
			}
		}
		// One-shot channels only see the first question's labels in
		// the top-level Options field; the full batch stays in
		// Questions[] for the wizard path.
		if len(ev.Permission.Options) != 3 {
			t.Errorf("Permission.Options = %v, want %v", ev.Permission.Options, wantOpts)
		}
		if ev.Permission.ResponseCh == nil {
			t.Fatal("Permission.ResponseCh is nil")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ask event from host waterfall")
	}

	if err := d.SendPermission("PostgreSQL"); err != nil {
		t.Fatalf("SendPermission: %v", err)
	}

	if err := waitForNoError(t, 2*time.Second, func() error {
		if mock.count.Load() == 0 {
			return errors.New("no respond call yet")
		}
		return nil
	}); err != nil {
		t.Fatalf("respond mock never fired: %v", err)
	}

	var env2 struct {
		Type    string `json:"type"`
		RPCID   string `json:"rpcId"`
		Method  string `json:"method"`
		Payload struct {
			Args struct {
				ClientID string `json:"clientId"`
				EventID  string `json:"eventId"`
				Outcome  struct {
					Kind  string          `json:"kind"`
					Value json.RawMessage `json:"value"`
				} `json:"outcome"`
			} `json:"args"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(mock.body(), &env2); err != nil {
		t.Fatalf("decode respond body: %v", err)
	}
	if env2.Method != "$events/result" {
		t.Errorf("method = %q, want $events/result", env2.Method)
	}
	if env2.Payload.Args.EventID != "rpc-host-q-1" {
		t.Errorf("eventId = %q, want rpc-host-q-1", env2.Payload.Args.EventID)
	}
	if env2.Payload.Args.Outcome.Kind != "result" {
		t.Errorf("outcome.kind = %q, want result", env2.Payload.Args.Outcome.Kind)
	}
	var answer struct {
		Answers []struct {
			ID       string   `json:"id"`
			Selected []string `json:"selected"`
			Custom   string   `json:"custom,omitempty"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(env2.Payload.Args.Outcome.Value, &answer); err != nil {
		t.Fatalf("decode outcome.value: %v", err)
	}
	if len(answer.Answers) != 1 {
		t.Fatalf("answers = %d, want 1", len(answer.Answers))
	}
	if answer.Answers[0].ID != "q1" {
		t.Errorf("answer[0].ID = %q, want q1", answer.Answers[0].ID)
	}
	if len(answer.Answers[0].Selected) != 1 || answer.Answers[0].Selected[0] != "PostgreSQL" {
		t.Errorf("answer[0].Selected = %v, want [PostgreSQL]", answer.Answers[0].Selected)
	}
}

// TestHostWaterfallHandler_UnsubscribedSessionDrops pins the
// package-level demux map: a host waterfall for a sessionId the
// driver hasn't registered must be silently dropped (debug log)
// without affecting any driver.
func TestHostWaterfallHandler_UnsubscribedSessionDrops(t *testing.T) {
	resetHostWaterfallStateForTest(t)
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-known"
	t.Cleanup(func() { close(d.closed) })

	registerDriverForWaterfall(d)

	// Pretend hostWaterfallHandler is the cli.SetHostHandler we
	// installed. The package guard inside installHostHandler makes
	// the function call idempotent across tests in the same
	// process — calling hostWaterfallHandler directly exercises
	// the demux without re-installing.
	env := waterfallEnvelope{
		AgentID: "session-foreign",
		Request: json.RawMessage(mustJSON(t, map[string]any{
			"agent":    map[string]any{"id": "session-foreign"},
			"toolName": "Bash",
			"reason":   "rm -rf",
		})),
	}
	hostWaterfallHandler("approval/request", "rpc-foreign-1", []byte(mustJSON(t, env)))

	// Driver for session-known must NOT receive the foreign
	// waterfall; nothing on its events chan within a short window.
	select {
	case ev := <-d.events:
		t.Fatalf("driver received foreign waterfall: %+v", ev)
	case <-time.After(150 * time.Millisecond):
		// expected
	}
	if mock.count.Load() != 0 {
		t.Errorf("respond mock fired on foreign waterfall, count = %d", mock.count.Load())
	}
}

// TestHandleHostFrame_MalformedBodyRecovered pins the recover
// contract: a body that fails inner unmarshal must not crash the
// handler, must not deliver a half-decoded event, and must leave
// the driver ready for the next waterfall.
func TestHandleHostFrame_MalformedBodyRecovered(t *testing.T) {
	resetHostWaterfallStateForTest(t)
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-bad-body"
	t.Cleanup(func() { close(d.closed) })

	registerDriverForWaterfall(d)

	// Inner request is an integer where the struct expects an
	// object — json.Unmarshal returns an error (no panic), the
	// handler logs and returns. The recover shim exists for the
	// narrower case where downstream code (e.g. optionLabels on
	// nil options) panics; we also exercise that path with a
	// request whose questions array is structurally valid but
	// contains a nil options list — optionLabels on nil is a
	// no-op (range over nil slice is fine in Go), so we instead
	// use a request whose envelope decodes but the body type is
	// wrong, exercising the Unmarshal error branch.
	env := waterfallEnvelope{
		AgentID: d.sessionID,
		Request: json.RawMessage(`{"agent":"not-an-object","toolName":42}`),
	}
	d.handleHostFrame("approval/request", "rpc-bad-1", []byte(mustJSON(t, env)))

	select {
	case ev := <-d.events:
		t.Fatalf("malformed waterfall produced event: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
	if mock.count.Load() != 0 {
		t.Errorf("respond mock fired on malformed waterfall, count = %d", mock.count.Load())
	}

	// Subsequent valid waterfall must still flow — proves the
	// handler didn't wedge.
	good := waterfallEnvelope{
		AgentID: d.sessionID,
		Request: json.RawMessage(mustJSON(t, map[string]any{
			"agent":    map[string]any{"id": d.sessionID},
			"toolName": "Bash",
			"reason":   "ls",
		})),
	}
	d.handleHostFrame("approval/request", "rpc-good-1", []byte(mustJSON(t, good)))
	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentPermission {
			t.Errorf("kind = %v, want EventAgentPermission", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for post-recover event")
	}
}

// TestHostWaterfallRegisterUnregister pins the package-level map
// lifecycle: register two drivers, unregister one, assert the
// remaining driver still demuxes correctly; double-unregister is
// a no-op.
func TestHostWaterfallRegisterUnregister(t *testing.T) {
	resetHostWaterfallStateForTest(t)
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	dA := newTestDriver(cli, "/tmp/a")
	dA.sessionID = "sess-A"
	dB := newTestDriver(cli, "/tmp/b")
	dB.sessionID = "sess-B"
	t.Cleanup(func() {
		close(dA.closed)
		close(dB.closed)
	})

	registerDriverForWaterfall(dA)
	registerDriverForWaterfall(dB)

	hostWaterfallMu.RLock()
	n := len(hostWaterfallBySess)
	hostWaterfallMu.RUnlock()
	if n != 2 {
		t.Fatalf("map size = %d, want 2", n)
	}

	// Waterfall for sess-A must reach dA, not dB.
	envA := waterfallEnvelope{
		AgentID: "sess-A",
		Request: json.RawMessage(mustJSON(t, map[string]any{
			"agent":    map[string]any{"id": "sess-A"},
			"toolName": "Bash",
			"reason":   "A-only",
		})),
	}
	hostWaterfallHandler("approval/request", "rpc-A-1", []byte(mustJSON(t, envA)))
	select {
	case ev := <-dA.events:
		if ev.Permission == nil || ev.Permission.Action != "A-only" {
			t.Errorf("dA got wrong event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("dA did not receive A's waterfall")
	}
	select {
	case ev := <-dB.events:
		t.Fatalf("dB received A's waterfall: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	// Unregister dB, register again is no-op for already-present
	// driver (map entry still dB); then unregister dB.
	unregisterDriverForWaterfall(dB)
	hostWaterfallMu.RLock()
	n = len(hostWaterfallBySess)
	hostWaterfallMu.RUnlock()
	if n != 1 {
		t.Fatalf("after unregister dB, map size = %d, want 1", n)
	}

	// Double-unregister is idempotent.
	unregisterDriverForWaterfall(dB)
	hostWaterfallMu.RLock()
	n = len(hostWaterfallBySess)
	hostWaterfallMu.RUnlock()
	if n != 1 {
		t.Fatalf("after double-unregister, map size = %d, want 1", n)
	}

	// dA still receives its waterfall.
	envA2 := waterfallEnvelope{
		AgentID: "sess-A",
		Request: json.RawMessage(mustJSON(t, map[string]any{
			"agent":    map[string]any{"id": "sess-A"},
			"toolName": "Read",
			"reason":   "A-again",
		})),
	}
	hostWaterfallHandler("approval/request", "rpc-A-2", []byte(mustJSON(t, envA2)))
	select {
	case ev := <-dA.events:
		if ev.Permission == nil || ev.Permission.Action != "A-again" {
			t.Errorf("dA got wrong second event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("dA did not receive second waterfall")
	}
}

// resetHostWaterfallStateForTest clears the package-level map and
// the captured clientId so each test starts from a known empty
// state. Without this, the test binary carries state across test
// cases.
func resetHostWaterfallStateForTest(t *testing.T) {
	t.Helper()
	hostWaterfallMu.Lock()
	defer hostWaterfallMu.Unlock()
	hostWaterfallBySess = nil
	// clientId lives in the host/ subpackage now (see
	// host/host_state.go). SetHostClientID("") is a no-op by
	// design (don't let a malformed ready clobber a good id);
	// the cross-package test reset lives at host.ResetHostClientIDForTest.
	host.ResetHostClientIDForTest()
}

// TestHandleHostFrame_HostCancel_DropsPending pins the server-side
// waterfall cancel path: when dsh sends {type:"cancel", eventId:…}
// for an in-flight approval or question, dropPendingByRPCID must
// drain the pending entry without firing /api/respond. Without
// this the runtime's permission card stays interactive for up to
// 5 minutes (until the permissionTimeout watchdog fires).
func TestHandleHostFrame_HostCancel_DropsPending(t *testing.T) {
	resetHostWaterfallStateForTest(t)
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-host-cancel"
	t.Cleanup(func() { close(d.closed) })

	registerDriverForWaterfall(d)

	d.handleHostFrame("user-questions/request", "rpc-cancel-q", []byte(mustJSON(t, waterfallEnvelope{
		AgentID: d.sessionID,
		Request: json.RawMessage(mustJSON(t, map[string]any{
			"questions": []map[string]any{
				{"id": "q1", "question": "?", "options": []map[string]any{{"label": "A"}}},
			},
		})),
	})))

	// Drain the EventAgentPermission so the runtime side is
	// waiting on the response channel.
	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentPermission {
			t.Fatalf("kind = %v, want EventAgentPermission", ev.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial ask event")
	}

	// Server-side cancel arrives.
	d.handleHostFrame("host/cancel", "rpc-cancel-q", nil)

	// No /api/respond (or /api/$events/result) must fire — the
	// server cancelled without expecting a reply.
	if err := waitForNoError(t, 200*time.Millisecond, func() error {
		if mock.count.Load() != 0 {
			return fmt.Errorf("respond mock fired %d times after cancel", mock.count.Load())
		}
		return nil
	}); err != nil {
		t.Fatalf("mock fired after cancel: %v", err)
	}

	// Pending entry must be gone — a subsequent SendPermission
	// against the same rpcID is a no-op (and the FIFO is empty
	// after our initial drain, so SendPermission should error
	// out cleanly).
	if err := d.SendPermission("A"); err == nil {
		t.Fatal("SendPermission after host/cancel should fail (no pending)")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func waitForNoError(t *testing.T, timeout time.Duration, fn func() error) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout after %s", timeout)
	}
	return lastErr
}
