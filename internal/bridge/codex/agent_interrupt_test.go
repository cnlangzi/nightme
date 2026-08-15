// Regression tests for the codex fix-stop path.
//
// fix-stop replaces the pre-fix SIGINT-driven Stop with the codex
// app-server's structured turn/interrupt JSON-RPC method. SIGINT
// terminated the app-server and forced the chat layer to --resume
// a fresh process; on a thread whose previous turn was interrupted
// mid-flight the resume wedged in a state where turn/start was
// accepted but turn/completed never arrived (silent "Working..."
// until the HungPrompt watchdog fired 5 minutes later).
//
// The tests below pin:
//
//  1. Stop() sends turn/interrupt with the captured (threadId,
//     turnId) when a turn is in flight — NOT SIGINT.
//  2. Stop() is a no-op when no turn is in flight (busy=false).
//  3. Stop() returns ErrNotSupported before the session exists.
//  4. Stop() falls back to SIGINT when turn/interrupt returns
//     -32601 method-not-found (old codex builds).
//  5. SendBlocks captures turnId from turn/start's response into
//     d.currentTurnID; if turn/start RPC fails the busy guard is
//     released so the next SendBlocks can proceed.
//  6. Stop() waits briefly for currentTurnID when turn/start is
//     still in flight (busy=true, turnID="").
//  7. Translator reads Status from turn/completed; "interrupted"
//     sets Result.Err and Done.Reason="interrupted".

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── helpers ───

// interruptServer is a JSON-RPC 2.0 fake wired against one end
// of a net.Pipe. It auto-replies to turn/start (returning a
// canned turnId) and turn/interrupt (per stub.behaviour: ok /
// method-missing / stall), and exposes a non-destructive frame
// stream (observed) for tests to inspect.
//
// We do NOT use rpc_test.go's newServerStub because its drain
// goroutine consumes frames from a single channel — if our
// reply loop also reads from that channel we'd race for frames
// and either the test's inspection or the reply path would lose
// (symptom: turn/interrupt requests hang waiting for a reply
// the test popped instead of the reply loop). The fan-out
// pattern below (one drain → two channels: reply + observed)
// avoids the race entirely.
type interruptServer struct {
	conn net.Conn

	// reply consumes every frame the bridge writes; the reply
	// goroutine drains this, decides what to write back, and
	// writes the response to conn.
	reply chan []byte

	// observed exposes each frame to tests without competing
	// with the reply goroutine.
	observed chan map[string]any

	mu        sync.Mutex
	behaviour string

	done chan struct{}
}

// startWire wires a fresh driver + interruptServer against each
// end of a net.Pipe. Returns a driver whose Stop/SendBlocks
// routes through the stub; tests can inspect intercepted frames
// via stub.popFrame.
func startWire(t *testing.T, behaviour string) (*driver, *interruptServer) {
	t.Helper()

	clientConn, serverConn := net.Pipe()

	stub := &interruptServer{
		conn:      serverConn,
		reply:     make(chan []byte, 16),
		observed:  make(chan map[string]any, 16),
		behaviour: behaviour,
		done:      make(chan struct{}),
	}

	go runInterruptDrain(serverConn, stub)
	go runInterruptReplyLoop(stub)

	client := newRPCClient(clientConn,
		func(string, json.RawMessage, json.RawMessage) {},
		func(string, json.RawMessage) {},
	)
	// The bridge's readPump is what delivers response frames
	// onto the per-request waiter channel. Without it,
	// rpc.request blocks forever.
	go client.readPump(context.Background(), func(error) {})

	d := &driver{
		closed: make(chan struct{}),
		session: &session{
			workspace: t.TempDir(),
			threadID:  "th-test",
			rpc:       client,
		},
	}

	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
		<-stub.done
	})

	return d, stub
}

// runInterruptDrain reads LF-delimited JSON-RPC frames off conn
// and fans each one out to BOTH stub.reply (for the reply loop)
// AND stub.observed (for test inspection). One reader — two
// independent channels — eliminates the race between the reply
// loop and tests popping frames.
func runInterruptDrain(conn net.Conn, stub *interruptServer) {
	defer close(stub.done)

	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		pending = append(pending, buf[:n]...)
		for {
			idx := indexLF(pending)
			if idx < 0 {
				break
			}
			frame := append([]byte(nil), pending[:idx]...)
			pending = pending[idx+1:]

			stub.reply <- frame

			var m map[string]any
			if json.Unmarshal(frame, &m) == nil {
				select {
				case stub.observed <- m:
				default:
				}
			}
		}
	}
}

// indexLF returns the index of the first '\n' in b, or -1.
func indexLF(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// runInterruptReplyLoop pops each frame off stub.reply and
// writes the canned turn/start or turn/interrupt reply. turn/
// start always succeeds (returns "turn-abc"); turn/interrupt
// honours stub.behaviour.
func runInterruptReplyLoop(stub *interruptServer) {
	for frame := range stub.reply {
		var probe struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(frame, &probe); err != nil {
			continue
		}
		if len(probe.ID) == 0 {
			continue
		}
		switch probe.Method {
		case "turn/start":
			resp := `{"jsonrpc":"2.0","id":` + string(probe.ID) +
				`,"result":{"turn":{"id":"turn-abc"}}}`
			_, _ = stub.conn.Write([]byte(resp + "\n"))
		case "turn/interrupt":
			stub.mu.Lock()
			beh := stub.behaviour
			stub.mu.Unlock()
			switch beh {
			case "ok":
				resp := `{"jsonrpc":"2.0","id":` + string(probe.ID) +
					`,"result":{}}`
				_, _ = stub.conn.Write([]byte(resp + "\n"))
			case "method-missing":
				resp := `{"jsonrpc":"2.0","id":` + string(probe.ID) +
					`,"error":{"code":-32601,"message":"Method not found"}}`
				_, _ = stub.conn.Write([]byte(resp + "\n"))
			case "stall":
				// no reply; bridge will time out via stopRPCTimeout
			}
		}
	}
}

// popFrame pulls one intercepted frame off the stub's observed
// channel. Returns the parsed JSON as map[string]any, or nil
// on timeout.
func (s *interruptServer) popFrame(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case f := <-s.observed:
		return f
	case <-time.After(timeout):
		return nil
	}
}

// ─── SendBlocks: turnId capture ───

// TestDriver_SendBlocks_CapturesTurnId verifies the fix-stop
// plumbing: turn/start's response carries the turnId; SendBlocks
// must store it on the driver so Stop() can pass it to
// turn/interrupt. Pre-fix, the bridge discarded the response and
// Stop() had nothing to interrupt with.
func TestDriver_SendBlocks_CapturesTurnId(t *testing.T) {
	d, _ := startWire(t, "ok")

	if err := d.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello"},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	d.pendingMu.Lock()
	got := d.currentTurnID
	busy := d.pendingTurnActive
	d.pendingMu.Unlock()

	if got != "turn-abc" {
		t.Errorf("currentTurnID = %q, want %q", got, "turn-abc")
	}
	if !busy {
		t.Errorf("pendingTurnActive = false after SendBlocks; want true")
	}
}

// TestDriver_SendBlocks_ReleasesGuardOnRPCError pins the
// busy-guard invariant when turn/start's RPC fails (closed wire,
// EOF, etc.). Without the release, every subsequent SendBlocks
// would return ErrTurnBusy.
func TestDriver_SendBlocks_ReleasesGuardOnRPCError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Drop the bridge side immediately so the read loop hits
	// EOF and request() returns ErrSessionClosed.
	_ = serverConn.Close()

	client := newRPCClient(clientConn,
		func(string, json.RawMessage, json.RawMessage) {},
		func(string, json.RawMessage) {},
	)

	d := &driver{
		closed: make(chan struct{}),
		session: &session{
			workspace: t.TempDir(),
			threadID:  "th-test",
			rpc:       client,
		},
	}

	err := d.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello"},
	})
	if err == nil {
		t.Fatal("SendBlocks should fail when the wire is closed")
	}

	d.pendingMu.Lock()
	busy := d.pendingTurnActive
	tid := d.currentTurnID
	d.pendingMu.Unlock()
	if busy {
		t.Errorf("pendingTurnActive leaked after RPC failure")
	}
	if tid != "" {
		t.Errorf("currentTurnID = %q after RPC failure; want empty", tid)
	}
}

// ─── Stop ───

// TestDriver_Stop_NoSession returns ErrNotSupported when Start
// has not been called. Mirrors the pre-fix contract.
func TestDriver_Stop_NoSession(t *testing.T) {
	d := &driver{closed: make(chan struct{})}
	err := d.Stop(context.Background())
	if !errors.Is(err, agent.ErrNotSupported) {
		t.Errorf("Stop before Start = %v, want ErrNotSupported", err)
	}
}

// TestDriver_Stop_NoInFlightTurn is a no-op (nil) when no turn is
// active — mirrors stop.go's "Action=noop" branch so a second
// /stop in quick succession doesn't surface a spurious failure.
func TestDriver_Stop_NoInFlightTurn(t *testing.T) {
	d, _ := startWire(t, "ok")

	if err := d.Stop(context.Background()); err != nil {
		t.Errorf("Stop with no in-flight turn = %v, want nil", err)
	}
}

// TestDriver_Stop_CallsTurnInterrupt is the central fix-stop
// regression. With a turn in flight, Stop() must issue a
// turn/interrupt JSON-RPC carrying the captured (threadId,
// turnId), NOT SIGINT. The stub captures the frame and we
// assert on its Method and Params.
func TestDriver_Stop_CallsTurnInterrupt(t *testing.T) {
	d, stub := startWire(t, "ok")

	// Simulate SendBlocks state: turn in flight on thread
	// "th-test" with turnId "turn-abc".
	d.pendingMu.Lock()
	d.pendingTurnActive = true
	d.currentTurnID = "turn-abc"
	d.pendingMu.Unlock()

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	frame := stub.popFrame(t, 2*time.Second)
	if frame == nil {
		t.Fatal("bridge never wrote turn/interrupt within 2s")
	}
	if got := frame["method"]; got != "turn/interrupt" {
		t.Fatalf("method = %v, want turn/interrupt", got)
	}
	params, _ := frame["params"].(map[string]any)
	if got := params["threadId"]; got != "th-test" {
		t.Errorf("params.threadId = %v, want th-test", got)
	}
	if got := params["turnId"]; got != "turn-abc" {
		t.Errorf("params.turnId = %v, want turn-abc", got)
	}
}

// TestDriver_Stop_FallsBackOnMethodNotFound covers the old-codex
// compatibility path. Post-fix we prefer turn/interrupt but fall
// back to SIGINT when the app-server returns -32601. The test
// asserts the bridge issued turn/interrupt AND did not surface
// the JSON-RPC error to the caller — the SIGINT fallback path
// tries to signal cmd.Process which is nil in our fake session,
// so the fallback returns ErrNotSupported (or nil in a future
// refactor). Either is acceptable evidence the fallback branch
// was taken.
func TestDriver_Stop_FallsBackOnMethodNotFound(t *testing.T) {
	d, stub := startWire(t, "method-missing")

	d.pendingMu.Lock()
	d.pendingTurnActive = true
	d.currentTurnID = "turn-abc"
	d.pendingMu.Unlock()

	err := d.Stop(context.Background())
	if err != nil && !errors.Is(err, agent.ErrNotSupported) {
		t.Errorf("Stop with method-not-found = %v; want nil or ErrNotSupported "+
			"(SIGINT fallback), NOT a -32601 propagated to caller", err)
	}

	// Confirm the bridge actually issued turn/interrupt (and
	// got the -32601 reply), not SIGINT blindly.
	frame := stub.popFrame(t, 2*time.Second)
	if frame == nil {
		t.Fatal("bridge never wrote turn/interrupt within 2s")
	}
	if got := frame["method"]; got != "turn/interrupt" {
		t.Fatalf("method = %v, want turn/interrupt (the fallback path "+
			"only triggers after a turn/interrupt RPC)", got)
	}
}

// TestDriver_Stop_WaitsForTurnId covers the race window where
// turn/start has been issued but its response has not yet
// populated d.currentTurnID. Stop() must NOT issue
// turn/interrupt with an empty turnId — it must wait briefly
// for the turnId to land, then call turn/interrupt with the
// captured turnId.
//
// This test focuses on the wait-for-turnId logic itself; we
// drive Stop's wait path by setting pendingTurnActive=true and
// currentTurnID="" via the startWire helper, then populating
// currentTurnID from a goroutine. The stub is configured to
// stall on turn/interrupt (so Stop's RPC hangs and we observe
// the wait timing), but turn/start isn't actually issued — we
// only care that the wait-and-find-the-turnId path runs.
func TestDriver_Stop_WaitsForTurnId(t *testing.T) {
	d, _ := startWire(t, "stall")

	// Simulate the SendBlocks state: pendingTurnActive=true,
	// currentTurnID="" (race window).
	d.pendingMu.Lock()
	d.pendingTurnActive = true
	d.pendingMu.Unlock()

	// Populate currentTurnID after a delay. Without this the
	// bridge would never see a turnId and Stop() would fall
	// back to SIGINT (which is wrong for the post-fix happy
	// path).
	go func() {
		time.Sleep(60 * time.Millisecond)
		d.pendingMu.Lock()
		d.currentTurnID = "turn-abc"
		d.pendingMu.Unlock()
	}()

	start := time.Now()
	// Use a short ctx so the stall reply doesn't keep this test
	// running for stopRPCTimeout (3s). The point is the wait
	// window, not the RPC reply.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := d.Stop(ctx)
	elapsed := time.Since(start)

	// The error is the context-deadline from our short ctx
	// (since the stub stalls on turn/interrupt). We accept
	// that as evidence the turn/interrupt branch was taken —
	// the SIGINT fallback would have returned ErrNotSupported
	// immediately because the fake session has no cmd.
	if errors.Is(err, agent.ErrNotSupported) {
		t.Fatalf("Stop took the SIGINT fallback path (err=%v); should have "+
			"waited for currentTurnID and called turn/interrupt", err)
	}

	// Stop() must have actually waited, not returned
	// immediately. 60ms is the simulated turnId delay; allow
	// slack.
	if elapsed < 30*time.Millisecond {
		t.Errorf("Stop returned in %v; should have waited for currentTurnID "+
			"to populate before falling back", elapsed)
	}
}

// ─── Translator ───

// TestTranslator_TurnCompletedInterrupted pins the fix-stop
// change in translate.go: turn/completed with Status="interrupted"
// must surface as EventAgentDone{Reason:"interrupted"} plus a
// soft Err on the AgentEvent so channels can render the stopped
// indicator. Pre-fix, the bridge hardcoded Reason="settled" for
// both clean and interrupted completions, hiding the SIGINT
// failure mode from the channel layer.
func TestTranslator_TurnCompletedInterrupted(t *testing.T) {
	delivered := make(chan agent.AgentEvent, 8)
	deliver := func(ev agent.AgentEvent) agent.AgentEvent {
		delivered <- ev
		return ev
	}

	turnEndCalled := false
	onTurnEnd := func() { turnEndCalled = true }

	tr := newTranslator(deliver, "codex", "/ws", "main",
		nil, // stderrTail
		onTurnEnd,
	)

	// Open a turn so completeTurn's `t.turn.active` branch is
	// taken. Without this, completeTurn short-circuits via the
	// "no meaningful activity" path and never delivers Result or
	// Done.
	tr.turn.active = true

	// Fire turn/completed{status:"interrupted"} as the codex
	// app-server emits after a successful turn/interrupt.
	// Drain up to 3 events: completeTurn may flush an
	// EventAgentText (when pendingMsgs is non-empty) before
	// emitting EventAgentResult + EventAgentDone.
	params := []byte(`{"turnId":"turn-abc","status":"interrupted"}`)
	tr.notify("turn/completed", params)

	var resultEvent, doneEvent *agent.AgentEvent
	deadline := time.After(2 * time.Second)
	for resultEvent == nil || doneEvent == nil {
		select {
		case ev := <-delivered:
			switch ev.Kind {
			case agent.EventAgentResult:
				e := ev
				resultEvent = &e
			case agent.EventAgentDone:
				e := ev
				doneEvent = &e
			}
		case <-deadline:
			t.Fatalf("did not receive Result+Done within 2s (result=%v done=%v)",
				resultEvent, doneEvent)
		}
	}

	if resultEvent == nil || resultEvent.Result == nil {
		t.Fatal("EventAgentResult not delivered")
	}
	if resultEvent.Err == nil {
		t.Error("AgentEvent.Err = nil; want \"codex: turn interrupted\"")
	} else if resultEvent.Err.Error() != "codex: turn interrupted" {
		t.Errorf("AgentEvent.Err = %q; want \"codex: turn interrupted\"",
			resultEvent.Err.Error())
	}

	if doneEvent == nil || doneEvent.Done == nil {
		t.Fatal("EventAgentDone not delivered")
	}
	if got := doneEvent.Done.Reason; got != "interrupted" {
		t.Errorf("Done.Reason = %q; want \"interrupted\"", got)
	}

	if !turnEndCalled {
		t.Error("onTurnEnd was not called; busy guard will leak")
	}
}

// TestTranslator_TurnCompletedNormalKeepsSettledReason is the
// companion to TestTranslator_TurnCompletedInterrupted — it pins
// that a clean completion still emits Reason="settled" so the
// fix-stop change did not regress the happy path.
func TestTranslator_TurnCompletedNormalKeepsSettledReason(t *testing.T) {
	delivered := make(chan agent.AgentEvent, 8)
	deliver := func(ev agent.AgentEvent) agent.AgentEvent {
		delivered <- ev
		return ev
	}
	tr := newTranslator(deliver, "codex", "/ws", "main", nil, func() {})

	tr.turn.active = true
	tr.notify("turn/completed", []byte(`{"status":"completed"}`))

	var doneEv *agent.AgentEvent
	deadline := time.After(2 * time.Second)
	for doneEv == nil {
		select {
		case ev := <-delivered:
			if ev.Kind == agent.EventAgentDone {
				e := ev
				doneEv = &e
			}
		case <-deadline:
			t.Fatal("EventAgentDone not delivered within 2s")
		}
	}
	if doneEv.Done.Reason != "settled" {
		t.Errorf("Done.Reason = %q; want \"settled\" (clean completion)",
			doneEv.Done.Reason)
	}
}

// ─── sentinel ───

// TestIsRPCMethodNotFound is a unit-level check on the sentinel
// helper: positive cases (real -32601 errors), negative cases
// (random errors that mention "method not found" in their
// message but with a different code), and the nil case.
func TestIsRPCMethodNotFound(t *testing.T) {
	if isRPCMethodNotFound(nil) {
		t.Error("isRPCMethodNotFound(nil) = true; want false")
	}
	if !isRPCMethodNotFound(&rpcError{Code: codeMethodNotFound, Message: "x"}) {
		t.Error("isRPCMethodNotFound(-32601) = false; want true")
	}
	if isRPCMethodNotFound(&rpcError{Code: -32600, Message: "method not found in body"}) {
		t.Error("isRPCMethodNotFound(-32600) = true; want false (code mismatch)")
	}
	if !isRPCMethodNotFound(errors.New("codex: stop: method not found")) {
		t.Error("isRPCMethodNotFound(\"method not found\" string) = false; want true")
	}
	if isRPCMethodNotFound(errors.New("codex: stop: timeout")) {
		t.Error("isRPCMethodNotFound(\"timeout\") = true; want false")
	}
}