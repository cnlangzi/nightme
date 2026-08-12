//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
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

// TestKill_GraceWindowLetsChildExitOnItsOwn asserts the default path
// gives the child a chance to shut down cleanly: a process that
// handles SIGTERM should not be SIGKILLed.
func TestKill_GraceWindowLetsChildExitOnItsOwn(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "trap 'exit 7' TERM; sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh not available: %v", err)
	}
	state := make(chan *exec.ExitError, 1)
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		if ee, ok := err.(*exec.ExitError); ok {
			state <- ee
		} else {
			state <- nil
		}
		close(done)
	}()
	// Give the shell time to install the trap before signalling.
	time.Sleep(200 * time.Millisecond)

	if err := killProcess(cmd.Process.Pid, false); err != nil {
		t.Fatalf("killProcess: %v", err)
	}

	select {
	case ee := <-state:
		if ee == nil {
			t.Fatalf("child exited without an ExitError, want exit status 7")
		}
		if code := ee.ExitCode(); code != 7 {
			t.Errorf("exit code = %d, want 7 (SIGTERM handler ran, no SIGKILL)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("child did not exit after SIGTERM")
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

// TestKill_VerifyPIDOwnerFailsOpen pins the fail-open policy: when
// we cannot obtain a contradiction, the sweep proceeds. Refusing on
// "cannot verify" would make `nightme kill` useless on any system
// where the probe doesn't work.
func TestKill_VerifyPIDOwnerFailsOpen(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	if _, ok := verifyPIDOwner(cmd.Process.Pid, ""); !ok {
		t.Errorf("unknown agent command must not block the kill")
	}
	// A dead PID cannot be probed either; killProcess reports it as
	// "already exited", so verification must not swallow it first.
	if _, ok := verifyPIDOwner(1<<30, "claude"); !ok {
		t.Errorf("unprobeable pid must not block the kill")
	}
	// Positive match still passes.
	if _, ok := verifyPIDOwner(cmd.Process.Pid, "/usr/bin/sleep"); !ok {
		t.Errorf("basename match must verify (ps comm may be a bare name)")
	}
}

// TestKill_VerifyPIDOwnerLongBinaryName pins the platform truth the
// PID-identity check rests on: what `ps -o comm=` reports for a long
// executable name.
//
// Verified on darwin 24: the full exec path, no truncation. Linux
// truncates comm to TASK_COMM_LEN-1 = 15 characters. Either way the
// contract must hold — a live process started from this binary
// verifies as itself — otherwise `nightme kill` would silently
// refuse to clean up agents whose binary has a long name.
//
// The fixture is a symlink, not a copy: macOS kills copies of signed
// platform binaries, which would leave a zombie and test nothing.
func TestKill_VerifyPIDOwnerLongBinaryName(t *testing.T) {
	link := filepath.Join(t.TempDir(), "nightme-long-agent-runner")
	if len(filepath.Base(link)) <= commTruncLen {
		t.Fatalf("fixture name %q is too short to exercise truncation", filepath.Base(link))
	}
	if err := os.Symlink("/bin/sleep", link); err != nil {
		t.Skipf("cannot symlink /bin/sleep: %v", err)
	}

	cmd := exec.Command(link, "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot exec %s: %v", link, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	actual, ok := verifyPIDOwner(cmd.Process.Pid, link)
	if !ok {
		t.Errorf("verifyPIDOwner(%q) = %q, false — a live process must verify as itself", link, actual)
	}
}

// TestKill_VerifyPIDOwnerZombie is the regression test for the case
// the identity check nearly broke: a zombie agent child, which is
// exactly what a crashed daemon leaves behind. `ps` reports
// "<defunct>" and the identity is unrecoverable, so verification
// must fail open — otherwise the sweep skips it and the registry
// entry stays "running" forever, defeating the command's purpose.
func TestKill_VerifyPIDOwnerZombie(t *testing.T) {
	// A child that exits while we never Wait() is a zombie.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh not available: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })

	deadline := time.Now().Add(3 * time.Second)
	for {
		if out, err := processCommand(pid); err == nil && out == defunctComm {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("child never observed in the zombie state on this platform")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, ok := verifyPIDOwner(pid, "claude"); !ok {
		t.Errorf("a zombie must not block the sweep — its entry would stay running forever")
	}
}
