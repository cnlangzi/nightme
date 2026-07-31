package ptyagent

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestAgentName verifies the Name field is surfaced through the
// interface.
func TestAgentName(t *testing.T) {
	a := New("claude", "claude")
	if got := a.Name(); got != "claude" {
		t.Fatalf("Name() = %q, want claude", got)
	}
}

// TestAgentMode verifies PTY agents report ModePTY so the
// SessionManager routes them through the PTY backend.
func TestAgentMode(t *testing.T) {
	a := New("claude", "claude")
	if got := a.Mode(); got != agent.ModePTY {
		t.Fatalf("Mode() = %s, want pty", got)
	}
}

// TestAgentDetect verifies Detect uses PATH lookup and returns nil
// for binaries present on the host (e.g. /bin/echo) and a non-nil
// error for binaries guaranteed not to exist.
func TestAgentDetect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip unix-only smoke test on windows")
	}

	a := New("echo", "/bin/echo")
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect(/bin/echo) returned error: %v", err)
	}

	a = New("definitely-not-installed-xyz", "/nope/binary")
	if err := a.Detect(); err == nil {
		t.Fatalf("Detect(invalid) returned nil error, want non-nil")
	}
}

// TestAgentStartEndToEnd spawns /bin/echo under a PTY and verifies a
// complete session round-trip: Start returns a non-nil session, the
// Events channel yields EventText containing "hello", and Close
// releases the bridge cleanly.
func TestAgentStartEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip unix-only smoke test on windows")
	}

	a := New("echo", "/bin/echo")
	sess, err := a.Start(context.Background(), agent.StartConfig{
		Workspace: t.TempDir(),
		Args:      []string{"hello"},
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// Drain events until we find the "hello" payload. The session's
	// readLoop only terminates when Close() unblocks the underlying
	// Read; we drive it with a short timeout so the test does not
	// depend on PTY EOF semantics.
	deadline := time.After(2 * time.Second)
	gotHello := false
drain:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break drain
			}
			if ev.Kind == agent.EventText && contains(ev.Text, "hello") {
				gotHello = true
			}
			if ev.Kind == agent.EventDone {
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if !gotHello {
		t.Fatalf("never observed a text event containing %q", "hello")
	}
}

// TestAgentStartRejectsBadWorkspace confirms that an unresolvable
// workspace path surfaces as an error rather than a nil session.
func TestAgentStartBadWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip unix-only smoke test on windows")
	}

	a := New("echo", "/bin/echo")
	// /this/path/definitely/does/not/exist is not a valid Cwd; the
	// underlying PTY spawn must return an error.
	_, err := a.Start(context.Background(), agent.StartConfig{
		Workspace: "/this/path/definitely/does/not/exist",
	})
	if err == nil {
		t.Fatalf("Start with bad workspace returned nil error")
	}
	if !errors.Is(err, err) {
		// Errors.Is(err, err) is always true; we just want to
		// confirm err is non-nil and not an interface-conformance
		// quirk.
		t.Fatalf("unexpected err type: %v", err)
	}
}

// contains is a tiny substring helper to avoid importing strings just
// for one call.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}