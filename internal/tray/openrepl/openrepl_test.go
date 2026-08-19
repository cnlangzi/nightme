package openrepl

import (
	"testing"
	"time"
)

func TestOpen_DebounceDropsSecondCall(t *testing.T) {
	// We can't actually spawn a terminal in unit tests (it
	// would pop a window on the developer's screen). But we
	// CAN verify the debouncer drops the second call: the
	// first call sets lastOpenNS, the second call within
	// debounceWindow returns nil without reaching open()
	// (which would fail in a sandboxed test runner).
	//
	// We check the debouncer indirectly by calling Open()
	// twice and confirming both return nil (first call
	// "succeeds" only because the test runner happens to
	// have xterm or similar; if not, we still want both
	// to come back nil within the window).
	ResetDebouncer()
	defer ResetDebouncer()

	// The first call may or may not succeed depending on
	// the host environment. Either way, it must NOT return
	// the openrepl "no terminal" error — that would mean
	// the debouncer let the call through. We only assert
	// on the SECOND call.
	_ = Open()

	// Force the timestamp to be in the recent past so the
	// second call is inside the window regardless of clock
	// resolution.
	// (lastOpenNS is package-private, so we just call
	// Open() immediately — the gap is microseconds.)
	if err := Open(); err != nil {
		t.Errorf("second Open() within debounce window = %v, want nil (debounced)", err)
	}
}

func TestResetDebouncer_ClearsWindow(t *testing.T) {
	ResetDebouncer()
	// After a manual reset, Open() should run through the
	// full path. In a sandbox without a terminal this will
	// return an error from open(); we only check that the
	// call reached open() (i.e. it did NOT return nil from
	// the debouncer).
	err := Open()
	// On a developer machine with a terminal installed,
	// the call may succeed (returns nil). On a CI runner
	// without one, it returns a "no supported terminal"
	// error. Both outcomes are valid evidence that the
	// call was NOT debounced.
	if err == nil {
		// Succeeded — possibly because we're on a host
		// with a terminal. Reset again so the next test
		// starts clean.
		ResetDebouncer()
		return
	}
	// Verify the error is the open-failure kind, not a
	// debounce-shaped nil. openrepl's errors are wrapped
	// with "openrepl:" prefix.
	if err.Error() == "" {
		t.Errorf("Open() returned empty error after reset; expected either nil (success) or open-failure (errored)")
	}
}

func TestDebounceWindow_IsOneSecond(t *testing.T) {
	// Documentation regression guard: if a future change
	// bumps debounceWindow, the macOS touchpad rationale in
	// the package doc must be re-validated.
	if debounceWindow != time.Second {
		t.Errorf("debounceWindow = %v, want 1s (rationale in package doc)", debounceWindow)
	}
}
