//go:build windows

package daemoncontrol

import (
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/windows"
)

// RequestSocket opens the named pipe at path, writes `command`,
// reads the response into result (which may be nil), and closes
// the pipe. Same surface as the Unix client_unix.go; the
// transport is a named pipe instead of an AF_UNIX socket.
//
// Timeout handling:
//   - The dial retries CreateFile every 20ms until the pipe
//     instance exists or the budget expires. This is the
//     client-side equivalent of WaitNamedPipe — that syscall
//     isn't in golang.org/x/sys/windows, so we approximate it.
//     The dial is capped at min(timeout/2, remaining) so a
//     contended CreateFile cannot starve the I/O of its
//     full budget.
//   - The write+read happens in a goroutine so we can apply the
//     same whole-call deadline the Unix client gets from
//     conn.SetDeadline. Synchronous ReadFile has no native
//     timeout, so we add one via select and CancelIoEx on expiry.
func RequestSocket(path, command string, timeout time.Duration, result any) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	// Reserve half the budget for the I/O. A slow CreateFile
	// (named pipe audit, ACL check) shouldn't burn the budget
	// the I/O needs to finish.
	dialBudget := timeout / 2
	if dialBudget < 50*time.Millisecond {
		dialBudget = timeout
	}

	handle, err := dialNamedPipe(path, dialBudget)
	if err != nil {
		return classifyPipeOpenErr(err)
	}
	defer windows.CloseHandle(handle)

	conn := &pipeConn{handle: handle}

	type ioResult struct {
		err error
	}
	done := make(chan ioResult, 1)
	go func() {
		if err := WriteRequest(conn, command); err != nil {
			done <- ioResult{fmt.Errorf("send daemon request: %w", err)}
			return
		}
		if err := ReadResponse(conn, result); err != nil {
			done <- ioResult{fmt.Errorf("read daemon response: %w", err)}
			return
		}
		done <- ioResult{nil}
	}()

	select {
	case r := <-done:
		return r.err
	case <-time.After(timeout):
		// Cancel I/O so the goroutine unblocks (otherwise the
		// kernel call stays pending and the goroutine leaks
		// until the daemon writes back — bad if the daemon
		// crashed mid-request).
		_ = windows.CancelIoEx(handle, nil)
		return fmt.Errorf("daemon request timed out after %s", timeout)
	}
}

func Ping(path string, timeout time.Duration) (bool, error) {
	var ready Ready
	if err := RequestSocket(path, "ping", timeout, &ready); err != nil {
		return false, err
	}
	return ready.Ready, nil
}

func GetStatus(path string, timeout time.Duration) (Status, error) {
	var status Status
	if err := RequestSocket(path, "status", timeout, &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

// GetHealth returns the live WS lifecycle snapshot from the running
// daemon. The returned HealthPayload.Health is the raw JSON-encoded
// WSHealthSnapshot (caller-side unmarshal). Used by `nightme health`.
func GetHealth(path string, timeout time.Duration) (HealthPayload, error) {
	var payload HealthPayload
	if err := RequestSocket(path, "health", timeout, &payload); err != nil {
		return HealthPayload{}, err
	}
	return payload, nil
}

func Stop(path string, timeout time.Duration) error {
	return RequestSocket(path, "stop", timeout, nil)
}

// --- internal helpers ---

// dialNamedPipe polls CreateFile until the named-pipe instance is
// available or the timeout elapses. Returns the last CreateFile
// error on timeout.
func dialNamedPipe(path string, timeout time.Duration) (windows.Handle, error) {
	ptr := windows.StringToUTF16Ptr(path)
	deadline := time.Now().Add(timeout)
	const retry = 20 * time.Millisecond
	var lastErr error
	for {
		h, err := windows.CreateFile(
			ptr,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, // no FILE_SHARE_* — exclusive client handle
			nil,
			windows.OPEN_EXISTING,
			0, 0,
		)
		if err == nil {
			return h, nil
		}
		lastErr = err
		// Retry on transient errors that mean "the pipe name
		// exists but no live instance right now":
		//   - ERROR_FILE_NOT_FOUND: pipe name not registered yet
		//     (server hasn't called CreateNamedPipe)
		//   - ERROR_BROKEN_PIPE: pipe instance exists but was
		//     just closed (e.g. server's seed instance in
		//     server_windows.go:Listen, or server.Serve rotating
		//     through instances)
		// Both are momentary; the next poll cycle should find a
		// live instance. Other errors (access denied, bad path)
		// give up immediately.
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
			!errors.Is(err, windows.ERROR_BROKEN_PIPE) {
			return 0, err
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(retry)
	}
	return 0, lastErr
}

// classifyPipeOpenErr translates the CreateFile errors that
// mean "no daemon" into ErrNotRunning so Ping / GetStatus / Stop
// callers can short-circuit cleanly. Other errors bubble up
// wrapped.
func classifyPipeOpenErr(err error) error {
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
		// pipe instance doesn't exist (dial timed out)
		return ErrNotRunning
	case errors.Is(err, windows.ERROR_PIPE_BUSY):
		// all pipe instances are in use — server is up but
		// saturated. Treat as transient, not "not running".
		return fmt.Errorf("connect to nightme daemon: %w", err)
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		// Pipe exists but our ACL doesn't grant access — likely
		// someone else's daemon on a shared machine.
		return ErrNotRunning
	default:
		return fmt.Errorf("connect to nightme daemon: %w", err)
	}
}

// pipeConn adapts a Windows named-pipe HANDLE to io.ReadWriteCloser
// for protocol.go's WriteRequest / ReadResponse helpers.
type pipeConn struct {
	handle windows.Handle
}

func (p *pipeConn) Read(buf []byte) (int, error) {
	var read uint32
	if err := windows.ReadFile(p.handle, buf, &read, nil); err != nil {
		return 0, err
	}
	if read == 0 {
		return 0, io.EOF
	}
	return int(read), nil
}

func (p *pipeConn) Write(buf []byte) (int, error) {
	var written uint32
	if err := windows.WriteFile(p.handle, buf, &written, nil); err != nil {
		return 0, err
	}
	return int(written), nil
}
