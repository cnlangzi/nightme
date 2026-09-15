package stt

import (
	"context"
	"fmt"
)

// Endpoint names the local IPC target. On Unix this is a Unix
// socket path; on Windows it is a named-pipe name (without the
// leading `\\.\pipe\` prefix). Constructed by the platform files
// (transport_unix.go / transport_windows.go) and surfaced through
// DefaultEndpoint below.
type Endpoint string

// Conn abstracts a single bidirectional local-IPC connection.
// ReadFrame / WriteFrame are the only required operations;
// Close cleans up the OS handle.
type Conn interface {
	ReadFrame(ctx context.Context, out any) error
	WriteFrame(ctx context.Context, in any) error
	Close() error
}

// Listener accepts inbound connections on a local endpoint.
// V1 processes one connection at a time per Listener (the
// manager serializes requests — issue #381 §2).
type Listener interface {
	Accept(ctx context.Context) (Conn, error)
	Close() error
	Endpoint() Endpoint
}

// Transport is the platform-specific factory for dialing and
// listening on the STT endpoint. Tests can substitute their own
// (e.g. an in-process pipe) without touching call sites.
type Transport interface {
	Dial(ctx context.Context, endpoint Endpoint) (Conn, error)
	Listen(ctx context.Context, endpoint Endpoint) (Listener, error)
}

// DefaultEndpoint returns the conventional per-user endpoint for
// the current OS. On Unix this is `<dataDir>/stt/stt.sock`; on
// Windows it is `\\.\pipe\nightme-stt`. dataDir is typically
// `~/.nightme` — without it, the transport has nowhere to put
// its socket file.
func DefaultEndpoint(dataDir string) (Endpoint, error) {
	if dataDir == "" {
		return "", fmt.Errorf("stt: empty data dir")
	}
	return defaultEndpoint(dataDir)
}
