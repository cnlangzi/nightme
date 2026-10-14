package syswake

import (
	"context"
	"log/slog"
	"runtime"
	"testing"
)

// TestNew_Construction exercises the public surface without
// requiring any real wake event. On dev machines / CI where the
// OS API prerequisites are met (logind on linux, the notify fd
// subsystem on darwin, an interactive desktop on windows), New
// returns a real Detector; on Session 0 services or in
// containers without logind, New returns a no-op detector. Both
// outcomes are valid — the test asserts the contract, not the
// specific implementation.
func TestNew_Construction(t *testing.T) {
	det, err := New(context.Background(), Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if det == nil {
		t.Fatal("New returned nil detector")
	}
	if ch := det.WakeChan(); ch == nil {
		t.Error("WakeChan returned nil")
	}
}

func TestNew_NilLogger(t *testing.T) {
	// nil Logger must not panic — syswake.New defaults to
	// slog.Default() internally.
	det, err := New(context.Background(), Config{Logger: nil})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if det == nil {
		t.Fatal("New returned nil detector")
	}
	defer det.Close()
}

func TestNew_NilCtx(t *testing.T) {
	// nil ctx must not panic — syswake.New defaults to
	// context.Background() internally.
	det, err := New(nil, Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if det == nil {
		t.Fatal("New returned nil detector")
	}
	defer det.Close()
}

func TestClose_Idempotent(t *testing.T) {
	det, err := New(context.Background(), Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := det.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := det.Close(); err != nil {
		t.Fatalf("second Close should be no-op: %v", err)
	}
}

func TestClose_NoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	det, err := New(context.Background(), Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := det.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Give any cleanup goroutine a chance to finish.
	for i := 0; i < 20; i++ {
		runtime.Gosched()
	}
	// Allow modest drift — runtime bookkeeping goroutines and
	// parallel test runners may add a few. We expect the new
	// detector's pump goroutine to be gone, so delta should be
	// small. Compare via diff, not equality.
	after := runtime.NumGoroutine()
	if after-before > 4 {
		t.Errorf("goroutine leak suspected: before=%d after=%d", before, after)
	}
}

// TestFake_Inject exercises the test-only fake detector that
// downstream tests use to deterministically fire wake events.
func TestFake_Inject(t *testing.T) {
	fd := NewFake()
	if fd.WakeChan() == nil {
		t.Fatal("WakeChan nil")
	}
	if err := fd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Inject after Close must not panic.
	fd.Inject()
}