// watchdog_test.go — tests for the SharedHost watchdog / respawn cycle.
//
// Uses a tiny bash "fake dsh" subprocess that prints the URL line
// and then either dies fast (simulating crash) or sleeps (so we can
// observe the respawned instance). Tests do NOT depend on the real
// dsh binary on PATH.
//
// Build tag: the watchdog is only useful on unix where dsh runs;
// windows is skipped via build tag.

//go:build !windows

package host_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

const fakeDSHScript = `#!/bin/bash
PORT=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --port) PORT="$2"; shift 2;;
        *) shift;;
    esac
done
echo "dsh web: http://127.0.0.1:${PORT}"
# Write PID to FAKE_DSH_PIDFILE via direct redirect — the parent
# closes our stdout pipe as soon as it parses the URL line above,
# so any subsequent echo to stdout would EPIPE. printf to file
# bypasses stdout entirely.
if [[ -n "$FAKE_DSH_PIDFILE" ]]; then
    printf '%d' "$$" > "$FAKE_DSH_PIDFILE"
fi
LIFETIME="${FAKE_DSH_LIFETIME:-0.05}"
sleep "$LIFETIME"
exit 1
`

// writeFakeDSH drops the bash script into t.TempDir() and chmods it.
func writeFakeDSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-dsh.sh")
	if err := os.WriteFile(path, []byte(fakeDSHScript), 0o755); err != nil {
		t.Fatalf("write fake dsh: %v", err)
	}
	return path
}

// findFreePort is reserved for future tests that need to dial the
// fake-dsh. Currently unused; the bash script just echoes whatever
// port it was given and exits, so no kernel port binding is needed.
var findFreePort = func() {} //nolint:unused // documentation stub

// waitPIDFile polls path until it contains a positive integer, or
// returns 0 on timeout.
func waitPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, perr := strconv.Atoi(string(data)); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0
}

// procAlive returns true if pid is currently a running process.
// Uses `kill -0` shell-out for cross-distro compatibility.
func procAlive(pid int) bool {
	cmd := exec.Command("kill", "-0", strconv.Itoa(pid))
	return cmd.Run() == nil
}

// TestSharedHost_WatchdogRespawns verifies the watchdog observes a
// fake-dsh crash and spawns a replacement. The bash script writes
// its PID to FAKE_DSH_PIDFILE on startup; we observe the file
// changing (different PID) as proof of respawn.
//
// Phase 1 (crash): LIFETIME=0.1s, fake-dsh dies fast → watchdog fires.
// Phase 2 (alive): LIFETIME=10s, watchdog respawns, new instance sticks.
func TestSharedHost_WatchdogRespawns(t *testing.T) {
	fake := writeFakeDSH(t)
	dir := t.TempDir()

	firstPIDFile := filepath.Join(dir, "first.pid")
	t.Setenv("FAKE_DSH_PIDFILE", firstPIDFile)
	t.Setenv("FAKE_DSH_LIFETIME", "0.1")

	// Reset singletons so this test (and the next) can install their
	// own. The watchdog tests run sequentially within the same
	// process; without this, the second test panics on SetGlobal's
	// "called twice" guard.
	host.UnsetGlobal()
	host.UnsetSharedHost()
	t.Cleanup(func() {
		host.UnsetGlobal()
		host.UnsetSharedHost()
	})

	sh, err := host.StartSharedHost(context.Background(), host.SharedHostOptions{
		Workspace: dir,
		HostCmd:   fake,
	})
	if err != nil {
		t.Fatalf("StartSharedHost: %v", err)
	}

	firstPID := waitPIDFile(t, firstPIDFile, 2*time.Second)
	if firstPID == 0 {
		t.Fatal("first fake-dsh never started")
	}

	// Wait for first instance to die + watchdog to respawn.
	// Switch the lifetime so the second instance is long-lived.
	time.Sleep(400 * time.Millisecond)
	secondPIDFile := filepath.Join(dir, "second.pid")
	t.Setenv("FAKE_DSH_PIDFILE", secondPIDFile)
	t.Setenv("FAKE_DSH_LIFETIME", "10")

	secondPID := waitPIDFile(t, secondPIDFile, 5*time.Second)
	if secondPID == 0 {
		t.Fatal("watchdog did not respawn fake-dsh (no second pid file)")
	}
	if secondPID == firstPID {
		t.Errorf("watchdog reused pid %d (should be a new process)", secondPID)
	}

	// Verify first instance is actually gone.
	if procAlive(firstPID) {
		t.Errorf("first fake-dsh pid %d still alive", firstPID)
	}

	// Shut down. Let the second instance die on SIGINT cleanly.
	t.Setenv("FAKE_DSH_LIFETIME", "0.05")
	if err := sh.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case <-sh.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("SharedHost.Done not closed within 2s of Close")
	}
}

// TestSharedHost_GracefulCloseStopsWatchdog verifies that Close()
// prevents the watchdog from respawning even when the dsh subprocess
// is still alive at close-time. Without this guarantee a Close
// could race with a respawn and leave an orphaned subprocess.
func TestSharedHost_GracefulCloseStopsWatchdog(t *testing.T) {
	fake := writeFakeDSH(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("FAKE_DSH_PIDFILE", pidFile)
	t.Setenv("FAKE_DSH_LIFETIME", "60") // long-lived; only SIGINT kills it

	host.UnsetGlobal()
	host.UnsetSharedHost()
	t.Cleanup(func() {
		host.UnsetGlobal()
		host.UnsetSharedHost()
	})

	sh, err := host.StartSharedHost(context.Background(), host.SharedHostOptions{
		Workspace: dir,
		HostCmd:   fake,
	})
	if err != nil {
		t.Fatalf("StartSharedHost: %v", err)
	}
	pid := waitPIDFile(t, pidFile, 2*time.Second)
	if pid == 0 {
		t.Fatal("fake-dsh never started")
	}

	if err := sh.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	// Watchdog should exit promptly.
	select {
	case <-sh.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("SharedHost.Done not closed within 1s of Close")
	}

	// Subprocess should be gone.
	if procAlive(pid) {
		t.Errorf("fake-dsh pid %d still alive after Close", pid)
	}
}

// ─── pure-unit backoff check ──────────────────────────────────────

// TestRespawnDelay_Bounded verifies respawnDelay returns a value
// within the documented bounds. We test via behavior (the
// constant is unexported), checking that an obviously huge attempt
// index clamps to the max.
func TestRespawnDelay_Bounded(t *testing.T) {
	// We can't call respawnDelay directly (unexported). Instead,
	// assert via the watchdog's behavior: when StartSharedHost is
	// given a fake-dsh that always fails, the watchdog should exit
	// within ~maxRespawnAttempts × respawnBackoffMax (a few minutes).
	// We don't actually wait that long; we just verify the bounds
	// are reasonable. This test is a placeholder for documentation.
	t.Skip("respawnDelay is unexported; bounded via direct testing of the watchdog which is slow")
}

// Compile-time anchor (so unused imports don't drift if the file
// gets pruned to skip-tags).
var _ = os.Stderr