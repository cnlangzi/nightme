package errors

import (
	stderrors "errors"
	"fmt"
	"testing"
)

// TestExitCode_Nil confirms the success sentinel for the no-error
// case.
func TestExitCode_Nil(t *testing.T) {
	if got := ExitCode(nil); got != CodeSuccess {
		t.Errorf("ExitCode(nil) = %d, want %d", got, CodeSuccess)
	}
}

// TestExitCode_CodedError extracts the code from a top-level
// CodedError.
func TestExitCode_CodedError(t *testing.T) {
	err := New(CodeAuthError, "bad creds")
	if got := ExitCode(err); got != CodeAuthError {
		t.Errorf("got %d, want %d", got, CodeAuthError)
	}
}

// TestExitCode_WrappedCodedError ensures Unwrap carries the code
// through a fmt.Errorf("%w", ...) wrap.
func TestExitCode_WrappedCodedError(t *testing.T) {
	inner := Wrap(CodeSessionError, "boom", stderrors.New("io closed"))
	outer := fmt.Errorf("run: %w", inner)
	if got := ExitCode(outer); got != CodeSessionError {
		t.Errorf("got %d, want %d", got, CodeSessionError)
	}
}

// TestExitCode_GenericError falls back to CodeGenericError when
// the chain is uncoded.
func TestExitCode_GenericError(t *testing.T) {
	if got := ExitCode(stderrors.New("boom")); got != CodeGenericError {
		t.Errorf("got %d, want %d", got, CodeGenericError)
	}
}

// TestCodedError_String confirms the rendered form is Message
// alone (without the code) when no cause is attached.
func TestCodedError_String(t *testing.T) {
	err := New(CodeConfigError, "missing field")
	if got := err.Error(); got != "missing field" {
		t.Errorf("got %q, want %q", got, "missing field")
	}
}

// TestCodedError_WithCause renders Message + cause.
func TestCodedError_WithCause(t *testing.T) {
	cause := stderrors.New("disk full")
	err := Wrap(CodeBridgeError, "spawn failed", cause)
	got := err.Error()
	if got != "spawn failed: disk full" {
		t.Errorf("got %q", got)
	}
	if !stderrors.Is(err, cause) {
		t.Error("errors.Is should match wrapped cause")
	}
}

// TestFrom_NoCause returns nil so callers can inline From without
// an explicit guard.
func TestFrom_NoCause(t *testing.T) {
	if got := From(CodeNotFound, nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestFrom_PropagatesCode ensures From wraps with the correct code.
func TestFrom_PropagatesCode(t *testing.T) {
	err := From(CodeValidationError, stderrors.New("x"))
	if ExitCode(err) != CodeValidationError {
		t.Errorf("got %d", ExitCode(err))
	}
}
