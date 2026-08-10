package chatsession

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// TestRespawn_ReapsOrphanBeforeSpawn verifies respawn kills a stale
// PID (left over from a previous crashed runtime) before launching
// the new child. Without reap the new child would race with /
// accumulate alongside the orphan.
func TestRespawn_ReapsOrphanBeforeSpawn(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	orphanPID := cmd.Process.Pid

	// Construct an AS in the same shape the runtime would see after
	// restoring from disk: stat=Exited, handle=nil, pid=orphanPID.
	as := NewAgentSession(newAgentSessionID(), "cs_test", "cc", "/code", nil)
	as.asMu.Lock()
	as.pid = orphanPID
	as.stat = StatusExited
	as.asMu.Unlock()

	// Background-reap the orphan so cmd.Process.Wait returns
	// promptly once the child is killed.
	waitDone := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(waitDone)
	}()

	spawner := &fakeRestartSpawner{handle: &callRecordingAS{fakeAgentSession: newFakeAgentSession(9999)}}

	if err := as.respawn(context.Background(), spawner, nil, ""); err != nil {
		t.Fatalf("respawn: %v", err)
	}

	// Orphan must be reaped.
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("orphan pid %d still alive 2s after respawn", orphanPID)
	}

	// New handle must be wired in.
	if got := as.Handle(); got == nil {
		t.Fatalf("Handle should be non-nil after respawn")
	}
	if got := as.PID(); got != 9999 {
		t.Fatalf("PID = %d, want 9999", got)
	}
	if got := as.Status(); got != StatusRunning {
		t.Fatalf("Status = %s, want StatusRunning", got)
	}
}

// TestRespawn_SpawnFailureRunningToExited verifies the New-fallback
// failure path: AS was Running (we replaced a closed handle), spawn
// failed → demote to Exited with exitCode=-1.
func TestRespawn_SpawnFailureRunningToExited(t *testing.T) {
	as := NewAgentSession(newAgentSessionID(), "cs_test", "cc", "/code", nil)
	as.asMu.Lock()
	as.pid = 12345
	as.stat = StatusRunning
	as.asMu.Unlock()

	spawner := &fakeFailingSpawner{err: errors.New("spawn blew up")}
	if err := as.respawn(context.Background(), spawner, nil, ""); err == nil {
		t.Fatalf("respawn should have returned error")
	}

	if got := as.Handle(); got != nil {
		t.Fatalf("Handle should be nil after spawn failure, got %T", got)
	}
	if got := as.PID(); got != 0 {
		t.Fatalf("PID = %d, want 0 after failure", got)
	}
	if got := as.Status(); got != StatusExited {
		t.Fatalf("Status = %s, want StatusExited", got)
	}
	if as.exitCode == nil || *as.exitCode != -1 {
		t.Fatalf("exitCode = %v, want -1", as.exitCode)
	}
}

// TestRespawn_SpawnFailurePreservesDetached verifies the Spawn-cold
// failure path: AS was Detached (never ran), spawn failed → stay
// Detached, no exitCode set. Pinned by
// TestAgentSession_SpawnFailureLeavesDetached.
func TestRespawn_SpawnFailurePreservesDetached(t *testing.T) {
	as := NewAgentSession(newAgentSessionID(), "cs_test", "cc", "/code", nil)
	// as.stat defaults to StatusDetached via newAgentSessionRuntime.

	spawner := &fakeFailingSpawner{err: errors.New("spawn blew up")}
	if err := as.respawn(context.Background(), spawner, nil, ""); err == nil {
		t.Fatalf("respawn should have returned error")
	}

	if got := as.Status(); got != StatusDetached {
		t.Fatalf("Status = %s, want StatusDetached (must preserve never-ran state)", got)
	}
	if as.exitCode != nil {
		t.Fatalf("exitCode = %v, want nil (no run ever happened)", as.exitCode)
	}
}

// TestRespawn_NoReapNeededWhenNoOrphan verifies respawn is a no-op
// for the reap step when there is no orphan (zero pid). Catches
// regressions where reap becomes unconditional.
func TestRespawn_NoReapNeededWhenNoOrphan(t *testing.T) {
	as := NewAgentSession(newAgentSessionID(), "cs_test", "cc", "/code", nil)
	// pid defaults to 0 (never spawned).

	spawner := &fakeRestartSpawner{handle: &callRecordingAS{fakeAgentSession: newFakeAgentSession(4242)}}
	if err := as.respawn(context.Background(), spawner, nil, ""); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if got := as.PID(); got != 4242 {
		t.Fatalf("PID = %d, want 4242", got)
	}
}

// TestRespawn_DoesNotTouchSessionID verifies the caller-decides
// contract: respawn leaves as.sessionID alone. Spawn preserves it
// for resume; New's fallback clears it explicitly after respawn.
func TestRespawn_DoesNotTouchSessionID(t *testing.T) {
	as := NewAgentSession(newAgentSessionID(), "cs_test", "cc", "/code", nil)
	as.SetSessionID("resume-me-later")

	spawner := &fakeRestartSpawner{handle: &callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}}
	if err := as.respawn(context.Background(), spawner, nil, "resume-me-later"); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if got := as.SessionID(); got != "resume-me-later" {
		t.Fatalf("SessionID = %q, want %q (respawn must not clear)", got, "resume-me-later")
	}
}