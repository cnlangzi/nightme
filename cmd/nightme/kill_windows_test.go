//go:build windows

package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
	"github.com/cnlangzi/nightme/internal/procsweep"
	"github.com/cnlangzi/nightme/internal/registry"
)

// startPing launches a long-lived child that is NOT one of the host
// interpreters — a stand-in for "some unrelated program that happens
// to own this PID now". ping.exe is a System32 binary present on
// every Windows install and runs until it is killed.
//
// The returned image name is what the OS reports for the child, so
// the assertions below stay valid whatever the file happens to be
// called on this machine.
//
// The caller owns the child's lifetime: tests that leave it running
// must reap it.
func startPing(t *testing.T) (*exec.Cmd, string) {
	t.Helper()

	cmd := proc.New(context.Background(), "ping", "-t", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skipf("ping.exe not available: %v", err)
	}

	image := procsweep.ImageName(cmd.Process.Pid)
	if image == "" {
		_ = cmd.Process.Kill()
		t.Skipf("no image name for pid %d", cmd.Process.Pid)
	}
	if isHostInterpreter(image) {
		_ = cmd.Process.Kill()
		t.Skipf("fixture image %q is a host interpreter", image)
	}
	return cmd, image
}

// isHostInterpreter reports whether image is one of the interpreters
// nightme's spawn path can start for a shim agent.
func isHostInterpreter(image string) bool {
	switch strings.ToLower(image) {
	case "cmd.exe", "powershell.exe", "node.exe":
		return true
	}
	return false
}

// TestKill_RefusesRecycledPID is the safety net for stale registry
// entries. Windows hands a freed PID straight back into the pool, so
// a naive sweep would TerminateProcess whatever owns that number
// now — the user's editor, a build, another agent. The entry claims
// "claude" while the live PID belongs to something else: the sweep
// must refuse and name what it found.
func TestKill_RefusesRecycledPID(t *testing.T) {
	_, asFile, asRun, _ := listFixture(t)

	cmd, image := startPing(t)
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	oc := killSession(asFile, listRow{AgentSessionID: asRun.ID, Agent: "claude", PID: cmd.Process.Pid}, "claude", false)
	if oc.err != nil {
		t.Fatalf("killSession err = %v, want nil", oc.err)
	}
	if !strings.Contains(oc.result, "skipped") || !strings.Contains(oc.result, image) {
		t.Errorf("result = %q, want a skip naming %q", oc.result, image)
	}

	// The innocent process must still be alive.
	if got := procsweep.ImageName(cmd.Process.Pid); got != image {
		t.Errorf("unrelated pid %d was terminated: image = %q, want %q", cmd.Process.Pid, got, image)
	}
	// And the entry must NOT be marked exited — we proved nothing.
	e, _ := asFile.Get(asRun.ID)
	if e.Status == registry.StatusExited {
		t.Errorf("entry marked exited although the kill was refused")
	}
}

// TestKill_TerminatesRunningChild asserts a real child is
// terminated and its registry entry flips to exited.
func TestKill_TerminatesRunningChild(t *testing.T) {
	_, asFile, asRun, _ := listFixture(t)

	cmd, _ := startPing(t)
	// The test reaps the child itself below; this only covers the
	// path where the sweep fails before the child dies.
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	oc := killSession(asFile, listRow{AgentSessionID: asRun.ID, PID: cmd.Process.Pid}, "", false)
	if oc.err != nil {
		t.Fatalf("killSession err = %v, want nil", oc.err)
	}
	if oc.result != "killed" {
		t.Errorf("result = %q, want %q", oc.result, "killed")
	}

	waitErr := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waitErr <- err
	}()

	select {
	case <-waitErr:
		// Wait returning proves the child exited; the error itself
		// is non-nil because it was terminated.
	case <-time.After(5 * time.Second):
		t.Fatalf("child pid %d still alive 5s after kill", cmd.Process.Pid)
	}

	e, ok := asFile.Get(asRun.ID)
	if !ok {
		t.Fatalf("entry %s deleted, want it preserved as exited", asRun.ID)
	}
	if e.Status != registry.StatusExited {
		t.Errorf("Status = %q, want %q", e.Status, registry.StatusExited)
	}
}
