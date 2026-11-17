package agent

import "fmt"

// StderrTailHeader introduces the stderr tail section of an error
// message. The bridges' lifecycle diagnostics use the same marker,
// so an operator reads a start failure and a mid-turn death the
// same way.
const StderrTailHeader = "--- stderr tail ---"

// WithStderrTail appends the child's stderr tail to err's text so
// the reason a bridge child died is visible where the chat layer
// renders the error.
//
// The start / handshake path has no event channel: the bridge
// returns an error and the chat shows "Failed to spawn agent: ...".
// Without the tail the operator only sees "session closed" and
// never learns that the child wrote the real reason to stderr.
//
// The tail is capped at 1024 bytes — the same budget the bridges
// already use for the tail they attach to a lifecycle
// EventAgentError. The cap drops the HEAD of the tail: a stderr
// tail is the child's most recent output, so the line that
// explains the exit sits at its end.
//
// The original error stays in the wrap chain, so errors.Is still
// matches the sentinels the chat layer switches on (ErrSessionClosed,
// ErrResumeUnhealthy, ...).
//
// An empty tail leaves err untouched: a child that wrote nothing to
// stderr gets no tail section at all.
func WithStderrTail(err error, tail string) error {
	if err == nil || tail == "" {
		return err
	}
	return fmt.Errorf("%w\n%s\n%s", err, StderrTailHeader, KeepTailForLog(tail, 1024))
}
