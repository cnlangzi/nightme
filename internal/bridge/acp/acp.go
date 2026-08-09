// Package acp implements the Agent Client Protocol client transport.
// ACP uses JSON-RPC 2.0 messages over a line-oriented stdio stream. The
// production session uses the PTY bridge as that stream's physical carrier.
package acp

import "io"

// Transport is the byte transport required by an ACP session. pty.Transport
// satisfies it, while tests can provide an in-memory or net.Pipe transport.
type Transport interface {
	io.ReadWriteCloser
	PID() int
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
