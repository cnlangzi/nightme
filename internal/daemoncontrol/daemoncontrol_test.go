//go:build unix

package daemoncontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockExcludesSecondOwnerAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	first, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock error = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock after release: %v", err)
	}
	_ = second.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %o, want 600", got)
	}
}

func TestResolvePathsCreatesPrivateDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nightme-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	paths, err := ResolvePaths(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(paths.Socket) {
		t.Fatalf("socket path is not absolute: %s", paths.Socket)
	}
	info, err := os.Stat(paths.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("data dir mode = %o, want 700", got)
	}
}

func TestServerPingStatusAndStop(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nightme-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	paths, err := ResolvePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server, err := Listen(paths.Socket, Status{
		PID: 42, StartedAt: time.Now().Add(-time.Second), Channel: "echo", Version: "test",
	}, cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go func() { _ = server.Serve() }()

	ready, err := Ping(paths.Socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("server reported ready before SetReady")
	}
	server.SetReady()
	status, err := GetStatus(paths.Socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || status.PID != 42 || status.Channel != "echo" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if err := Stop(paths.Socket, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel daemon context")
	}
	if got := server.Status().State; got != "stopping" {
		t.Fatalf("state = %q, want stopping", got)
	}
}

// TestStopInStartingStateExits covers the bug fix for `nightme restart`
// in the window where runDaemon is between Listen and SetReady. In
// that window cancel() is unconsumed by the wait select (ch.Start is
// synchronous and runs before the select), so a Stop RPC must take
// the fast-path that exits the process to release the daemon flock.
//
// Without the fix, stopDaemon would poll TryLock(DaemonLock) for 15s
// without seeing it released (the daemon is still in ch.Start), and
// `nightme restart` would error out with "daemon did not stop within
// 15s".
func TestStopInStartingStateExits(t *testing.T) {
	dir := t.TempDir()
	paths, err := ResolvePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := context.WithCancel(context.Background())
	server, err := Listen(paths.Socket, Status{
		PID: 42, StartedAt: time.Now(), Channel: "echo", Version: "test",
	}, cancel)
	if err != nil {
		t.Fatal(err)
	}
	// Intentionally NOT calling server.Close() — the fast-path
	// bypasses it via osExit.
	go func() { _ = server.Serve() }()

	// Intercept the osExit call so the test process survives.
	exitCode := make(chan int, 1)
	origExit := osExit
	osExit = func(code int) { exitCode <- code }
	t.Cleanup(func() { osExit = origExit })

	// Sanity: state is "starting" before SetReady.
	if got := server.Status().State; got != "starting" {
		t.Fatalf("pre-Stop state = %q, want starting", got)
	}

	// Send Stop RPC. Server should take the fast-path: write
	// response, close conn, call osExit(0). The client must
	// receive the response (Unix socket flush requires explicit
	// conn.Close before exit), so Stop should return nil.
	if err := Stop(paths.Socket, 2*time.Second); err != nil {
		t.Fatalf("Stop returned error: %v (fast-path must write a response before exit)", err)
	}

	// Verify osExit was called with 0.
	select {
	case code := <-exitCode:
		if code != 0 {
			t.Fatalf("osExit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("osExit was not called within 1s")
	}
}

// TestStartingStateStopAckContinuesPastWriteAndCloseErrors covers
// the error-surfacing contract added in response to PR review:
// WriteResult and conn.Close failures must be logged but MUST NOT
// prevent osExit from firing. The operator gets a breadcrumb in
// daemon-stderr.log and the flock still releases — the worst case
// "client sees EOF + stopDaemon times out" is still better than
// "stopDaemon times out AND daemon keeps running".
func TestStartingStateStopAckContinuesPastWriteAndCloseErrors(t *testing.T) {
	// Capture the diagnostic log output so the test stays quiet.
	logBuf := &bytes.Buffer{}
	prevLogf := stopLogf
	stopLogf = func(format string, args ...any) {
		fmt.Fprintf(logBuf, format+"\n", args...)
	}
	t.Cleanup(func() { stopLogf = prevLogf })

	exitCode := make(chan int, 1)
	prevExit := osExit
	osExit = func(code int) { exitCode <- code }
	t.Cleanup(func() { osExit = prevExit })

	failing := &failingConn{
		writeErr: errors.New("synthetic write failure"),
		closeErr: errors.New("synthetic close failure"),
	}
	startingStateStopAck(failing)

	select {
	case code := <-exitCode:
		if code != 0 {
			t.Fatalf("osExit code = %d, want 0 (must fire even when write/close fail)", code)
		}
	case <-time.After(time.Second):
		t.Fatal("osExit was not called within 1s — fast-path bailed on write/close error")
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "write response failed") {
		t.Errorf("expected write error to be logged, got: %q", logs)
	}
	if !strings.Contains(logs, "close conn failed") {
		t.Errorf("expected close error to be logged, got: %q", logs)
	}
}

// failingConn is a writeCloseCloser that returns canned errors.
// Verifies the fast-path logs but does not propagate them.
type failingConn struct {
	writeErr error
	closeErr error
	closed   bool
}

func (f *failingConn) Write(p []byte) (int, error) { return 0, f.writeErr }
func (f *failingConn) Close() error                { f.closed = true; return f.closeErr }

// TestStopInReadyStateCancels covers the opposite branch: when state
// is already "ready" (SetReady has fired), the stop handler must
// take the graceful path — call cancel() so runDaemon's wait select
// fires, runDaemon runs ShutdownRunMulti, then the runtime exits
// cleanly. The fast-path osExit must NOT fire here, because the
// runtime is now armed and waiting for the cancel.
func TestStopInReadyStateCancels(t *testing.T) {
	dir := t.TempDir()
	paths, err := ResolvePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server, err := Listen(paths.Socket, Status{
		PID: 42, StartedAt: time.Now(), Channel: "echo", Version: "test",
	}, cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go func() { _ = server.Serve() }()

	// SetReady transitions state to "ready" — the wait select in
	// runDaemon is now armed and will consume the cancel.
	server.SetReady()

	// Intercept osExit to verify it is NOT called on this path.
	exitCalled := make(chan struct{}, 1)
	origExit := osExit
	osExit = func(code int) { exitCalled <- struct{}{} }
	t.Cleanup(func() { osExit = origExit })

	if err := Stop(paths.Socket, time.Second); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	// cancel() must fire — verify ctx.Done within 1s.
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel() not called when state was ready")
	}

	// osExit must NOT be called on the ready-state path.
	select {
	case <-exitCalled:
		t.Fatal("osExit fired on ready-state path — fast-path leaked into graceful branch")
	case <-time.After(50 * time.Millisecond):
		// expected: no exit
	}
}

func TestListenRejectsSymlinkSocket(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "daemon.sock")
	if err := os.Symlink(target, socket); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(socket, Status{}, func() {}); err == nil {
		t.Fatal("Listen accepted a symlink socket path")
	}
}
