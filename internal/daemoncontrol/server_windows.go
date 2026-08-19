//go:build windows

package daemoncontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Server mirrors server_unix.go's API on Windows. The cross-
// platform surface (HealthProvider, SetHealthProvider, SetReady,
// Status, Serve, Close) is identical; only the listener /
// accept / cleanup differs:
//
//   - Unix:    AF_UNIX socket file
//   - Windows: named pipe at the path returned by ResolvePaths
//
// Both versions duplicate the Server struct + cross-platform
// methods verbatim — Go allows this because the build tags
// ensure only one definition is compiled per platform.

// HealthProvider is defined in server.go (cross-platform).

// osExit + the starting-state fast-path live in stop_fastpath.go
// so the unix and windows servers share one definition.

type Server struct {
	pipeName string

	status    Status
	state     atomic.Value // "ready" / "stopping" / uninitialized
	stopOnce  sync.Once
	cancel    context.CancelFunc
	closeOnce sync.Once

	// closed is closed by Close() and watched by Serve so the
	// accept loop can exit between iterations. pending lists
	// in-flight pipe HANDLEs whose ConnectNamedPipe hasn't
	// completed yet; Close() CancelIoEx's each so the kernel
	// unblocks the goroutine immediately instead of waiting
	// for the process to exit.
	closed  chan struct{}
	mu      sync.Mutex
	pending []windows.Handle

	// healthProvider supplies the live WS lifecycle snapshot for the
	// "health" RPC. Set via SetHealthProvider after Listen. Optional —
	// when nil, "health" returns ErrNoHealthProvider.
	healthProvider HealthProvider
}

func (s *Server) SetHealthProvider(p HealthProvider) {
	s.healthProvider = p
}

// pipeInstance is the per-connection HANDLE we hand to handle().
// Wrapping in a named struct keeps Read/Write idiomatic and
// matches the *net.UnixConn pattern in server_unix.go.
type pipeInstance struct {
	handle windows.Handle
}

func (p *pipeInstance) Read(buf []byte) (int, error) {
	var read uint32
	if err := windows.ReadFile(p.handle, buf, &read, nil); err != nil {
		return 0, err
	}
	if read == 0 {
		return 0, io.EOF
	}
	return int(read), nil
}

func (p *pipeInstance) Write(buf []byte) (int, error) {
	var written uint32
	if err := windows.WriteFile(p.handle, buf, &written, nil); err != nil {
		return 0, err
	}
	return int(written), nil
}

func (p *pipeInstance) Close() error {
	return windows.CloseHandle(p.handle)
}

// Listen creates the named pipe and primes it for Serve. The
// pipe's ACL is restricted to the current user SID so other
// local users on a shared Windows machine cannot talk to our
// daemon (the default ACL is Everyone, which would leak).
func Listen(path string, status Status, cancel context.CancelFunc) (*Server, error) {
	// Create the very first pipe instance now so a second Listen
	// (same dataDir → same pipeName) fails with
	// ERROR_ACCESS_DENIED / ERROR_PIPE_BUSY and we report a
	// clean error.
	h, err := createUserPipe(path)
	if err != nil {
		return nil, fmt.Errorf("daemon control: create pipe %s: %w", path, err)
	}
	// We close the seed instance here; Serve will recreate one
	// per accept loop. The kernel still has the pipe name
	// registered, so a racing second Listen would see
	// ERROR_PIPE_BUSY on CreateNamedPipe.
	_ = windows.CloseHandle(h)

	srv := &Server{
		pipeName: path,
		status:   status,
		closed:   make(chan struct{}),
	}
	srv.state.Store("starting")
	srv.cancel = cancel
	return srv, nil
}

// createUserPipe allocates one pipe instance with a DACL that
// grants access only to the current user SID + the local
// Administrators group. Anyone else gets ACCESS_DENIED at
// CreateFile time.
func createUserPipe(path string) (windows.Handle, error) {
	sa, err := userOnlySecurityAttributes()
	if err != nil {
		return 0, fmt.Errorf("build security descriptor: %w", err)
	}

	h, err := windows.CreateNamedPipe(
		windows.StringToUTF16Ptr(path),
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|
			windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES,
		0, 0, 0,
		sa,
	)
	if err != nil {
		return 0, err
	}
	return h, nil
}

func (s *Server) SetReady() { s.state.Store("ready") }

func (s *Server) Status() Status {
	status := s.status
	if state, ok := s.state.Load().(string); ok {
		status.State = state
	}
	status.UptimeSeconds = int64(time.Since(status.StartedAt).Seconds())
	return status
}

// Serve runs the accept loop. For each new pipe connection it
// creates a fresh pipe instance, hands it to handle(), and lets
// the goroutine serve the RPC.
//
// Each iteration:
//
//  1. CreateNamedPipe a fresh instance (kernel tees the next
//     connection to it)
//  2. ConnectNamedPipe on the seed instance (or overlapped)
//  3. spawn handle()
//  4. loop
//
// We accept a fixed-number of concurrent connections equal to
// PIPE_UNLIMITED_INSTANCES; in practice the daemon serves at
// most a handful (CLI status / stop / doctor) so the limit is
// theoretical.
func (s *Server) Serve() error {
	for {
		select {
		case <-s.closed:
			return nil
		default:
		}

		h, err := createUserPipe(s.pipeName)
		if err != nil {
			return fmt.Errorf("daemon control: create pipe: %w", err)
		}
		s.trackPipe(h)

		// Recheck closed after trackPipe so a Close() that
		// landed between the top-of-loop select and trackPipe
		// does not leak the just-created pipe. Close() takes
		// s.mu AFTER reading s.closed, so the read below is
		// synchronised with Close()'s snapshot+clear of pending.
		select {
		case <-s.closed:
			s.untrackPipe(h)
			_ = windows.CloseHandle(h)
			return nil
		default:
		}

		// Overlapped ConnectNamedPipe so we can interleave
		// accept + Close. Close() CancelIoEx's any handle
		// listed in s.pending, which unblocks ConnectNamedPipe
		// with ERROR_OPERATION_ABORTED.
		overlapped := &windows.Overlapped{}
		if err := windows.ConnectNamedPipe(h, overlapped); err != nil {
			if !errors.Is(err, windows.ERROR_IO_PENDING) &&
				!errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
				s.untrackPipe(h)
				_ = windows.CloseHandle(h)
				if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
					return nil
				}
				return fmt.Errorf("daemon control: connect pipe: %w", err)
			}
		}

		// Spawn the per-connection goroutine; it owns the
		// handle from here and is responsible for Close +
		// untrack.
		conn := &pipeInstance{handle: h}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn *pipeInstance) {
	defer conn.Close()
	defer s.untrackPipe(conn.handle)

	// WriteRequest / ReadResponse operate on io.ReadWriter.
	// pipeInstance satisfies both, so we can wrap directly.
	req, err := ReadRequest(conn)
	if err != nil {
		_ = WriteError(conn, err)
		return
	}
	switch req.Command {
	case "ping":
		state, _ := s.state.Load().(string)
		_ = WriteResult(conn, Ready{Ready: state == "ready"})
	case "status":
		_ = WriteResult(conn, s.Status())
	case "health":
		if s.healthProvider == nil {
			_ = WriteError(conn, fmt.Errorf("daemon does not provide health snapshot"))
			return
		}
		channel, snapshot, err := s.healthProvider()
		if err != nil {
			_ = WriteError(conn, err)
			return
		}
		_ = WriteResult(conn, HealthPayload{Channel: channel, Health: snapshot})
	case "stop":
		s.stopOnce.Do(func() {
			// Mirror server_unix.go's stop handler. See
			// stop_fastpath.go for the shared fast-path rationale
			// and stop_fastpath_test.go for the cross-platform
			// coverage. CAS closes the Load/Store race with
			// SetReady() — losing the CAS means the runtime has
			// armed its wait select, so we fall through to the
			// graceful cancel path.
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

// Close cancels the accept loop and unblocks any in-flight
// ConnectNamedPipe calls via CancelIoEx. The kernel reaps the
// pipe name when the last handle closes.
//
// Close also fires s.cancel() via s.stopOnce, the same Once that
// the "stop" RPC handler uses. This is the contract that keeps
// the daemon context cancellable from BOTH shutdown paths:
// external Stop RPC AND direct Close. Without it, a Windows
// pipe race (Stop RPC arrives during the seed-close vs new-
// create window) would leave s.cancel() unfired and the daemon
// ctx stuck — exactly the failure mode TestWindowsPipePingStatusStop
// guards against.
//
// Ordering: s.closed is closed BEFORE the pending snapshot is
// taken. The Serve loop rechecks s.closed immediately after each
// trackPipe (see Serve), so any new pipe created in the same
// iteration as Close has its pending ConnectNamedPipe cancelled.
// Without the early-close, a Serve iteration that raced past
// the top-of-loop select would leak one handle + one per-
// connection goroutine (the kernel cancel list never saw the
// new pipe, and CancelIoEx on a not-yet-pending handle is a
// no-op).
func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		close(s.closed)
		pending := s.pending
		s.pending = nil
		s.mu.Unlock()
		for _, h := range pending {
			_ = windows.CancelIoEx(h, nil)
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

// trackPipe adds h to the in-flight set so Close() can cancel
// its pending I/O.
func (s *Server) trackPipe(h windows.Handle) {
	s.mu.Lock()
	s.pending = append(s.pending, h)
	s.mu.Unlock()
}

// untrackPipe removes h from the in-flight set once the
// per-connection goroutine has taken ownership.
func (s *Server) untrackPipe(h windows.Handle) {
	s.mu.Lock()
	for i, p := range s.pending {
		if p == h {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

// userOnlySecurityAttributes builds a SECURITY_ATTRIBUTES with
// a DACL that grants FILE_GENERIC_READ | FILE_GENERIC_WRITE |
// FILE_GENERIC_EXECUTE (== GRGW) only to the current user's SID.
// No other user / group / SID gets access — same isolation Unix
// AF_UNIX gets from filesystem permissions.
//
// SDDL shape:
//
//	D:P(A;;GRGW;;;<user-sid>)
//
//	D — DACL
//	P — Protected (no inheritance from container)
//	A — Allow
//	GRGW — Generic Read + Write (file equivalent of
//	  FILE_GENERIC_READ | FILE_GENERIC_WRITE |
//	  FILE_GENERIC_EXECUTE, the CreateNamedPipe default)
//
// Owner / primary group default to the calling user when
// omitted from SDDL, which matches what we want.
//
// The result is cached at package level — the SDDL/SID/SA never
// change for the lifetime of the process, so we avoid parsing
// SDDL and calling SecurityDescriptorFromString on every accept.
var cachedSA *windows.SecurityAttributes

func userOnlySecurityAttributes() (*windows.SecurityAttributes, error) {
	if cachedSA != nil {
		return cachedSA, nil
	}
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sddl := "D:P(A;;GRGW;;;" + sid + ")"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("parse sddl %q: %w", sddl, err)
	}
	cachedSA = &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}
	return cachedSA, nil
}

// currentUserSID returns the SDDL string form of the current
// process token's user SID (e.g. "S-1-5-21-...").
//
// The result is cached at package level because the SID is a
// process-lifetime constant; refetching on every CreateNamedPipe
// call would burn a syscall per accept. The cache is populated
// lazily on first call.
//
// Implementation: GetCurrentProcessToken (a TOKEN_QUERY token
// owned by the runtime, no Close needed) → GetTokenUser →
// SID.String(). Each step is OS-supported and stable since
// Windows 7.
var cachedSID string

func currentUserSID() (string, error) {
	if cachedSID != "" {
		return cachedSID, nil
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("get token user: %w", err)
	}
	if tu == nil || tu.User.Sid == nil {
		return "", errors.New("token user has no SID")
	}
	sid := tu.User.Sid.String()
	cachedSID = sid
	return sid, nil
}
