//go:build !windows

package daemoncontrol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotRunning is defined cross-platform in protocol.go.

// osExit + the starting-state fast-path live in stop_fastpath.go
// so the unix and windows servers share one definition.

type Server struct {
	listener  *net.UnixListener
	path      string
	fileInfo  os.FileInfo
	status    Status
	state     atomic.Value
	stopOnce  sync.Once
	cancel    context.CancelFunc
	closeOnce sync.Once

	// healthSources holds one entry per started channel. The "health"
	// RPC collects them all, so every running channel is reported —
	// not just the last one registered.
	healthMu      sync.Mutex
	healthSources []HealthSource
}

// AddHealthSource registers one channel's health source. Defined
// cross-platform in server.go.

func Listen(path string, status Status, cancel context.CancelFunc) (*Server, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("daemon control: refusing symlink socket %s", path)
		}
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("daemon control: refusing non-socket path %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("daemon control: remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("daemon control: inspect socket: %w", err)
	}

	addr := &net.UnixAddr{Name: path, Net: "unix"}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("daemon control: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("daemon control: chmod socket: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon control: stat socket: %w", err)
	}
	status.ProtocolVersion = ProtocolVersion
	status.State = "starting"
	s := &Server{listener: ln, path: path, fileInfo: info, status: status, cancel: cancel}
	s.state.Store("starting")
	return s, nil
}

func (s *Server) SetReady() { s.state.Store("ready") }

func (s *Server) Status() Status {
	status := s.status
	status.State = s.state.Load().(string)
	status.UptimeSeconds = int64(time.Since(status.StartedAt).Seconds())
	return status
}

func (s *Server) Serve() error {
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("daemon control: accept: %w", err)
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req, err := ReadRequest(conn)
	if err != nil {
		_ = WriteError(conn, err)
		return
	}
	switch req.Command {
	case "ping":
		_ = WriteResult(conn, Ready{Ready: s.state.Load().(string) == "ready"})
	case "status":
		_ = WriteResult(conn, s.Status())
	case "health":
		_ = WriteResult(conn, s.healthPayload())
	case "stop":
		s.stopOnce.Do(func() {
			// CAS "starting" → "stopping". If the CAS succeeds we
			// are still in the window where SetReady() has NOT
			// fired — cancel() would be unconsumed by runDaemon's
			// wait select. The fast-path writes a stop ack, closes
			// the conn (Unix sockets don't flush send buffer on
			// exit), and calls osExit to release the daemon flock.
			// If SetReady() raced ahead, the CAS fails and we fall
			// through to the graceful cancel path below. See
			// stop_fastpath.go for the full rationale.
			if s.state.CompareAndSwap("starting", "stopping") {
				startingStateStopAck(conn)
			}
			s.state.Store("stopping")
			if s.cancel != nil {
				s.cancel()
			}
		})
		_ = WriteResult(conn, struct{}{})
	default:
		_ = WriteError(conn, fmt.Errorf("unknown command %q", req.Command))
	}
}

func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		closeErr = s.listener.Close()
		if info, err := os.Lstat(s.path); err == nil && os.SameFile(info, s.fileInfo) {
			if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) && closeErr == nil {
				closeErr = err
			}
		}
		// Fire cancel from the same Once as the "stop" RPC
		// handler so the daemon ctx is always cancellable —
		// regardless of which path initiated the shutdown.
		s.stopOnce.Do(func() {
			s.state.Store("stopping")
			if s.cancel != nil {
				s.cancel()
			}
		})
	})
	return closeErr
}
