//go:build !windows

package host_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// TestStartSharedHost_SpawnsWhenNoDsh: with no dsh on 3080,
// StartSharedHost spawns the configured HostCmd (the bash fake).
// ForceSpawn is required in test env because we can't guarantee
// 3080 is empty in CI — and tests must NOT silently attach to a
// developer's local dsh.
func TestStartSharedHost_SpawnsWhenNoDsh(t *testing.T) {
	fake := writeFakeDSH(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("FAKE_DSH_PIDFILE", pidFile)
	t.Setenv("FAKE_DSH_LIFETIME", "60") // stay alive long enough for assertions

	host.UnsetGlobal()
	host.UnsetSharedHost()
	t.Cleanup(func() {
		host.UnsetGlobal()
		host.UnsetSharedHost()
	})

	sh, err := host.StartSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  dir,
		HostCmd:    fake,
		ForceSpawn: true,
	})
	if err != nil {
		t.Fatalf("StartSharedHost: %v", err)
	}
	// SharedHost no longer exposes Close: the daemon never tears
	// dsh down. Tests still need to terminate the spawned
	// subprocess so the test binary doesn't leak it. Send SIGKILL
	// directly; the kernel reaps on test exit.
	defer killFakeDSH(t, sh)

	pid := waitPIDFile(t, pidFile, 2*time.Second)
	if pid == 0 {
		t.Fatal("fake-dsh never spawned")
	}
	// PID() returns the spawned subprocess's PID — proves we
	// OWNED the subprocess (not attached to a pre-existing one).
	if got := sh.PID(); got != pid {
		t.Errorf("SharedHost.PID=%d, want %d (spawned subprocess)", got, pid)
	}
}

// TestStartSharedHost_NonCanonicalPort — dsh spawned but bound a
// non-3080 port (e.g. via DSH_PORT env override, a config file, or
// the upcoming dsh --port 0 fallback). StartSharedHost must detect
// the mismatch, kill the spawn, and return a clear error — silent
// acceptance would let the daemon think it owns a host that the
// next StartSharedHost call can't re-discover on 3080.
func TestStartSharedHost_NonCanonicalPort(t *testing.T) {
	host.UnsetGlobal()
	host.UnsetSharedHost()
	host.ResetEnsureForTest()
	t.Cleanup(func() {
		host.UnsetGlobal()
		host.UnsetSharedHost()
		host.ResetEnsureForTest()
	})

	// Build a fake-dsh that always reports a non-3080 port,
	// regardless of argv. The daemon parses this URL and the
	// port check (in StartSharedHost) must reject it.
	dir := t.TempDir()
	fakePath := filepath.Join(dir, "fake-dsh-wrong-port.sh")
	script := `#!/bin/bash
echo "dsh web: http://127.0.0.1:13080"
sleep 30
`
	if err := os.WriteFile(fakePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dsh: %v", err)
	}

	sh, err := host.StartSharedHost(context.Background(), host.SharedHostOptions{
		Workspace:  dir,
		HostCmd:    fakePath,
		ForceSpawn: true,
	})
	if err == nil {
		killFakeDSH(t, sh)
		t.Fatal("StartSharedHost should reject non-3080 port, got nil error")
	}
	if !strings.Contains(err.Error(), "13080") || !strings.Contains(err.Error(), "3080") {
		t.Errorf("error should mention both ports, got: %v", err)
	}
}
