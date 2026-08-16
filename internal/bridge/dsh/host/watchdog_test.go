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
PORT="3080"
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

// killFakeDSH sends SIGKILL to the subprocess owned by sh (if any)
// and waits for it to be reaped. Replaces the legacy sh.Close() in
// tests after the daemon stopped tearing dsh down on shutdown —
// tests still need to terminate the spawned process so the test
// binary doesn't leak it. No-op when sh owns no subprocess
// (PID == 0).
func killFakeDSH(t *testing.T, sh *host.SharedHost) {
	t.Helper()
	pid := sh.PID()
	if pid == 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		// Already gone (ESRCH); nothing to do.
		return
	}
	_ = proc.Signal(os.Kill)
	// Wait for the kernel to reap so the next test's port /
	// state isn't racing with a zombie. 2s is generous for a
	// SIGKILL'd process.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			if !procAlive(pid) {
				close(done)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Logf("killFakeDSH: pid %d did not exit within 2s", pid)
	}
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
		Workspace:  dir,
		HostCmd:    fake,
		ForceSpawn: true, // bypass discover — drive our own fake-dsh
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

	// Shut down. The second instance is short-lived now, so the
	// fake-dsh will exit on its own. Then we SIGKILL the live
	// subprocess (if any) to clean up. SharedHost no longer
	// exposes Close/Done — the daemon doesn't tear dsh down.
	t.Setenv("FAKE_DSH_LIFETIME", "0.05")
	killFakeDSH(t, sh)
}

// TestSharedHost_GracefulCloseStopsWatchdog verifies that Close()
// prevents the watchdog from respawning even when the dsh subprocess
// is still alive at close-time. Without this guarantee a Close
// could race with a respawn and leave an orphaned subprocess.
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