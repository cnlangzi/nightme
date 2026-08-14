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
