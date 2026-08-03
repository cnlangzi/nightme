//go:build unix

package daemoncontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
