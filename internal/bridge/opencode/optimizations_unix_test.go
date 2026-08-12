//go:build !windows

// Tests for the start-retry + watchdog + empty-response patches
// (Patches 1-3). Each test is intentionally narrow — the goal is
// to lock in the behaviour of the new helpers, not to re-exercise
// the broader bridge. End-to-end coverage lives in agent_e2e_test.go
// and session_real_test.go.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestIsUnrecoverableStartErr covers the classifier used by the
// Start retry wrapper. Auth / config / context-cancel errors must
// not retry; transient / unknown errors must retry.
func TestIsUnrecoverableStartErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is recoverable (no error)", nil, false},
		{"executable not found", errors.New("executable file not found in $PATH"), true},
		{"command not found", errors.New("command not found: opencode"), true},
		{"no such file", errors.New("open /no/such/path: no such file or directory"), true},
		{"context canceled", context.Canceled, true},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"network blip is recoverable", errors.New("dial tcp: connection refused"), false},
		{"unknown error is recoverable", errors.New("something weird happened"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUnrecoverableStartErr(c.err); got != c.want {
				t.Errorf("isUnrecoverableStartErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestWatchdogTimeout_DefaultAndOverride verifies the env override.
// We don't assert the exact default (10m) — that's a tuning knob —
// but we DO assert the override path. Locking the default value in
// a test would create churn every time someone tweaks turnWatchdogTimeout.
func TestWatchdogTimeout_DefaultAndOverride(t *testing.T) {
	t.Run("default (env unset) returns the package constant", func(t *testing.T) {
		t.Setenv("NIGHTME_OPENCODE_TURN_WATCHDOG", "")
		got := watchdogTimeout()
		if got != turnWatchdogTimeout {
			t.Errorf("watchdogTimeout() = %s, want %s", got, turnWatchdogTimeout)
		}
	})

	t.Run("env override parses", func(t *testing.T) {
		t.Setenv("NIGHTME_OPENCODE_TURN_WATCHDOG", "30s")
		if got, want := watchdogTimeout(), 30*time.Second; got != want {
			t.Errorf("watchdogTimeout() = %s, want %s", got, want)
		}
	})

	t.Run("garbage env falls back to default", func(t *testing.T) {
		t.Setenv("NIGHTME_OPENCODE_TURN_WATCHDOG", "not-a-duration")
		if got := watchdogTimeout(); got != turnWatchdogTimeout {
			t.Errorf("watchdogTimeout() = %s, want %s (fallback)", got, turnWatchdogTimeout)
		}
	})

	t.Run("zero disables the watchdog", func(t *testing.T) {
		t.Setenv("NIGHTME_OPENCODE_TURN_WATCHDOG", "0s")
		if got := watchdogTimeout(); got != 0 {
			t.Errorf("watchdogTimeout() = %s, want 0", got)
		}
	})
}

// TestWatchdog_FiresOnSilence asserts the watchdog kills the bridge
// after the configured timeout when no SSE events arrive. We
// override the timeout to 200ms via the env var so the test runs
// in well under a second.
func TestWatchdog_FiresOnSilence(t *testing.T) {
	t.Setenv("NIGHTME_OPENCODE_TURN_WATCHDOG", "200ms")

	a := &driver{
		name:        "opencode",
		events:      make(chan agent.AgentEvent, 64),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      &serverProc{baseURL: "http://127.0.0.1:1"},
		client:      newClient(&serverProc{baseURL: "http://127.0.0.1:1"}, "/tmp"),
		trans:       newTranslator(stubDeliver(), "opencode", "/tmp", "main", "ses_test", ""),
	}
	// Simulate an in-flight turn. lastEventAt is 1h ago so the
	// deadline check passes immediately.
	a.lastEventAtUnixNano.Store(time.Now().Add(-1 * time.Hour).UnixNano())
	a.pendingMu.Lock()
	a.pendingTurnActive = true
	a.pendingMu.Unlock()
	// Drive the Close goroutine so a.Close() returns within the test.
	go a.lifecycle()

	go a.watchdog()

	// Expect EventAgentError within ~1s. The watchdog ticks every
	// 10s, but the deadline check (time.Now().Before(deadline))
	// fires on the first tick because lastEventAt is ancient.
	// Note: deadline = now() + timeout when busy + activity is old.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	sawErr := false
	for !sawErr {
		select {
		case ev := <-a.events:
			if ev.Kind.String() == "error" {
				sawErr = true
				if ev.Err == nil || !strings.Contains(ev.Err.Error(), "watchdog") {
					t.Errorf("Err = %v, want watchdog message", ev.Err)
				}
			}
		case <-deadline.C:
			t.Fatal("watchdog did not fire within 2s")
		}
	}
}

// TestWatchdog_ExitsWhenTurnSettled asserts the watchdog goroutine
// exits promptly once the busy-guard drops (no need to kill the
// bridge for a turn that completed normally).
func TestWatchdog_ExitsWhenTurnSettled(t *testing.T) {
	t.Setenv("NIGHTME_OPENCODE_TURN_WATCHDOG", "100ms")

	a := &driver{
		name:        "opencode",
		events:      make(chan agent.AgentEvent, 64),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      &serverProc{baseURL: "http://127.0.0.1:1"},
		client:      newClient(&serverProc{baseURL: "http://127.0.0.1:1"}, "/tmp"),
	}
	// pendingTurnActive starts false → watchdog must exit on the
	// first 10s tick. We override the tick rate implicitly by
	// checking `a.closed` directly: spin up the watchdog, give it
	// 50ms, then close. Since the busy-guard is already false the
	// loop hits `if !busy { return }` immediately.
	done := make(chan struct{})
	go func() {
		a.watchdog()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog did not exit when no turn is pending")
	}
}

// TestWatchdog_ActivityResetsDeadline asserts the watchdog does NOT
// kill the bridge while a turn is pending AND recent activity
// (lastEventAt within timeout window) is being recorded. This is the
// happy-path regression guard — without this assertion the watchdog
// could spuriously fire on every long turn.
func TestWatchdog_ActivityResetsDeadline(t *testing.T) {
	// timeout=300ms, tick=30ms. We refresh lastEventAt every 50ms
	// for 200ms (4 ticks). Watchdog must NOT fire.
	t.Setenv("NIGHTME_OPENCODE_TURN_WATCHDOG", "300ms")

	a := &driver{
		name:        "opencode",
		events:      make(chan agent.AgentEvent, 64),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      &serverProc{baseURL: "http://127.0.0.1:1"},
		client:      newClient(&serverProc{baseURL: "http://127.0.0.1:1"}, "/tmp"),
	}
	a.pendingMu.Lock()
	a.pendingTurnActive = true
	a.pendingMu.Unlock()
	// recent activity — watchdog should treat this as alive
	a.lastEventAtUnixNano.Store(time.Now().UnixNano())

	// Refresh activity in the background.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				a.lastEventAtUnixNano.Store(time.Now().UnixNano())
			}
		}
	}()

	// Run watchdog for 200ms. It must NOT kill the bridge.
	done := make(chan struct{})
	go func() {
		a.watchdog()
		close(done)
	}()

	select {
	case <-done:
		close(stop)
		t.Fatal("watchdog returned prematurely despite active turn")
	case <-time.After(200 * time.Millisecond):
		close(stop)
		// Watchdog is still running (good — activity kept it alive).
		// Shut it down via a.closed.
		close(a.closed)
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("watchdog did not exit after Close()")
		}
	}

	// No EventAgentError should have been delivered.
	select {
	case ev := <-a.events:
		if ev.Kind == agent.EventAgentError {
			t.Errorf("unexpected error event during active turn: %v", ev.Err)
		}
	default:
	}
}

// TestScanBanner_HonorsTimeout is a regression guard for the
// scanner.Scan() blocking-vs-deadline bug we just fixed: the
// previous `select { default: }` style allowed a quiet stdout to
// hang indefinitely. We verify the timeout fires by handing the
// scanner a reader that produces nothing.
func TestScanBanner_HonorsTimeout(t *testing.T) {
	pr, pw := newBlockingPipe()
	// Defer cleanup order: close the writer first so the reader's
	// pending Scan returns EOF, then close the reader. Without
	// this the scanBanner goroutine leaks and Go runtime detects
	// a goroutine leak at test teardown.
	defer pw.Close()
	defer pr.Close()

	start := time.Now()
	url, err := scanBanner(pr, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("scanBanner returned err: %v", err)
	}
	if url != "" {
		t.Errorf("scanBanner returned url=%q, want empty (timeout path)", url)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("scanBanner returned in %s, want ~200ms (timeout not honored?)", elapsed)
	}
	if elapsed > 1*time.Second {
		t.Errorf("scanBanner took %s, want ~200ms (timeout way too loose)", elapsed)
	}
}

// newBlockingPipe returns a (reader, writer) pair where the reader
// blocks on Read until either data arrives or the writer closes.
// Used by TestScanBanner_HonorsTimeout.
func newBlockingPipe() (*blockingRead, *blockingWrite) {
	type pair struct {
		r *blockingRead
		w *blockingWrite
	}
	// Use a synchronous channel as the carrier; the reader blocks
	// on receive, the writer blocks on send. Both Close paths
	// select { default: } to avoid deadlocking the test.
	r := &blockingRead{ch: make(chan []byte, 1)}
	w := &blockingWrite{ch: r.ch}
	return r, w
}

type blockingRead struct{ ch chan []byte }

func (r *blockingRead) Read(p []byte) (int, error) {
	b, ok := <-r.ch
	if !ok {
		return 0, fmt.Errorf("closed")
	}
	n := copy(p, b)
	return n, nil
}
func (r *blockingRead) Close() error {
	defer func() { recover() }()
	close(r.ch)
	return nil
}

type blockingWrite struct{ ch chan []byte }

func (w *blockingWrite) Write(p []byte) (int, error) { return len(p), nil }
func (w *blockingWrite) Close() error                { return nil }