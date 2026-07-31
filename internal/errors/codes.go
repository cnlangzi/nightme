// Package errors defines nightme's unified error code surface.
//
// Every command exits with a small, stable integer code defined
// here. The mapping lets shell scripts (CI, cron, systemd) branch
// on what went wrong without parsing free-form error strings.
//
// Convention:
//
//   - 0        success
//   - 1        generic / unmapped error (default fallback)
//   - 2..9     stable category codes
//
// Callers should:
//
//   - Wrap concrete failures in CodedError{Code: ..., Message: ..., Cause: err}
//     at the lowest layer that knows the category.
//   - Use ExitCode(err) at the top-level to derive the process
//     exit code (unknown errors fall back to CodeGenericError).
package errors

import (
	stderrors "errors"
	"fmt"
)

// Exit code constants. Keep the list narrow and stable — adding
// new codes mid-cycle would break consumers that pattern-match on
// these integers.
const (
	CodeSuccess         = 0
	CodeGenericError    = 1
	CodeConfigError     = 2
	CodeAuthError       = 3
	CodeChannelError    = 4
	CodeSessionError    = 5
	CodeAgentError      = 6
	CodeBridgeError     = 7
	CodeValidationError = 8
	CodeNotFound        = 9
)

// CodedError carries an exit code alongside a human message and an
// optional underlying cause. It satisfies the standard error
// interface and unwraps to its Cause so errors.Is / errors.As work
// as callers expect.
type CodedError struct {
	Code    int
	Message string
	Cause   error
}

// Error renders the message plus, when present, the cause. The
// code is intentionally omitted from the string — the exit code is
// metadata, not diagnostic text.
func (e *CodedError) Error() string {
	if e == nil {
		return "<nil coded error>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

// Unwrap returns the wrapped cause so errors.Is / errors.As can
// reach through a CodedError to the original failure.
func (e *CodedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// New constructs a CodedError with the given code and message.
// Use Wrap to attach an underlying cause.
func New(code int, message string) *CodedError {
	return &CodedError{Code: code, Message: message}
}

// Wrap attaches a cause to a code + message. A nil cause returns
// a plain CodedError (no wrap needed).
func Wrap(code int, message string, cause error) *CodedError {
	return &CodedError{Code: code, Message: message, Cause: cause}
}

// ExitCode walks err and any wrapped cause, returning the first
// CodedError's Code it encounters. A nil error yields
// CodeSuccess; a non-coded error falls back to CodeGenericError.
//
// Multiple nested CodedErrors return the OUTERMOST code — the
// one closest to the call site, which is what shell users expect.
func ExitCode(err error) int {
	if err == nil {
		return CodeSuccess
	}
	var coded *CodedError
	if stderrors.As(err, &coded) {
		return coded.Code
	}
	return CodeGenericError
}

// From is a convenience for one-call construction where the caller
// already has a code and cause. Returns nil when cause is nil so
// callers can use it inline without a guard.
func From(code int, cause error) error {
	if cause == nil {
		return nil
	}
	return Wrap(code, codeMessage(code), cause)
}

// codeMessage returns a stable human label for a code. The label
// is also used by From so callers don't need to invent their own
// wording. Unknown codes fall back to "error".
func codeMessage(code int) string {
	switch code {
	case CodeConfigError:
		return "config error"
	case CodeAuthError:
		return "auth error"
	case CodeChannelError:
		return "channel error"
	case CodeSessionError:
		return "session error"
	case CodeAgentError:
		return "agent error"
	case CodeBridgeError:
		return "bridge error"
	case CodeValidationError:
		return "validation error"
	case CodeNotFound:
		return "not found"
	default:
		return "error"
	}
}
