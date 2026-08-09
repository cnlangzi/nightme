// Package acp implements the Agent Client Protocol client transport.
// ACP uses JSON-RPC 2.0 messages over a line-oriented stdio stream. The
// production session uses the PTY bridge as that stream's physical carrier.
package acp

import (
	"io"
	"os"
)

// Bridge is the byte transport required by an ACP session. pty.Bridge
// satisfies it, while tests can provide an in-memory or net.Pipe transport.
type Bridge interface {
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
