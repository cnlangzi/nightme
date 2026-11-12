//go:build !windows

package main

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

// TestKill_TerminatesRunningChild spawns a real child, runs the same
// code path `nightme kill` uses, and asserts the child dies and its
// registry entry flips to exited.
//
// Ground truth for "the child exited" is Wait() returning, not
// kill(pid, 0): after SIGTERM the process stays visible as a zombie
// until its parent reaps it.
func TestKill_TerminatesRunningChild(t *testing.T) {
	_, asFile, asRun, _ := listFixture(t)

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	pid := cmd.Process.Pid
	waitErr := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waitErr <- err
	}()

	oc := killSession(asFile, listRow{AgentSessionID: asRun.ID, PID: pid}, "", false)
	if oc.err != nil {
		t.Fatalf("killSession err = %v, want nil", oc.err)
	}
	if oc.result != "killed" {
		t.Errorf("result = %q, want %q", oc.result, "killed")
	}

	select {
	case <-waitErr:
		// Wait returning proves the child exited; the error itself
		// is non-nil because it died from a signal.
	case <-time.After(5 * time.Second):
		t.Fatalf("child pid %d still alive 5s after kill", pid)
	}

	e, ok := asFile.Get(asRun.ID)
	if !ok {
		t.Fatalf("entry %s deleted, want it preserved as exited", asRun.ID)
	}
	if e.Status != registry.StatusExited {
		t.Errorf("Status = %q, want %q", e.Status, registry.StatusExited)
	}
}

// TestKill_ForceKillsRunningChild exercises the --force path (SIGKILL
// with no grace window) against a child that ignores SIGTERM.
func TestKill_ForceKillsRunningChild(t *testing.T) {
	_, asFile, asRun, _ := listFixture(t)

	// `trap '' TERM` makes the child immune to SIGTERM, so only the
	// SIGKILL path can end it.
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh not available: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waitErr <- err
	}()

	oc := killSession(asFile, listRow{AgentSessionID: asRun.ID, PID: cmd.Process.Pid}, "", true)
	if oc.err != nil {
		t.Fatalf("killSession(force) err = %v, want nil", oc.err)
	}

	select {
	case <-waitErr:
	case <-time.After(5 * time.Second):
		t.Fatalf("child pid %d survived --force kill", cmd.Process.Pid)
	}
}

// TestKill_AlreadyExitedIsSuccess asserts a child that died between
// list and kill is reported as "already exited" (not a failure) and
// its entry is still marked exited.
func TestKill_AlreadyExitedIsSuccess(t *testing.T) {
	_, asFile, asRun, _ := listFixture(t)

	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh not available: %v", err)
	}
	pid := cmd.Process.Pid
	// Wait reaps the child, so the PID is fully gone (no zombie).
	_ = cmd.Wait()

	oc := killSession(asFile, listRow{AgentSessionID: asRun.ID, PID: pid}, "", false)
	if oc.err != nil {
		t.Fatalf("killSession err = %v, want nil", oc.err)
	}
	if !strings.Contains(oc.result, "already exited") {
		t.Errorf("result = %q, want %q", oc.result, "already exited")
	}
	e, _ := asFile.Get(asRun.ID)
	if e.Status != registry.StatusExited {
		t.Errorf("Status = %q, want %q", e.Status, registry.StatusExited)
	}
}

// TestKill_RefusesRecycledPID is the safety net for stale registry
// entries. agent_sessions.json can hold PIDs from days ago (entries
// survive a daemon crash) and the OS recycles PIDs, so a naive sweep
// would SIGTERM whatever process happens to hold that number now —
// the user's editor, a build, anything. The entry claims "claude",
// the live PID is /bin/sleep: the sweep must refuse and say so.
func TestKill_RefusesRecycledPID(t *testing.T) {
	_, asFile, asRun, _ := listFixture(t)

	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	oc := killSession(asFile, listRow{AgentSessionID: asRun.ID, Agent: "claude", PID: pid}, "claude", false)
	if oc.err != nil {
		t.Fatalf("killSession err = %v, want nil", oc.err)
	}
	if !strings.Contains(oc.result, "skipped") || !strings.Contains(oc.result, "sleep") {
		t.Errorf("result = %q, want a skip naming the actual process", oc.result)
	}

	// The innocent process must still be alive.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("unrelated pid %d was signalled: %v", pid, err)
	}
	// And the entry must NOT be marked exited — we proved nothing.
	e, _ := asFile.Get(asRun.ID)
	if e.Status == registry.StatusExited {
		t.Errorf("entry marked exited although the kill was refused")
	}
}
