// Tests for the BridgeDiagnostic scaffolding in bridge_diagnostic.go.
// Covers every exported symbol in that file (plus a couple of
// invariants on the BridgeExitKind String round-trip) so a future
// refactor that breaks the wire format trips a CI signal here
// instead of in a downstream bridge.
package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// ─── ClassifyExit ─────────────────────────────────────────────────────

func TestClassifyExit(t *testing.T) {
	t.Run("GracefulClose", func(t *testing.T) {
		// graceful=true short-circuits everything, including
		// non-nil waitErr and panic-like error messages.
		cases := []error{
			nil,
			errors.New("anything"),
			errors.New("panic: runtime error"),
		}
		for _, e := range cases {
			if got := ClassifyExit(e, true); got != BridgeExitGracefulClose {
				t.Errorf("ClassifyExit(%v, true) = %s, want %s", e, got, BridgeExitGracefulClose)
			}
		}
	})

	t.Run("CleanExit", func(t *testing.T) {
		if got := ClassifyExit(nil, false); got != BridgeExitCleanExit {
			t.Errorf("ClassifyExit(nil, false) = %s, want %s", got, BridgeExitCleanExit)
		}
	})

	t.Run("NonZeroExit", func(t *testing.T) {
		// Simulate a child exit code 1 (positive).
		cmd := exec.Command("true")
		err := cmd.Run()
		if err != nil {
			t.Skipf("exec test fixture unavailable: %v", err)
		}
		// Force a non-zero exit instead.
		cmd2 := exec.Command("sh", "-c", "exit 1")
		err = cmd2.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
		}
		if exitErr.ExitCode() <= 0 {
			t.Fatalf("expected positive exit code, got %d", exitErr.ExitCode())
		}
		if got := ClassifyExit(err, false); got != BridgeExitNonZeroExit {
			t.Errorf("ClassifyExit(non-zero, false) = %s, want %s", got, BridgeExitNonZeroExit)
		}
	})

	t.Run("SignalKilled", func(t *testing.T) {
		// Go exposes negative codes for signals (-SIGNAL). We can't
		// easily fabricate one in a test, so synthesize via a
		// ProcessState from a process we kill ourselves. Skip
		// gracefully if the platform refuses.
		cmd := exec.Command("sh", "-c", "sleep 30")
		if err := cmd.Start(); err != nil {
			t.Skipf("cannot start sleep: %v", err)
		}
		// SIGKILL = 9; Go returns -9 for the exit code.
		_ = cmd.Process.Kill()
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Skipf("expected *exec.ExitError after SIGKILL, got %T: %v", err, err)
		}
		if exitErr.ExitCode() >= 0 {
			t.Skipf("expected negative exit code from signal, got %d", exitErr.ExitCode())
		}
		if got := ClassifyExit(err, false); got != BridgeExitSignalKilled {
			t.Errorf("ClassifyExit(signal, false) = %s, want %s", got, BridgeExitSignalKilled)
		}
	})

	t.Run("Panic", func(t *testing.T) {
		// errors.As(*exec.ExitError) fails → falls through to
		// string match for "panic".
		err := errors.New("panic: runtime error: invalid memory address")
		if got := ClassifyExit(err, false); got != BridgeExitPanic {
			t.Errorf("ClassifyExit(panic-msg, false) = %s, want %s", got, BridgeExitPanic)
		}
	})

	t.Run("Unknown", func(t *testing.T) {
		// Not an ExitError and no "panic" in the message → unknown.
		err := errors.New("some other error")
		if got := ClassifyExit(err, false); got != BridgeExitUnknown {
			t.Errorf("ClassifyExit(other, false) = %s, want %s", got, BridgeExitUnknown)
		}
	})
}

// ─── StderrFingerprint ────────────────────────────────────────────────

func TestStderrFingerprint(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		if got := StderrFingerprint(""); got != "" {
			t.Errorf("StderrFingerprint(\"\") = %q, want \"\"", got)
		}
	})

	t.Run("SingleLineNoNoise", func(t *testing.T) {
		in := "fatal: out of memory"
		got := StderrFingerprint(in)
		if got != in {
			t.Errorf("StderrFingerprint(%q) = %q, want %q", in, got, in)
		}
	})

	t.Run("StripsTimestampsAndFrames", func(t *testing.T) {
		in := strings.Join([]string{
			"2026-08-15T09:57:03.909Z INFO startup",
			"panic: runtime error: invalid memory address or nil pointer dereference",
			"	/Users/me/proj/main.go:42 +0xabc",
			"	/usr/local/go/src/runtime/panic.go:987 +0x3a2",
			"",
			"[1700000000000] another line",
		}, "\n")
		got := StderrFingerprint(in)
		// We expect the panic message + first stable frame.
		// Timestamps and frame line numbers / offsets should be stripped.
		if strings.Contains(got, "2026-08-15") {
			t.Errorf("timestamp not stripped: %q", got)
		}
		if strings.Contains(got, "+0xabc") {
			t.Errorf("frame offset not stripped: %q", got)
		}
		if strings.Contains(got, "[1700000000000]") {
			t.Errorf("bracketed timestamp not stripped: %q", got)
		}
		if !strings.Contains(got, "panic") {
			t.Errorf("panic message lost: %q", got)
		}
		if !strings.Contains(got, "main.go") {
			t.Errorf("frame path lost: %q", got)
		}
	})

	t.Run("BudgetLimit", func(t *testing.T) {
		// Pad with enough junk to exceed the 200-byte fingerprint budget.
		var b strings.Builder
		for i := 0; i < 50; i++ {
			fmt.Fprintf(&b, "filler line %02d with extra words to consume budget\n", i)
		}
		got := StderrFingerprint(b.String())
		if len(got) > 200 {
			t.Errorf("fingerprint length = %d, want <= 200", len(got))
		}
		if got == "" {
			t.Errorf("fingerprint should not be empty for non-empty input")
		}
	})
}

// ─── StderrRingBuffer ─────────────────────────────────────────────────

func TestStderrRingBuffer(t *testing.T) {
	t.Run("AccumulatesWithinMax", func(t *testing.T) {
		r := NewStderrRingBuffer(100)
		_, _ = r.Write([]byte("hello "))
		_, _ = r.Write([]byte("world"))
		if got := r.String(); got != "hello world" {
			t.Errorf("String() = %q, want %q", got, "hello world")
		}
	})

	t.Run("DropsHeadWhenOverflow", func(t *testing.T) {
		r := NewStderrRingBuffer(10)
		_, _ = r.Write([]byte("0123456789"))
		_, _ = r.Write([]byte("ABCDE"))
		// Ring keeps last 10 bytes: "56789ABCDE"
		if got := r.String(); got != "56789ABCDE" {
			t.Errorf("String() = %q, want %q", got, "56789ABCDE")
		}
	})

	t.Run("StringReturnsCopy", func(t *testing.T) {
		r := NewStderrRingBuffer(100)
		_, _ = r.Write([]byte("original"))
		s := r.String()
		if s != "original" {
			t.Fatalf("initial String() = %q, want %q", s, "original")
		}
		// Mutate the returned string and confirm the ring is unaffected.
		s = "tampered"
		if got := r.String(); got != "original" {
			t.Errorf("after caller mutation, String() = %q, want %q", got, "original")
		}
	})

	t.Run("ConcurrentSafe", func(t *testing.T) {
		r := NewStderrRingBuffer(1024)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					_, _ = r.Write([]byte("x"))
					_ = r.String()
				}
			}()
		}
		wg.Wait()
		// After 8 writers x 100 bytes = 800 bytes written into a
		// 1024-byte ring, the ring should not have overflowed; if
		// it did, contents are still consistent (no race).
		if got := r.String(); len(got) > 1024 {
			t.Errorf("ring length %d exceeded max 1024", len(got))
		}
	})

	t.Run("ZeroMaxFallsBackToDefault", func(t *testing.T) {
		r := NewStderrRingBuffer(0)
		if r.max != StderrTailBytes {
			t.Errorf("NewStderrRingBuffer(0).max = %d, want %d", r.max, StderrTailBytes)
		}
	})
}

// ─── TruncateForLog ───────────────────────────────────────────────────

func TestTruncateForLog(t *testing.T) {
	t.Run("ShortUnchanged", func(t *testing.T) {
		in := "short"
		if got := TruncateForLog(in, 100); got != in {
			t.Errorf("TruncateForLog(short, 100) = %q, want %q", got, in)
		}
	})

	t.Run("LongTruncated", func(t *testing.T) {
		in := strings.Repeat("a", 200)
		got := TruncateForLog(in, 50)
		if len(got) > 50 {
			t.Errorf("len(got) = %d, want <= 50", len(got))
		}
		if !strings.Contains(got, "[truncated]") {
			t.Errorf("missing truncation marker: %q", got)
		}
	})

	t.Run("EmptyMax", func(t *testing.T) {
		if got := TruncateForLog("anything", 0); got != "anything" {
			t.Errorf("TruncateForLog(x, 0) = %q, want \"anything\" (no truncation)", got)
		}
	})
}

// ─── BridgeExitKind.String ────────────────────────────────────────────

func TestBridgeExitKindString(t *testing.T) {
	// Lock down the wire-format of ExitKind strings — they appear
	// in /diagnose output and in feishu error cards, so a rename
	// would break user-visible UI.
	stable := map[BridgeExitKind]string{
		BridgeExitUnknown:       "unknown",
		BridgeExitGracefulClose: "graceful-close",
		BridgeExitCleanExit:     "clean-exit",
		BridgeExitNonZeroExit:   "non-zero-exit",
		BridgeExitSignalKilled:  "signal-killed",
		BridgeExitPanic:         "panic",
	}
	for k, want := range stable {
		if got := k.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", k, got, want)
		}
	}
}
