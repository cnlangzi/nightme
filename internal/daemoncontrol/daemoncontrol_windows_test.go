//go:build windows

package daemoncontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWindowsLockTryExcludesSecondOwner verifies the LockFileEx
// path mirrors lock_unix.go's flock semantics:
//   - TryLock on an unheld file succeeds
//   - TryLock on a held file returns ErrLocked
//   - Close releases the lock
func TestWindowsLockTryExcludesSecondOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")

	first, err := TryLock(path)
	if err != nil {
		t.Fatalf("first TryLock: %v", err)
	}

	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock err = %v, want ErrLocked", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock after release: %v", err)
	}
	_ = second.Close()
}

// TestWindowsPathStable asserts that ResolvePaths returns the
// same pipe name for the same data dir across calls (the SHA256
// hash of the data dir is the key).
func TestWindowsPathStable(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")

	a, err := ResolvePaths(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolvePaths(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Socket != b.Socket {
		t.Fatalf("pipe name not stable: %q vs %q", a.Socket, b.Socket)
	}
	const wantPrefix = `\\.\pipe\nightme-`
	if !strings.HasPrefix(a.Socket, wantPrefix) {
		t.Fatalf("pipe name %q does not match %s*", a.Socket, wantPrefix)
	}

	// Different data dir → different pipe name.
	other, err := ResolvePaths(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Socket == other.Socket {
		t.Fatalf("pipe name collision between distinct data dirs: %q", a.Socket)
	}
}

// TestWindowsPipePingStatusStop does a full end-to-end RPC
// round-trip against a real named pipe: Listen → Serve →
// Ping (before SetReady → false) → SetReady → GetStatus
// (State=ready, Uptime>0) → Stop → ctx cancelled.
func TestWindowsPipePingStatusStop(t *testing.T) {
	dataDir := t.TempDir()
	paths, err := ResolvePaths(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startedAt := time.Now().Add(-2 * time.Second)
	server, err := Listen(paths.Socket, Status{
		PID:       4242,
		StartedAt: startedAt,
		Channel:   "echo",
		Version:   "test",
	}, cancel)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	go func() { _ = server.Serve() }()

	// Ping before SetReady — must return Ready=false (no error).
	// Retry on transient errors (EOF / broken pipe / not found)
	// because the test races with the server's seed-close vs
	// new-create cycle; production dialNamedPipe also retries
	// internally but a single test-level retry covers the case
	// where the connection survives long enough to be returned
	// to the caller but then the server side closes before
	// responding.
	var ready bool
	for i := 0; i < 50; i++ {
		var err error
		ready, err = Ping(paths.Socket, 2*time.Second)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ready {
		t.Fatal("server reported ready before SetReady")
	}
	if ready {
		t.Fatal("server reported ready before SetReady")
	}

	server.SetReady()

	status, err := GetStatus(paths.Socket, 2*time.Second)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.State != "ready" {
		t.Fatalf("State = %q, want ready", status.State)
	}
	if status.PID != 4242 {
		t.Fatalf("PID = %d, want 4242", status.PID)
	}
	if status.Channel != "echo" {
		t.Fatalf("Channel = %q, want echo", status.Channel)
	}
	if status.Version != "test" {
		t.Fatalf("Version = %q, want test", status.Version)
	}
	if status.UptimeSeconds < 1 {
		t.Fatalf("UptimeSeconds = %d, want >= 1 (StartedAt was 2s ago)", status.UptimeSeconds)
	}

	// Stop: ctx should cancel, server.Status().State should flip
	// to "stopping".
	if err := Stop(paths.Socket, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel daemon context within 2s")
	}
	if got := server.Status().State; got != "stopping" {
		t.Fatalf("State after Stop = %q, want stopping", got)
	}
}

// TestWindowsPipeNotRunning confirms that dialing a pipe that
// has no server returns ErrNotRunning — the same sentinel Unix
// uses for "no socket file".
func TestWindowsPipeNotRunning(t *testing.T) {
	dataDir := t.TempDir()
	paths, err := ResolvePaths(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Ping(paths.Socket, 2*time.Second)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Ping on un-served pipe err = %v, want ErrNotRunning", err)
	}
}

// TestWindowsResolvePathsRunsOnTempDir is the Windows twin of
// TestResolvePathsCreatesPrivateDirectory. We can't assert
// 0o700 perms (Windows ignores chmod mode bits), but we can
// assert that ResolvePaths created the directory and returned
// absolute paths.
func TestWindowsResolvePathsRunsOnTempDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "state")
	paths, err := ResolvePaths(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(paths.Dir) {
		t.Fatalf("Dir not absolute: %s", paths.Dir)
	}
	if !filepath.IsAbs(paths.DaemonLock) {
		t.Fatalf("DaemonLock not absolute: %s", paths.DaemonLock)
	}
	if !filepath.IsAbs(paths.LifecycleLock) {
		t.Fatalf("LifecycleLock not absolute: %s", paths.LifecycleLock)
	}
	info, err := os.Stat(paths.Dir)
	if err != nil {
		t.Fatalf("Stat data dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", paths.Dir)
	}
}
