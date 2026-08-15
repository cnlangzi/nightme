// safego_test.go — pins the panic-isolation contract of SafeGo.
//
// The contract: a panic in the wrapped fn MUST NOT propagate up
// to crash the host goroutine (and therefore the nightme daemon).
// The host process survives; the panic is logged. The "name" tag
// is mandatory so multiple pumps in the same bridge are
// distinguishable in the recovery log.
//
// We don't assert on the log output here (the slog default handler
// would spam the test verbose output). We DO assert on the
// goroutine behavior: a done channel signals that fn() returned
// (cleanly or via panic-recovery), and a short sleep + process
// still alive proves the recovery worked.
package agent

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestSafeGo_RecoversFromPanic pins the headline contract: a
// panic in the wrapped fn does not crash the host. We trigger a
// panic inside SafeGo and then assert the process is still alive
// and well-behaved after a brief wait. If SafeGo failed to recover,
// `go test` would itself crash with the panic and the test would
// never reach the assertions below.
func TestSafeGo_RecoversFromPanic(t *testing.T) {
	const name = "test:panicker"
	done := make(chan struct{})

	SafeGo(name, func() {
		defer close(done)
		// Trivial panic — the runtime's recovery in SafeGo
		// must catch this; otherwise the panic propagates and
		// crashes the test binary.
		panic("intentional panic for safego test")
	})

	select {
	case <-done:
		// fn() returned — recovery worked.
	case <-time.After(2 * time.Second):
		t.Fatal("SafeGo did not return within 2s after panic; recovery failed")
	}
}

// TestSafeGo_NameIsPassedThrough is a soft check: the name we
// pass shows up in the slog attributes. We don't parse slog
// output (too brittle) but we verify that SafeGo accepts a name
// without panicking on a non-empty string.
func TestSafeGo_NameIsPassedThrough(t *testing.T) {
	done := make(chan struct{})
	SafeGo("bridge:role", func() {
		defer close(done)
		// no-op
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SafeGo did not return for a no-op fn")
	}
}

// TestSafeGo_NormalReturnWorks pins the boring case: a fn that
// returns normally must still be invoked, and the goroutine
// wrapper must not interfere.
func TestSafeGo_NormalReturnWorks(t *testing.T) {
	var called atomic.Bool
	done := make(chan struct{})
	SafeGo("test:normal", func() {
		defer close(done)
		called.Store(true)
	})
	select {
	case <-done:
		if !called.Load() {
			t.Fatal("SafeGo returned but fn body did not execute")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SafeGo did not return within 2s for a no-panic fn")
	}
}

// TestSafeGo_MultiplePanicsAreContained verifies the recovery
// works for repeated panics, not just the first one. This
// matters because long-lived pump goroutines may panic many
// times if a bad wire message keeps arriving — the recovery
// path must not get poisoned by a previous panic.
func TestSafeGo_MultiplePanicsAreContained(t *testing.T) {
	const n = 3
	done := make(chan struct{}, n)

	for range n {
		SafeGo("test:multi", func() {
			defer func() { done <- struct{}{} }()
			panic("boom")
		})
	}

	for i := range n {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("SafeGo did not recover panic #%d within 2s", i+1)
		}
	}
}

// ─── PanicEventHandler tests ──────────────────────────────────────────

// TestPanicEventHandler_NoPanicIsNoop pins the boring case: when
// the goroutine returns normally (no panic), PanicEventHandler
// does not invoke the deliver function. We pass an instrumented
// deliver that increments a counter; the counter MUST stay at 0.
func TestPanicEventHandler_NoPanicIsNoop(t *testing.T) {
	var called atomic.Int32
	deliver := func(ev AgentEvent) { called.Add(1) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer PanicEventHandler("test:no-panic", deliver,
			"sess", "agent", "/ws", "main")
		// no panic
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not return within 2s")
	}
	if got := called.Load(); got != 0 {
		t.Fatalf("deliver was called %d times for a no-panic case; want 0", got)
	}
}

// TestPanicEventHandler_DeliversErrorEventOnPanic pins the
// headline contract: a panic in the parent goroutine produces
// exactly one EventAgentError delivered via the deliver function,
// with the correct fields (Kind, SessionID, AgentName, Workspace,
// Branch, Err).
func TestPanicEventHandler_DeliversErrorEventOnPanic(t *testing.T) {
	got := make(chan AgentEvent, 1)
	deliver := func(ev AgentEvent) { got <- ev }
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer PanicEventHandler("test:panicker", deliver,
			"sess-1", "agent-1", "/ws-1", "branch-1")
		panic("intentional panic for panic-event test")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not return within 2s after panic")
	}
	select {
	case ev := <-got:
		if ev.Kind != EventAgentError {
			t.Fatalf("event Kind = %v, want EventAgentError", ev.Kind)
		}
		if ev.SessionID != "sess-1" {
			t.Fatalf("event SessionID = %q, want %q", ev.SessionID, "sess-1")
		}
		if ev.AgentName != "agent-1" {
			t.Fatalf("event AgentName = %q, want %q", ev.AgentName, "agent-1")
		}
		if ev.Workspace != "/ws-1" {
			t.Fatalf("event Workspace = %q, want %q", ev.Workspace, "/ws-1")
		}
		if ev.Branch != "branch-1" {
			t.Fatalf("event Branch = %q, want %q", ev.Branch, "branch-1")
		}
		if ev.Err == nil {
			t.Fatal("event Err is nil; want non-nil")
		}
		// Err should mention the panic name and the panic value.
		if msg := ev.Err.Error(); !contains(msg, "test:panicker") {
			t.Fatalf("event Err = %q, want it to mention the panic name", msg)
		}
		if msg := ev.Err.Error(); !contains(msg, "intentional panic for panic-event test") {
			t.Fatalf("event Err = %q, want it to mention the panic value", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deliver was not called within 2s after panic")
	}
}

// TestPanicEventHandler_PropagatesNonStringPanic pins a small
// edge case: the panic value need not be a string. recover()
// returns `any`; we should wrap it via fmt.Errorf("%v", r) which
// works for any value.
func TestPanicEventHandler_PropagatesNonStringPanic(t *testing.T) {
	got := make(chan AgentEvent, 1)
	deliver := func(ev AgentEvent) { got <- ev }
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer PanicEventHandler("test:err-panic", deliver, "", "", "", "")
		panic(errors.New("error-value panic"))
	}()

	<-done
	select {
	case ev := <-got:
		if ev.Err == nil {
			t.Fatal("event Err is nil; want non-nil")
		}
		if msg := ev.Err.Error(); !contains(msg, "error-value panic") {
			t.Fatalf("event Err = %q, want it to mention the wrapped error", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deliver was not called within 2s after panic")
	}
}

// contains is a tiny stdlib-free substring check used by the
// assertion strings above. strings.Contains would also work but
// this avoids pulling strings into the test file.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := range len(haystack) - len(needle) + 1 {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
