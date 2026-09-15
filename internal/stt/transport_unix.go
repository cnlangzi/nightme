//go:build !windows

package stt

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// defaultEndpoint resolves the conventional Unix-socket endpoint
// under the user's data dir. The parent directory is created
// here so callers (manager + worker) don't have to agree on
// mkdir order.
func defaultEndpoint(dataDir string) (Endpoint, error) {
	dir := filepath.Join(dataDir, "stt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("stt: mkdir %s: %w", dir, err)
	}
	return Endpoint(filepath.Join(dir, "stt.sock")), nil
}

// unixTransport is the production Transport on Unix.
// Permission enforcement happens at socket-bind time (mode
// 0600) so the filesystem, not the worker, is the authority
// on who can dial in.
type unixTransport struct{}

func DefaultTransport() Transport { return unixTransport{} }

func (unixTransport) Dial(ctx context.Context, endpoint Endpoint) (Conn, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("stt: empty endpoint")
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", string(endpoint))
	if err != nil {
		return nil, fmt.Errorf("stt: dial: %w", err)
	}
	return &unixConn{Conn: conn}, nil
}

func (unixTransport) Listen(ctx context.Context, endpoint Endpoint) (Listener, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("stt: empty endpoint")
	}
	// Stale-endpoint probe: if a live worker owns the socket,
	// the dial succeeds — refuse to steal it. We only proceed
	// when the socket either doesn't exist or is dead.
	if _, err := os.Stat(string(endpoint)); err == nil {
		probe, perr := net.DialTimeout("unix", string(endpoint), 200*time.Millisecond)
		if perr == nil {
			_ = probe.Close()
			return nil, fmt.Errorf("stt: endpoint %s already owned by another worker", endpoint)
		}
		// Stale — remove and let the next listener create a
		// fresh socket.
		_ = os.Remove(string(endpoint))
	}
	addr, err := net.ResolveUnixAddr("unix", string(endpoint))
	if err != nil {
		return nil, fmt.Errorf("stt: resolve endpoint: %w", err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("stt: listen: %w", err)
	}
	// 0600 so only the current user can dial.
	if err := os.Chmod(string(endpoint), 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("stt: chmod endpoint: %w", err)
	}
	return &unixListener{ln: ln, endpoint: endpoint}, nil
}

type unixConn struct {
	net.Conn
}

func (c *unixConn) ReadFrame(ctx context.Context, out any) error {
	if dl, ok := ctxDeadline(ctx, 30*time.Second); ok {
		_ = c.Conn.SetDeadline(dl)
		defer c.Conn.SetDeadline(time.Time{})
	}
	if err := ReadFrame(c.Conn, out); err != nil {
		return fmt.Errorf("stt: read frame: %w", err)
	}
	return nil
}

func (c *unixConn) WriteFrame(ctx context.Context, in any) error {
	if dl, ok := ctxDeadline(ctx, 30*time.Second); ok {
		_ = c.Conn.SetDeadline(dl)
		defer c.Conn.SetDeadline(time.Time{})
	}
	if err := WriteFrame(c.Conn, in); err != nil {
		return fmt.Errorf("stt: write frame: %w", err)
	}
	return nil
}

func (c *unixConn) Close() error { return c.Conn.Close() }

type unixListener struct {
	ln       *net.UnixListener
	endpoint Endpoint
}

func (l *unixListener) Accept(ctx context.Context) (Conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = l.ln.SetDeadline(dl)
		defer l.ln.SetDeadline(time.Time{})
	}
	conn, err := l.ln.AcceptUnix()
	if err != nil {
		return nil, fmt.Errorf("stt: accept: %w", err)
	}
	return &unixConn{Conn: conn}, nil
}

func (l *unixListener) Close() error {
	err := l.ln.Close()
	_ = os.Remove(string(l.endpoint))
	return err
}

func (l *unixListener) Endpoint() Endpoint { return l.endpoint }
