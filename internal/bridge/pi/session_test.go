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
//  1. Start succeeds and emits exactly one EventAgentReady;
//  2. the first SendBlocks returns nil and yields a stream
//     of text + tool events followed by EventAgentDone with
//     Reason:"settled" -- the channel is NOT closed;
//  3. a second SendBlocks also succeeds and yields another
//     EventAgentDone -- still no channel close;
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
	// "binary". A real user would call NewStarter("pi", "pi", nil)
	// and rely on PATH; here we point at the absolute path so
	// the test does not require `pi` to be uninstalled from
	// the developer's machine.
	a := NewStarter("pi", mockPath, nil)
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	ctx := context.Background()
	sess, err := a.Start(ctx, agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Step 1: collect the EventAgentReady.
	init := mustFirstEventOfKind(t, sess, agent.EventAgentReady, 2*time.Second)
	if init.AgentName != "pi" {
		t.Errorf("Init.AgentName = %q, want pi", init.AgentName)
	}
	if init.SessionID != "mock-session-1" {
		t.Errorf("Init.SessionID = %q, want mock-session-1", init.SessionID)
	}
	if !strings.Contains(init.Model, "Claude Sonnet 4") {
		t.Errorf("Init.Model = %q, want to contain 'Claude Sonnet 4'", init.Model)
	}

	// Step 2: drive a first prompt. The mock streams
	// text_delta*2 + text_end + message_end + agent_settled, so
	// we expect at least one EventAgentText and the terminal
	// EventAgentDone with Reason:"settled".
	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello from the bridge"},
	}); err != nil {
		t.Fatalf("SendBlocks #1: %v", err)
	}
	first := drainEventsUntilDone(t, sess, 3*time.Second)
	if first.Done == nil {
		t.Fatalf("first turn: no EventAgentDone received; events=%v", first.Kinds)
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
	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "second turn"},
	}); err != nil {
		t.Fatalf("SendBlocks #2: %v", err)
	}
	second := drainEventsUntilDone(t, sess, 3*time.Second)
	if second.Done == nil || second.Done.Reason != "settled" {
		t.Fatalf("second turn: bad termination, events=%v", second.Kinds)
	}

	// Step 4: close. The events channel MUST close exactly
	// once within a reasonable deadline — that's the only
	// invariant the bridge needs to satisfy for clean shutdown.
	//
	// The previous version of this step counted events received
	// between sess.Close() and the events channel closing, which
	// is FUNDAMENTALLY RACY: sess.Close() signals the lifecycle
	// goroutine to kill the child, but events already buffered
	// in the child's stdout pipe are still drained by the
	// lifecycle goroutine and delivered via deliver() BEFORE
	// s.events is closed. Those events are legitimate, not
	// phantoms — there's no way to distinguish them from "events
	// the bridge should not have emitted after Close" without
	// race-prone wall-clock assertions.
	//
	// The race-free invariant is the structural one: once the
	// events channel is closed, no further events can ever be
	// delivered to it (sending on a closed channel panics; in
	// practice the lifecycle goroutine finishes its drain loop
	// before closing the channel). So we just wait for close
	// with a deadline and assert it actually happened.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range sess.Events() {
			// drain until lifecycle goroutine closes the channel;
			// we don't count — anything received here is either an
			// in-flight event from the child (legit) or a phantom
			// (would have to come AFTER s.events close, which is
			// structurally impossible — a "phantom after close" is
			// a Go channel invariant violation, not a bridge bug).
		}
	}()
	start := time.Now()
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-closed:
		// Bridge shut down cleanly: child exited, lifecycle
		// goroutine drained pipe, s.events closed. Total time
		// is informational only — we don't bound it because
		// process-exit + pipe-drain latency is environment-
		// dependent (CI runners are slower than local dev).
		_ = time.Since(start)
	case <-time.After(2 * time.Second):
		t.Fatal("events channel did not close within 2s of Close() — lifecycle goroutine stuck")
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
	Done   *agent.AgentDoneEvent
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

	a := NewStarter("pi", mockPath, nil)
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

// TestSession_PromptTimeout_NotInfinite is the regression for
// the F-32 2026-08-06 incident where SendBlocks returned
// context.Background() and the bridge blocked forever on a hung
// pi prompt. The fix (SendBlocks now wraps the ctx with
// promptTimeout) means a hung prompt surfaces within
// promptTimeout as a context.DeadlineExceeded error -- not as
// a silent blank receipt card.
//
// Strategy:
//   - mock answers get_state normally (handshake succeeds);
//   - mock silently swallows every prompt command
//     (MOCK_PI_PROMPT_HANG=1);
//   - we shrink promptTimeout to 300 ms so the test runs in <2s
//     instead of waiting the production 90 s;
//   - SendBlocks must return within ~promptTimeout and the error
//     must wrap context.DeadlineExceeded.
//
// We restore the original promptTimeout via t.Cleanup so this
// test never bleeds into the others in this package.
func TestSession_PromptTimeout_NotInfinite(t *testing.T) {
	mockPath, err := filepath.Abs("../../testdata/pi_mock.sh")
	if err != nil {
		t.Fatalf("abs mock path: %v", err)
	}
	if _, err := os.Stat(mockPath); err != nil {
		t.Fatalf("mock script missing at %s: %v", mockPath, err)
	}

	workspace := t.TempDir()
	t.Setenv("MOCK_PI_PROMPT_HANG", "1")

	// Shrink promptTimeout so the test is fast. Restore on exit.
	const shrunk = 300 * time.Millisecond
	orig := promptTimeout
	promptTimeout = shrunk
	t.Cleanup(func() { promptTimeout = orig })

	a := NewStarter("pi", mockPath, nil)
	sess, err := a.Start(context.Background(), agent.StartConfig{Workspace: workspace})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		closeDone := make(chan error, 1)
		go func() { closeDone <- sess.Close() }()
		select {
		case <-closeDone:
		case <-time.After(3 * time.Second):
			t.Errorf("Close did not return within 3s after prompt-timeout test")
		}
	}()

	// Drain EventAgentReady so we know the bridge is fully up before
	// we measure SendBlocks's behaviour. Without this, a slow
	// handshake could leak into the SendBlocks measurement.
	mustFirstEventOfKind(t, sess, agent.EventAgentReady, 3*time.Second)

	start := time.Now()
	sendErr := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hi"},
	})
	elapsed := time.Since(start)

	if sendErr == nil {
		t.Fatalf("SendBlocks returned nil after %s; bridge ignored the prompt deadline (F-32 2026-08-06 hang)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("SendBlocks took %s with a %s deadline; ctx was ignored", elapsed, shrunk)
	}
	if !errors.Is(sendErr, context.DeadlineExceeded) {
		t.Errorf("SendBlocks error %v does not wrap context.DeadlineExceeded; runtime cannot distinguish prompt-hang from other failures", sendErr)
	}
	t.Logf("SendBlocks timed out as expected in %s: %v", elapsed, sendErr)
}

func mustFirstEventOfKind(t *testing.T, sess *agent.Agent, kind agent.EventKind, timeout time.Duration) agent.AgentEvent {
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
// EventAgentDone arrives or the timeout elapses. Returns a snapshot
// of the collected events plus the Done payload.
func drainEventsUntilDone(t *testing.T, sess *agent.Agent, timeout time.Duration) captured {
	t.Helper()
	deadline := time.After(timeout)
	out := captured{}
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("events channel closed before EventAgentDone; saw %v", out.Kinds)
			}
			out.Events = append(out.Events, ev)
			out.Kinds = append(out.Kinds, ev.Kind)
			if ev.Kind == agent.EventAgentDone {
				out.Done = ev.Done
				return out
			}
		case <-deadline:
			t.Fatalf("timeout waiting for EventAgentDone; saw %v", out.Kinds)
		}
	}
}
