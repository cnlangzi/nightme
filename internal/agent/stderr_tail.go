package agent

import "fmt"

// WithStderrTail appends the child's stderr tail to err's text so
// the reason a bridge child died is visible where the chat layer
// renders the error.
//
// The start / handshake path has no event channel: the bridge
// returns an error and the chat shows "Failed to spawn agent: ...".
// Without the tail the operator only sees "session closed" and
// never learns that the child wrote the real reason to stderr.
//
// The tail is truncated to 1024 bytes — the same budget the
// bridges already use for the tail they attach to a lifecycle
// EventAgentError. The original error stays in the wrap chain, so
// errors.Is still matches the sentinels the chat layer switches on
// (ErrSessionClosed, ErrResumeUnhealthy, ...).
//
// An empty tail leaves err untouched: a child that wrote nothing to
// stderr gets no tail section at all.
func WithStderrTail(err error, tail string) error {
	if err == nil || tail == "" {
		return err
	}
	return fmt.Errorf("%w\n--- stderr tail ---\n%s", err, TruncateForLog(tail, 1024))
}
