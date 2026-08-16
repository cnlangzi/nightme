// Package acp implements the Agent Client Protocol client transport.
// ACP uses JSON-RPC 2.0 messages over a line-oriented stdio stream. The
// production session uses the PTY bridge as that stream's physical carrier.
package acp

import (
	"errors"
	"io"
	"os"
)

// ErrTurnBusy is returned by SendBlocks when a previous session/prompt
// request is still in flight (i.e. the previous turn has not yet
// settled). Mirrors codex / pi / claudecode / opencode-bridge's
// ErrTurnBusy semantics: the caller may retry once the bridge's
// readPump observes the terminal event (EventAgentDone /
// EventAgentError) for the previous turn.
//
// Background: starting with F-OPENCODE-ACP-MIGRATION, the generic
// acp.SendBlocks awaits the session/prompt response. The response
// arrives only when the agent settles the turn, so a second
// SendBlocks call while the first is pending would otherwise queue
// a second prompt on the same session. ErrTurnBusy is the explicit
// "queue is full" rejection.
var ErrTurnBusy = errors.New("bridge/acp: previous turn still active")

// Transport is the byte transport required by an ACP session. pty.Transport
// satisfies it, while tests can provide an in-memory or net.Pipe transport.
type Transport interface {
	io.ReadWriteCloser
	PID() int
	// Signal sends a signal to the child process. Tests can
	// stub it as a no-op; production uses go-pty's underlying
	// *os.Process.Signal.
	Signal(os.Signal) error
}

const (
	protocolVersion = 1
	clientName      = "nightme"
	clientVersion   = "0.2.0"
)

// SessionOptions controls the ACP handshake without coupling the bridge to
// the agent registry.
type SessionOptions struct {
	Workspace string
}

// WithWorkspace supplies the cwd sent to session/new.
func WithWorkspace(workspace string) func(*SessionOptions) {
	return func(options *SessionOptions) { options.Workspace = workspace }
}
