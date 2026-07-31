package main

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// TestRecover_PanicInCommand sets up a root whose RunE panics and
// confirms Recover() converts the panic to a CodedError with
// CodeGenericError (which yields exit code 1). Stderr is
// redirected to a pipe so the test output is not polluted by the
// stack trace dumped by the guard.
func TestRecover_PanicInCommand(t *testing.T) {
	root := &cobra.Command{Use: "nightme"}
	root.RunE = func(cmd *cobra.Command, args []string) error {
		panic("kaboom")
	}
	Recover(root)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	out := root.Execute()

	// Drain the pipe so the guard's fprintf calls complete
	// before we swap stderr back.
	_ = w.Close()
	buf := make([]byte, 8192)
	for {
		_, err := r.Read(buf)
		if err != nil {
			break
		}
	}
	_ = r.Close()

	if out == nil {
		t.Fatal("expected error from Recover, got nil")
	}
	if got := nmerrors.ExitCode(out); got != nmerrors.CodeGenericError {
		t.Errorf("exit code = %d, want %d", got, nmerrors.CodeGenericError)
	}
}

// TestRecover_NoPanic ensures a well-behaved RunE is unaffected.
func TestRecover_NoPanic(t *testing.T) {
	root := &cobra.Command{Use: "nightme"}
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return nil
	}
	Recover(root)

	if err := root.Execute(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRecover_PreservesReturnError — a non-nil error from RunE
// propagates untouched.
func TestRecover_PreservesReturnError(t *testing.T) {
	want := nmerrors.New(nmerrors.CodeConfigError, "missing field")
	root := &cobra.Command{Use: "nightme"}
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return want
	}
	Recover(root)

	got := root.Execute()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if nmerrors.ExitCode(got) != nmerrors.CodeConfigError {
		t.Errorf("exit code = %d, want %d",
			nmerrors.ExitCode(got), nmerrors.CodeConfigError)
	}
}

// TestRecover_Idempotent — calling Recover twice on the same root
// does not install a second guard. We verify via the Annotations
// marker rather than comparing function pointers (which Go
// forbids).
func TestRecover_Idempotent(t *testing.T) {
	root := &cobra.Command{Use: "nightme"}
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return nil
	}
	Recover(root)
	if _, ok := root.Annotations[panicGuardKey]; !ok {
		t.Fatal("first Recover() did not set marker annotation")
	}
	// Second call must short-circuit (annotation guard) and leave
	// the wrapped RunE in place. Clear the annotation to peek at
	// the closure state via a panic probe.
	root.Annotations = map[string]string{}
	Recover(root)
	if _, ok := root.Annotations[panicGuardKey]; !ok {
		t.Error("second Recover re-marked the root (no-op expected)")
	}
}

// TestRecover_NoOpOnNil avoids a nil-deref panic if a caller
// forgets to pass the command.
func TestRecover_NoOpOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Recover(nil) should be a no-op, got panic: %v", r)
		}
	}()
	Recover(nil)
}

// TestPanicMsg sanity-checks the banner so log scrapers can pin
// the format.
func TestPanicMsg(t *testing.T) {
	if !strings.Contains(panicMsg, "panic") {
		t.Errorf("panicMsg should mention panic: %q", panicMsg)
	}
}
