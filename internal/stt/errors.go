// Package stt is the in-process client / transport / manager /
// installer surface NightMe uses to talk to a separate `nightme-stt`
// worker over a local IPC endpoint.
//
// The split is intentional: the heavy native dependencies
// (sherpa-onnx, ONNX Runtime, model files) live in the
// `nightme-stt` binary; NightMe core stays pure-Go and pays no
// build-time or run-time cost for users who never send a Telegram
// Voice message. See docs/channel/telegram.md §21 for the full
// architectural rationale.
//
// Issue #381 contract (relevant to this package):
//
//   - HTTPS only (release.yml / installer.go enforce this on the
//     download path; the live IPC transport is local-only).
//   - SHA-256 verified before activation.
//   - Lazy model + worker load: nothing starts until the first Voice
//     message arrives.
//   - One transcription at a time per worker (V1 single-utterance).
package stt

import (
	"errors"
)

// Error is the typed error returned by the STT client and surfaced
// by the worker. Code is one of the Code* constants and is the
// stable contract callers branch on; Message is the human-readable
// diagnostic and may change across worker versions.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e == nil {
		return "stt: <nil>"
	}
	if e.Message == "" {
		return "stt: " + e.Code
	}
	return "stt: " + e.Code + ": " + e.Message
}

// Error code constants. The set is closed: adding a new code is a
// protocol change (issue #381 §2). Callers branch on these values
// to render user-visible messages and structured logs.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeUnsupportedFormat   = "unsupported_format"
	CodeNotReady            = "not_ready"
	CodeTranscriptionFailed = "transcription_failed"
	CodeTimeout             = "timeout"
	CodeInternal            = "internal_error"
	CodeProtocolMismatch    = "protocol_mismatch"
	CodeOversizedRequest    = "oversized_request"
)

// NewError builds an *Error with the given code + message.
func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// IsCode reports whether err is an *Error with the given code.
// Safe against nil.
func IsCode(err error, code string) bool {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code == code
	}
	return false
}

// AsError extracts a typed *Error from err, if present.
func AsError(err error) (*Error, bool) {
	return errors.AsType[*Error](err)
}
