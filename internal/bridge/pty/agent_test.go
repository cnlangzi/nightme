package pty

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
	a := NewAgent("claude", "claude", nil, nil)
	if got := a.Name(); got != "claude" {
		t.Fatalf("Name() = %q, want claude", got)
	}
}

// TestAgentMode verifies PTY agents report ModePTY so the
// SessionManager routes them through the PTY backend.
func TestAgentMode(t *testing.T) {
	a := NewAgent("claude", "claude", nil, nil)
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

	a := NewAgent("echo", "/bin/echo", nil, nil)
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect(/bin/echo) = %v", err)
	}

	if err := NewAgent("missing", "/no/such/pty-agent", nil, nil).Detect(); err == nil {
		t.Fatal("Detect(missing) = nil, want error")
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

	a := NewAgent("echo", "/bin/echo", nil, nil)
	sess, err := a.Start(context.Background(), agent.StartConfig{
		Workspace: t.TempDir(),
		Args:      []string{"hello"},
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

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
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if !gotHello {
		t.Errorf("did not observe 'hello' in events before deadline")
	}
}

// TestAgentStart_MissingBinary checks the Start path when the
// configured binary does not resolve.
func TestAgentStart_MissingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip unix-only smoke test on windows")
	}
	a := NewAgent("missing", "/no/such/binary", nil, nil)
	_, err := a.Start(context.Background(), agent.StartConfig{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("Start on missing binary returned nil error")
	}
	if !errors.Is(err, err) { // any non-nil satisfies this; document the surface.
		t.Fatalf("unexpected error type: %v", err)
	}
}

// contains is a tiny helper kept local so the test does not pull in
// strings for one call site.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}