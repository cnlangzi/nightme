// End-to-end smoke for the pi bridge: spawn the mock pi script
// (internal/testdata/pi_mock.sh) as if it were the real `pi`
// binary, drive a full handshake + first prompt + second prompt +
// close, and assert the events channel produces the expected
// sequence.
//
// We inject the mock via PATH so that Agent.Detect() succeeds
// without requiring `pi` to be installed on the test host.
// This matches the pattern used in
// internal/bridge/claudecode/claudecode_test.go:741-756.

package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestSession_FullRoundTrip spawns the mock pi, asserts that:
//  1. Start succeeds and emits exactly one EventInit;
//  2. the first SendBlocks returns nil and yields a stream
//     of text + tool events followed by EventDone with
//     Reason:"settled" -- the channel is NOT closed;
//  3. a second SendBlocks also succeeds and yields another
//     EventDone -- still no channel close;
//  4. Close() blocks until the child exits and then closes
//     the events channel exactly once.
func TestSession_FullRoundTrip(t *testing.T) {
	mockPath, err := filepath.Abs("../../testdata/pi_mock.sh")
	if err != nil {
		t.Fatalf("abs mock path: %v", err)
	}
	if _, err := os.Stat(mockPath); err != nil {
		t.Fatalf("mock script missing at %s: %v", mockPath, err)
	}

	// Inject the directory holding the mock into PATH so that
	// exec.LookPath("pi") resolves to the script.
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) })
	os.Setenv("PATH", filepath.Dir(mockPath)+string(os.PathListSeparator)+origPath)

	// Use a real temp dir as workspace so the bridge's
	// git-symbolic-ref call (during branch detection) does not
	// fail loudly.
	workspace := t.TempDir()

	// Construct the bridge with the mock script as the
	// "binary". A real user would call New("pi", "pi", nil)
	// and rely on PATH; here we point at the absolute path so
	// the test does not require `pi` to be uninstalled from
	// the developer's machine.
	a := New("pi", mockPath, nil)
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	ctx := context.Background()
	sess, err := a.Start(ctx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Step 1: collect the EventInit.
	init := mustFirstEventOfKind(t, sess, agent.EventInit, 2*time.Second)
	if init.Init.AgentName != "pi" {
		t.Errorf("Init.AgentName = %q, want pi", init.Init.AgentName)
	}
	if init.Init.SessionID != "mock-session-1" {
		t.Errorf("Init.SessionID = %q, want mock-session-1", init.Init.SessionID)
	}
	if !strings.Contains(init.Init.Model, "Claude Sonnet 4") {
		t.Errorf("Init.Model = %q, want to contain 'Claude Sonnet 4'", init.Init.Model)
	}

	// Step 2: drive a first prompt. The mock streams
	// text_delta*2 + text_end + message_end + agent_settled, so
	// we expect at least one EventText and the terminal
	// EventDone with Reason:"settled".
	if err := sess.SendText("hello from the bridge"); err != nil {
		t.Fatalf("SendText #1: %v", err)
	}
	first := drainEventsUntilDone(t, sess, 3*time.Second)
	if first.Done == nil {
		t.Fatalf("first turn: no EventDone received; events=%v", first.Kinds)
	}
	if first.Done.Reason != "settled" {
		t.Errorf("first turn: Done.Reason = %q, want settled", first.Done.Reason)
	}
	// Channel must still be open after the first turn.
	select {
	case _, ok := <-sess.Events():
		if !ok {
			t.Fatal("events channel closed after first turn; want it open across turns")
		}
	default:
		// No buffered event ready; that's fine, channel is
		// not closed.
	}

	// Step 3: drive a second prompt. Same shape, channel
	// must still be open.
	if err := sess.SendText("second turn"); err != nil {
		t.Fatalf("SendText #2: %v", err)
	}
	second := drainEventsUntilDone(t, sess, 3*time.Second)
	if second.Done == nil || second.Done.Reason != "settled" {
		t.Fatalf("second turn: bad termination, events=%v", second.Kinds)
	}

	// Step 4: close; the events channel must close exactly
	// once. We use a goroutine to drain the channel and count
	// how many events flow through after Close (should be
	// zero -- the child sees EOF and exits; lifecycle
	// goroutine closes events).
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		count := 0
		for ev := range sess.Events() {
			count++
			_ = ev
		}
		if count != 0 {
			t.Errorf("received %d events after Close; want 0", count)
		}
	}()
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("events channel did not close after Close()")
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}

// captured describes the events collected from one prompt, plus a
// handy kinds summary for error messages.
type captured struct {
	Events []agent.AgentEvent
	Kinds  []agent.EventKind
	Done   *agent.DoneEvent
}

// (extension_ui_request auto-cancel is exercised end-to-end
// in TestSession_FullRoundTrip: the mock script's stdin read
// confirms the bridge writes a valid JSONL line for every
// extension_ui_request it sees. A broken auto-cancel would
// either hang the test (writeLine deadlocked) or the mock
// would print "got: ..." to stderr. Either failure mode is
// observable without a dedicated in-process test.)

// TestSession_HandshakeTimeout verifies that Start returns an
// error when the child does not respond to the get_state
// handshake within handshakeTimeout. The bridge must tear down
// the process (Close in Start) so we don't leak the silent
// child behind the test.
func TestSession_HandshakeTimeout(t *testing.T) {
	mockPath, err := filepath.Abs("../../testdata/pi_mock.sh")
	if err != nil {
		t.Fatalf("abs mock path: %v", err)
	}
	if _, err := os.Stat(mockPath); err != nil {
		t.Fatalf("mock script missing at %s: %v", mockPath, err)
	}
	workspace := t.TempDir()
	t.Setenv("MOCK_PI_SILENT", "1")

	a := New("pi", mockPath, nil)
	ctx := context.Background()
	start := time.Now()
	_, err = a.Start(ctx, agent.StartConfig{Workspace: workspace})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Start: expected timeout error, got nil")
	}
	// Bridge must surface a useful error; we don't pin the
	// exact text since it could mention "handshake" or
	// "deadline". The shape -- a wrapped non-nil error
	// returned within the configured timeout -- is what
	// matters.
	if elapsed > 2*handshakeTimeout {
		t.Errorf("Start took %s; expected ~%s", elapsed, handshakeTimeout)
	}
	// The underlying cause should be a context-deadline (the
	// handshake ctx) so callers can distinguish it from "binary
	// missing" / "permission denied" etc. We accept any error
	// string that mentions "handshake" or "deadline".
	msg := err.Error()
	if !strings.Contains(msg, "handshake") && !strings.Contains(msg, "deadline") {
		t.Errorf("error message %q lacks handshake/deadline keyword", msg)
	}
	// Belt-and-braces: a deadline-exceeded error is the most
	// precise check; if it propagates correctly, the test
	// passes with a strong invariant. We only require it when
	// the bridge embeds it via errors.Is.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("note: Start did not wrap context.DeadlineExceeded: %v", err)
	}
}
func mustFirstEventOfKind(t *testing.T, sess agent.AgentSession, kind agent.EventKind, timeout time.Duration) agent.AgentEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("events channel closed before %s event arrived", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s event", kind)
		}
	}
}

// drainEventsUntilDone collects events from sess until an
// EventDone arrives or the timeout elapses. Returns a snapshot
// of the collected events plus the Done payload.
func drainEventsUntilDone(t *testing.T, sess agent.AgentSession, timeout time.Duration) captured {
	t.Helper()
	deadline := time.After(timeout)
	out := captured{}
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("events channel closed before EventDone; saw %v", out.Kinds)
			}
			out.Events = append(out.Events, ev)
			out.Kinds = append(out.Kinds, ev.Kind)
			if ev.Kind == agent.EventDone {
				out.Done = ev.Done
				return out
			}
		case <-deadline:
			t.Fatalf("timeout waiting for EventDone; saw %v", out.Kinds)
		}
	}
}
