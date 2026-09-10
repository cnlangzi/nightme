//go:build !windows

package host_test

import (
	"context"
	"path/filepath"
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

// (intentionally empty: no NonCanonicalPort assertion in the new
// architecture. spawnAndWire passes --port explicitly, so dsh can't
// bind elsewhere unless it's a dsh bug — and the contract is enforced
// structurally, not by parsing stdout. waitForListen on the requested
// port + cli.Start's HTTP handshake catch any drift downstream.)
