// Regression tests for the /stop → "no events from pi after abort"
// bug (fix-pi-stop, 2026-08-19).
//
// Pre-fix, the pi driver.Stop only sent the abort RPC and returned.
// The original prompt RPC — which SendBlocks was waiting on — never
// got a response because pi only ACKs `abort`, not the abandoned
// `prompt`. SendBlocks's defer didn't fire, turnActive stayed true
// for up to promptTimeout (90s), and every subsequent SendBlocks
// bounced off ErrTurnBusy. The user saw a silent bridge.
//
// The fix has two pieces:
//   1. driver.Stop now calls rpcClient.failResponse on the in-flight
//      prompt RPC BEFORE sending abort. SendBlocks returns
//      ErrSessionClosed immediately, the defer clears turnActive,
//      Submit rolls back IsReady=true.
//   2. driver.SendBlocks records the prompt's request id on the
//      driver struct so Stop can find it.
//
// These tests pin both halves down. Two flavours:
//   - E2E tests via the mock pi script (testdata/pi_mock.sh)
//     driving the real Spawn → handshake → readPump → translate
//     → events pipeline. They cover the user-visible recovery
//     and the wire shapes pi actually exhibits.
//   - Unit tests on rpcClient + driver primitives. These run
//     without a subprocess and pin the contract that the E2E
//     tests depend on.
//
// Wire shapes covered (all driven by MOCK_PI_CONTROL_FILE
// pointing at a tmpfile that the mock re-reads per line):
//   - hung prompt + ack abort
//     → TestStop_FailsInFlightPromptRPC
//   - hung prompt + ack abort + new prompt on the same bridge
//     → TestStop_AllowsNewPromptAfterAbort
//   - hung prompt + ack abort w/o agent_settled
//     → TestStop_PiSilentAfterAbort_AllowsNewPrompt
//
// Plus unit tests:
//   - rpcClient.failResponse targeted close
//     → TestFailResponse_TargetedClose
//   - driver records inFlightPromptID and reuses it for next turn
//     → TestSendBlocks_RecordsInFlightPromptID

package pi

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// waitForTurnActive polls the driver until inFlightPromptID +
// promptCancel are both set under turnMu, or the deadline
// expires. Used to synchronize E2E tests with the moment
// SendBlocks has registered its prompt — preferable to a fixed
// sleep because it adapts to CI jitter and gives a deterministic
// "parked" signal rather than guessing a worst-case latency.
//
// Returns the promptID so tests can correlate the parked id with
// subsequent RPC log lines if they want.
func waitForTurnActive(t *testing.T, d *driver, deadline time.Duration) string {
	t.Helper()
	end := time.Now().Add(deadline)
	var id string
	for time.Now().Before(end) {
		d.turnMu.Lock()
		id = d.inFlightPromptID
		active := d.turnActive
		hasCancel := d.promptCancel != nil
		d.turnMu.Unlock()
		if id != "" && active && hasCancel {
			return id
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("SendBlocks did not park on prompt RPC within %s; turnActive or promptCancel not set", deadline)
	return ""
}

// mockControlFile is a tmpfile path the tests pass via
// MOCK_PI_CONTROL_FILE so the running mock script can be told to
// switch modes mid-test. Env vars are baked into the subprocess
// at fork time, so a normal t.Setenv toggle would not be visible
// to a child that's already running — the mock has to read its
// mode from a file on every dispatch iteration.
type mockControlFile string

func newMockControlFile(t *testing.T) mockControlFile {
	t.Helper()
	return mockControlFile(filepath.Join(t.TempDir(), "mock-pi-mode"))
}

func (c mockControlFile) setMode(t *testing.T, mode string) {
	t.Helper()
	if err := os.WriteFile(string(c), []byte(mode+"\n"), 0o644); err != nil {
		t.Fatalf("write mock control file %s: %v", c, err)
	}
}

// startMockBridge wires up the shared mock pi script as a real
// subprocess driver so the tests below exercise the same code
// path the runtime hits in production (Spawn → handshake →
// readPump → translate → events). The mock watches ctrl on
// every dispatch iteration so tests can flip between
// "abort-only" and "all" mid-run.
func startMockBridge(t *testing.T, workspace string, ctrl mockControlFile) (*agent.Agent, func()) {
	t.Helper()
	mockPath, err := filepath.Abs("../../testdata/pi_mock.sh")
	if err != nil {
		t.Fatalf("abs mock path: %v", err)
	}
	if _, err := os.Stat(mockPath); err != nil {
		t.Fatalf("mock script missing at %s: %v", mockPath, err)
	}

	// Make exec.LookPath("pi") resolve to the script. The full
	// round-trip test injects PATH too; we do the same here for
	// parity even though we pass an absolute command path.
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) })
	os.Setenv("PATH", filepath.Dir(mockPath)+string(os.PathListSeparator)+origPath)

	// Point the mock at our control file so the test can flip
	// "abort-only" ↔ "all" mid-run. Pre-populate with "all" so
	// the bootstrap handshake (get_state) succeeds even before
	// the test sets the first explicit mode.
	t.Setenv("MOCK_PI_CONTROL_FILE", string(ctrl))
	ctrl.setMode(t, "all")

	a := NewStarter("pi", mockPath, nil)
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	sess, err := a.Start(context.Background(), agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain EventAgentReady so we know the bridge is fully up
	// before the test starts measuring anything.
	mustFirstEventOfKind(t, sess, agent.EventAgentReady, 3*time.Second)

	cleanup := func() {
		closeDone := make(chan error, 1)
		go func() { closeDone <- sess.Close() }()
		select {
		case <-closeDone:
		case <-time.After(3 * time.Second):
			t.Errorf("Close did not return within 3s after stop test")
		}
	}
	return sess, cleanup
}

// driverOf type-asserts sess.Driver() to the package-private
// *driver so the tests can inspect fields like turnActive and
// inFlightPromptID directly. The chat layer's IsReady is a
// derived signal that exercises more machinery than these
// regression tests need — checking the driver state is both
// faster and more focused on the actual contract under test.
func driverOf(t *testing.T, sess *agent.Agent) *driver {
	t.Helper()
	d, ok := sess.Driver().(*driver)
	if !ok {
		t.Fatalf("sess.Driver() type = %T, want *pi.driver", sess.Driver())
	}
	return d
}

// TestStop_FailsInFlightPromptRPC is the core fix-pi-stop
// regression: while a prompt RPC is hung on the wire, driver.Stop
// must close that pending channel so SendBlocks unblocks
// immediately rather than waiting promptTimeout (90s).
//
// Wire shape: control file set to "abort-only" makes the mock
// answer get_state + abort and silently drop every prompt. That's
// exactly the production-bug scenario: pi ACKs abort but never
// answers the abandoned prompt.
func TestStop_FailsInFlightPromptRPC(t *testing.T) {
	workspace := t.TempDir()
	ctrl := newMockControlFile(t)
	sess, cleanup := startMockBridge(t, workspace, ctrl)
	defer cleanup()

	// Flip the mock to abort-only AFTER Start so the
	// handshake succeeds normally.
	ctrl.setMode(t, "abort-only")

	// Fire SendBlocks in a goroutine; it'll block on the prompt
	// RPC because the mock drops every prompt line.
	type sendResult struct {
		err error
		d   time.Duration
	}
	resultCh := make(chan sendResult, 1)
	go func() {
		start := time.Now()
		err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentText, Text: "hung prompt"},
		})
		resultCh <- sendResult{err: err, d: time.Since(start)}
	}()

	// Wait for SendBlocks to actually park on the prompt RPC.
	// Polling is more robust than a fixed sleep — the prompt id
	// is observable on the driver once turnMu is held.
	d := driverOf(t, sess)
	waitForTurnActive(t, d, 2*time.Second)

	// Stop. With the fix, the hung prompt RPC's channel is
	// closed and SendBlocks returns ErrSessionClosed within a
	// few hundred ms (not 90s).
	stopStart := time.Now()
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopElapsed := time.Since(stopStart)
	if stopElapsed > 2*time.Second {
		t.Errorf("Stop took %s; should return within handshakeTimeout once abort is acked", stopElapsed)
	}

	// SendBlocks must return promptly with an error — NOT
	// waiting the full promptTimeout. We assert < 2s as a
	// generous upper bound that still fails the test if
	// promptTimeout (90s) leaks through.
	select {
	case r := <-resultCh:
		if r.err == nil {
			t.Fatalf("SendBlocks returned nil after Stop; failResponse did not unblock it")
		}
		if r.d > 2*time.Second {
			t.Errorf("SendBlocks took %s after Stop; prompt RPC was not failed (want < 2s)", r.d)
		}
		t.Logf("SendBlocks unblocked in %s after Stop: %v", r.d, r.err)
		// ErrSessionClosed is the natural return value: the
		// pending channel closed without a response envelope.
		if !errors.Is(r.err, ErrSessionClosed) && !strings.Contains(r.err.Error(), "session closed") {
			t.Errorf("SendBlocks error %v does not look like a session-closed signal", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendBlocks did not return within 3s after Stop; failResponse did not unblock the prompt RPC")
	}

	// Driver state must be clean: turnActive=false and the
	// in-flight id + promptCancel cleared, otherwise the next
	// SendBlocks would ErrTurnBusy or hang forever. (d was
	// already bound from the pre-Stop poll.)
	d.turnMu.Lock()
	active := d.turnActive
	pendingID := d.inFlightPromptID
	cancelFn := d.promptCancel
	d.turnMu.Unlock()
	if active {
		t.Errorf("turnActive still true after Stop; next SendBlocks would ErrTurnBusy (regression)")
	}
	if pendingID != "" {
		t.Errorf("inFlightPromptID still %q after Stop; defer did not clear it", pendingID)
	}
	if cancelFn != nil {
		t.Errorf("promptCancel still non-nil after Stop; defer did not clear it")
	}
}

// TestStop_AllowsNewPromptAfterAbort verifies the user-visible
// recovery: after /stop, sending a fresh prompt lands a new turn
// (no ErrTurnBusy, no 90s promptTimeout wait).
//
// Wire shape: control file flipped to "abort-only" for the first
// half (prompt hangs, abort is acked), then flipped back to "all"
// so the mock answers the second prompt normally.
func TestStop_AllowsNewPromptAfterAbort(t *testing.T) {
	workspace := t.TempDir()
	ctrl := newMockControlFile(t)
	sess, cleanup := startMockBridge(t, workspace, ctrl)
	defer cleanup()

	ctrl.setMode(t, "abort-only")

	// First prompt hangs.
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- sess.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentText, Text: "hung first"},
		})
	}()
	// Wait for the prompt to park (more reliable than a fixed
	// sleep; the prompt id is visible once SendBlocks holds
	// turnMu).
	waitForTurnActive(t, driverOf(t, sess), 2*time.Second)

	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-firstErr:
		if err == nil {
			t.Fatalf("hung SendBlocks returned nil after Stop; failResponse did not unblock it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hung SendBlocks did not return within 3s after Stop")
	}

	// Flip the mock back to normal mode and verify a fresh
	// prompt produces a full event stream. If turnActive were
	// still true, SendBlocks would ErrTurnBusy here.
	ctrl.setMode(t, "all")

	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "second turn"},
	}); err != nil {
		t.Fatalf("second SendBlocks after Stop returned %v; turnActive not cleared (regression)", err)
	}

	// The new prompt should produce a full event stream ending
	// in EventAgentDone with Reason:"settled".
	got := drainEventsUntilDone(t, sess, 3*time.Second)
	if got.Done == nil || got.Done.Reason != "settled" {
		t.Fatalf("second turn: bad termination, events=%v", got.Kinds)
	}
}

// TestStop_PiSilentAfterAbort_AllowsNewPrompt covers the wire
// shape the production bug actually exhibits: pi ACKs the abort
// RPC but DOES NOT emit agent_settled. With the fix, the
// in-flight prompt RPC's failResponse is what unblocks the
// bridge — agent_settled is not required for recovery. This
// pins down the contract that failResponse alone is sufficient.
func TestStop_PiSilentAfterAbort_AllowsNewPrompt(t *testing.T) {
	t.Setenv("MOCK_PI_ABORT_NO_SETTLED", "1")

	workspace := t.TempDir()
	ctrl := newMockControlFile(t)
	sess, cleanup := startMockBridge(t, workspace, ctrl)
	defer cleanup()

	ctrl.setMode(t, "abort-only")

	// First prompt hangs; mock drops every prompt.
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- sess.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentText, Text: "hung first"},
		})
	}()
	waitForTurnActive(t, driverOf(t, sess), 2*time.Second)

	// Stop: mock answers abort (success) but emits NO
	// agent_settled. failResponse must still unblock the hung
	// prompt RPC.
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-firstErr:
		if err == nil {
			t.Fatalf("hung SendBlocks returned nil after Stop despite MOCK_PI_ABORT_NO_SETTLED; failResponse did not unblock it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hung SendBlocks did not return within 3s after Stop")
	}

	// Flip mock to normal mode and verify a fresh prompt
	// produces a full event stream. If turnActive were still
	// true, SendBlocks would ErrTurnBusy here.
	ctrl.setMode(t, "all")

	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "second turn after silent abort"},
	}); err != nil {
		t.Fatalf("second SendBlocks after silent-abort returned %v; turnActive not cleared", err)
	}
	got := drainEventsUntilDone(t, sess, 3*time.Second)
	if got.Done == nil || got.Done.Reason != "settled" {
		t.Fatalf("second turn after silent abort: bad termination, events=%v", got.Kinds)
	}
}

// ─── unit tests for the new primitives ──────────────────────────────

// TestStop_NoInFlightPrompt covers the pendingID="" branch of Stop.
// With no prompt parked in SendBlocks, Stop must not hang on the
// poll loop's 5ms re-snapshot window or on the abort RPC; it
// should return promptly after sending abort. This is the common
// case in production (user hits /stop between turns).
func TestStop_NoInFlightPrompt(t *testing.T) {
	workspace := t.TempDir()
	ctrl := newMockControlFile(t)
	sess, cleanup := startMockBridge(t, workspace, ctrl)
	defer cleanup()

	start := time.Now()
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop with no in-flight prompt returned %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("Stop with no in-flight prompt took %s; should be < 1s", elapsed)
	}

	// Bridge is still healthy — a follow-up prompt must
	// succeed normally.
	ctrl.setMode(t, "all")
	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "after no-op stop"},
	}); err != nil {
		t.Fatalf("SendBlocks after no-op Stop returned %v", err)
	}
	got := drainEventsUntilDone(t, sess, 3*time.Second)
	if got.Done == nil || got.Done.Reason != "settled" {
		t.Fatalf("post-no-op prompt: bad termination, events=%v", got.Kinds)
	}
}

// TestStop_ConcurrentDoubleTap covers a /stop double-tap (or two
// callers racing the same chat session). Both Stop calls snapshot
// the same in-flight id and BOTH register abort RPCs with id
// "abort-1". The second abort call would normally collide on the
// pending map (key already present from the first call) and block
// until handshakeTimeout. The fix tolerates this because the
// second failResponse on the prompt id is a no-op (id already
// failed), and the second abort RPC is harmless (pi sees two
// abort commands, second is a no-op).
//
// Wire shape: abort-only so the prompt hangs and the abort RPC
// returns success.
func TestStop_ConcurrentDoubleTap(t *testing.T) {
	workspace := t.TempDir()
	ctrl := newMockControlFile(t)
	sess, cleanup := startMockBridge(t, workspace, ctrl)
	defer cleanup()

	ctrl.setMode(t, "abort-only")

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- sess.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentText, Text: "hung first"},
		})
	}()
	waitForTurnActive(t, driverOf(t, sess), 2*time.Second)

	// Two Stop calls fired in parallel — neither should hang
	// past the 10s handshakeTimeout.
	type stopResult struct {
		err error
		d   time.Duration
	}
	results := make(chan stopResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			start := time.Now()
			err := sess.Stop(context.Background())
			results <- stopResult{err: err, d: time.Since(start)}
		}()
	}

	deadline := time.After(5 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("Stop #%d returned %v after %s", i, r.err, r.d)
			}
			if r.d > 3*time.Second {
				t.Errorf("Stop #%d took %s; concurrent Stops should not serialize", i, r.d)
			}
			t.Logf("Stop returned in %s", r.d)
		case <-deadline:
			t.Fatal("concurrent Stops did not both return within 5s")
		}
	}

	// SendBlocks must still unblock.
	select {
	case err := <-firstErr:
		if err == nil {
			t.Errorf("SendBlocks returned nil after double-Stop; failResponse did not unblock it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendBlocks did not unblock after double-Stop")
	}
}

// TestFailResponse_TargetedClose is the unit test for the new
// rpcClient.failResponse helper. failPending closes every pending
// channel; failResponse closes exactly the one matching id, so
// Stop can unblock SendBlocks without nuking the abort RPC's own
// pending slot (which it then needs to receive).
//
// We don't need a real driver here — only an rpcClient wired to a
// no-op stdin (so writes don't fail) is sufficient.
func TestFailResponse_TargetedClose(t *testing.T) {
	client := newRPCClient(nopCloser{Writer: io.Discard})

	// Register two pending RPCs with distinct ids.
	chA := client.expectResponse("req-A")
	chB := client.expectResponse("req-B")

	// Fail only A; B's channel must remain open.
	if !client.failResponse("req-A", ErrTurnAborted) {
		t.Fatalf("failResponse(req-A) returned false; want true")
	}

	select {
	case <-chA:
		// expected — A's channel closed
	case <-time.After(100 * time.Millisecond):
		t.Fatal("req-A channel did not close after failResponse")
	}

	// B must still be open — receiving from it would block.
	// Use a non-blocking probe so a buggy failResponse (which
	// accidentally closed B too) doesn't hang the test.
	select {
	case <-chB:
		t.Fatal("req-B channel closed; failResponse hit the wrong slot")
	default:
		// expected — B is still open
	}

	// Empty / unknown id is a clean no-op.
	if client.failResponse("", ErrTurnAborted) {
		t.Errorf("failResponse(\"\") returned true; want false")
	}
	if client.failResponse("req-NONE", ErrTurnAborted) {
		t.Errorf("failResponse on unknown id returned true; want false")
	}

	// A second failResponse on the same id is also a no-op
	// (the slot was deleted by the first call).
	if client.failResponse("req-A", ErrTurnAborted) {
		t.Errorf("failResponse on already-failed id returned true; want false")
	}
}

// TestSendBlocks_RecordsInFlightPromptID pins the driver-side
// change that lets Stop find the prompt RPC to fail. We exercise
// just enough of SendBlocks (the turnMu + rpc.request entry) to
// observe inFlightPromptID and turnActive without forking a real
// pi process. The End-to-end tests above already cover the
// recovery path; this test guards the invariant the recovery
// path depends on.
func TestSendBlocks_RecordsInFlightPromptID(t *testing.T) {
	// Construct a minimal driver. Only the fields that
	// SendBlocks touches need to be wired. rpc.stdinW goes
	// to a no-op writer so the prompt RPC's write doesn't
	// fail; the read side doesn't exist (we never call
	// readPump) so the request parks on the pending channel.
	d := &driver{
		rpc:      newRPCClient(nopCloser{Writer: io.Discard}),
		closed:   make(chan struct{}),
		exitDone: make(chan struct{}),
	}

	// Fire SendBlocks in a goroutine. It will park on the
	// prompt RPC because nothing dispatches a response.
	sendDone := make(chan error, 1)
	var sendWG sync.WaitGroup
	sendWG.Go(func() {
		sendDone <- d.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentText, Text: "anything"},
		})
	})

	// Poll for the id field to be set. SendBlocks sets
	// inFlightPromptID synchronously under turnMu BEFORE
	// entering the rpc.request wait, so 50ms is plenty.
	deadline := time.Now().Add(2 * time.Second)
	var id string
	var active bool
	for time.Now().Before(deadline) {
		d.turnMu.Lock()
		id = d.inFlightPromptID
		active = d.turnActive
		d.turnMu.Unlock()
		if id != "" && active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if id == "" {
		t.Fatalf("inFlightPromptID is empty while SendBlocks is parked on prompt RPC")
	}
	if !active {
		t.Fatalf("turnActive is false while SendBlocks is parked on prompt RPC")
	}
	if !strings.HasPrefix(id, "req-") {
		t.Errorf("inFlightPromptID = %q, want req-* (matches nextRequestID format)", id)
	}

	// Unblock SendBlocks by closing its pending channel
	// (simulates Stop's failResponse path).
	if !d.rpc.failResponse(id, ErrTurnAborted) {
		t.Fatalf("failResponse(%q) returned false; id was not in pending map", id)
	}

	sendWG.Wait()
	err := <-sendDone
	if err == nil {
		t.Errorf("SendBlocks returned nil after failResponse; want ErrSessionClosed")
	}

	// After SendBlocks returns, the defer must have cleared
	// both flags atomically.
	d.turnMu.Lock()
	if d.turnActive {
		t.Errorf("turnActive still true after SendBlocks returned")
	}
	if d.inFlightPromptID != "" {
		t.Errorf("inFlightPromptID still %q after SendBlocks returned; defer did not clear it", d.inFlightPromptID)
	}
	d.turnMu.Unlock()

	// And the prompt id must be re-usable for a follow-up
	// turn (Stop's whole point).
	sendDone2 := make(chan error, 1)
	go func() {
		sendDone2 <- d.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentText, Text: "second"},
		})
	}()
	deadline2 := time.Now().Add(2 * time.Second)
	var id2 string
	for time.Now().Before(deadline2) {
		d.turnMu.Lock()
		id2 = d.inFlightPromptID
		d.turnMu.Unlock()
		if id2 != "" && id2 != id {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id2 == "" || id2 == id {
		t.Errorf("follow-up SendBlocks did not register a fresh prompt id (id=%q, id2=%q)", id, id2)
	}
	// Unblock the second one too so the goroutine exits.
	d.rpc.failResponse(id2, ErrSessionClosed)
}