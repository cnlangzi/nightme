//go:build !windows

package procsweep

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// TestKill_TerminatesRunningChild spawns a real child and asserts
// the sweep's termination path kills it.
//
// Ground truth for "the child exited" is Wait() returning, not
// kill(pid, 0): after SIGTERM the process stays visible as a zombie
// until its parent reaps it.
func TestKill_TerminatesRunningChild(t *testing.T) {
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

	if err := Kill(pid, false); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case <-waitErr:
		// Wait returning proves the child exited; the error itself
		// is non-nil because it died from a signal.
	case <-time.After(5 * time.Second):
		t.Fatalf("child pid %d still alive 5s after kill", pid)
	}
}

// TestKill_ForceKillsRunningChild exercises the force path (SIGKILL
// with no grace window) against a child that ignores SIGTERM.
func TestKill_ForceKillsRunningChild(t *testing.T) {
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

	if err := Kill(cmd.Process.Pid, true); err != nil {
		t.Fatalf("Kill(force): %v", err)
	}

	select {
	case <-waitErr:
	case <-time.After(5 * time.Second):
		t.Fatalf("child pid %d survived the forced kill", cmd.Process.Pid)
	}
}

// TestKill_AlreadyExitedIsSuccess asserts a child that died before
// the sweep reached it is reported as gone, not as a failure.
func TestKill_AlreadyExitedIsSuccess(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh not available: %v", err)
	}
	pid := cmd.Process.Pid
	// Wait reaps the child, so the PID is fully gone (no zombie).
	_ = cmd.Wait()

	err := Kill(pid, false)
	if err == nil {
		t.Fatalf("Kill on a dead pid = nil, want the gone sentinel")
	}
	if !IsProcessGone(err) {
		t.Errorf("IsProcessGone(%v) = false, want true", err)
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

	if err := Kill(cmd.Process.Pid, false); err != nil {
		t.Fatalf("Kill: %v", err)
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

// TestVerifyOwner_RefusesRecycledPID is the safety net for stale
// registry entries. agent_sessions.json can hold PIDs from days ago
// (entries survive a daemon crash) and the OS recycles PIDs, so a
// naive sweep would SIGTERM whatever process happens to hold that
// number now — the user's editor, a build, anything. The entry claims
// "claude", the live PID is /bin/sleep: the sweep must refuse and say
// so.
func TestVerifyOwner_RefusesRecycledPID(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	owner := VerifyOwner(cmd.Process.Pid, "claude")
	if owner.Match {
		t.Errorf("VerifyOwner = %q, match — a recycled PID must be refused", owner.Binary)
	}
	// The innocent process must still be alive.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("unrelated pid %d was signalled: %v", cmd.Process.Pid, err)
	}
}

// TestVerifyOwner_FailsOpen pins the fail-open policy: when we
// cannot obtain a contradiction, the sweep proceeds. Refusing on
// "cannot verify" would make the cleanup useless on any system where
// the probe doesn't work.
func TestVerifyOwner_FailsOpen(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	if owner := VerifyOwner(cmd.Process.Pid, ""); !owner.Match {
		t.Errorf("unknown agent command must not block the kill")
	}
	// A dead PID cannot be probed either; Kill reports it as
	// "already exited", so verification must not swallow it first.
	if owner := VerifyOwner(1<<30, "claude"); !owner.Match {
		t.Errorf("unprobeable pid must not block the kill")
	}
	// Positive match still passes — and is reported as verified, so
	// the caller may signal the process group.
	owner := VerifyOwner(cmd.Process.Pid, "/usr/bin/sleep")
	if !owner.Match || !owner.Verified {
		t.Errorf("basename match must verify (ps comm may be a bare name): %+v", owner)
	}
}

// TestVerifyOwner_LongBinaryName pins the platform truth the
// PID-identity check rests on: what `ps -o comm=` reports for a long
// executable name.
//
// Verified on darwin 24: the full exec path, no truncation. Linux
// truncates comm to TASK_COMM_LEN-1 = 15 characters. Either way the
// contract must hold — a live process started from this binary
// verifies as itself — otherwise the sweep would silently refuse to
// clean up agents whose binary has a long name.
//
// The fixture is a symlink, not a copy: macOS kills copies of signed
// platform binaries, which would leave a zombie and test nothing.
func TestVerifyOwner_LongBinaryName(t *testing.T) {
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

	owner := VerifyOwner(cmd.Process.Pid, link)
	if !owner.Match || !owner.Verified {
		t.Errorf("VerifyOwner(%q) = %+v — a live process must verify as itself", link, owner)
	}
}

// TestVerifyOwner_Zombie is the regression test for the case the
// identity check nearly broke: a zombie agent child, which is exactly
// what a crashed daemon leaves behind. The identity is unrecoverable
// from ps, so verification must fail open — otherwise the sweep skips
// it and the registry entry stays "running" forever, defeating the
// cleanup's purpose.
//
// The zombie marker is platform-specific: Linux prints
// "<name> <defunct>", macOS prints "<defunct>". Matching only the
// macOS spelling made this test skip on Linux, which is how a
// fail-open that never fired survived.
func TestVerifyOwner_Zombie(t *testing.T) {
	// A child that exits while we never Wait() is a zombie.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh not available: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })

	deadline := time.Now().Add(3 * time.Second)
	for {
		if out, err := processCommand(pid); err == nil && isDefunct(out) {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("child never observed in the zombie state on this platform")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if owner := VerifyOwner(pid, "claude"); !owner.Match {
		t.Errorf("a zombie must not block the sweep — its entry would stay running forever: %+v", owner)
	}
}

// TestIsDefunct pins the platform spellings of the zombie marker.
// Linux keeps the command name and appends the marker ("sh
// <defunct>"); macOS reports the marker alone ("<defunct>").
func TestIsDefunct(t *testing.T) {
	cases := []struct {
		comm string
		want bool
	}{
		{"<defunct>", true},
		{"sh <defunct>", true},
		{"python3 <defunct>", true},
		{"claude", false},
		{"<defunct>sh", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isDefunct(c.comm); got != c.want {
			t.Errorf("isDefunct(%q) = %v, want %v", c.comm, got, c.want)
		}
	}
}

// TestKillGroup_TerminatesTheWholeGroup is the reason the sweep
// signals the group and not just the recorded PID: an agent CLI
// leads its own process group, and the tool subprocesses it spawned
// (a shell running a command, a language server, …) live in that
// group. Killing only the CLI would orphan them.
//
// The fixture mirrors the real spawn recipe: proc.NewWith sets
// Setsid, so the child is a session — and therefore process-group —
// leader.
//
// The grandchild is the probe. It is not our child, so when the
// group signal reaches it, it dies and init reaps it: its pid
// disappears entirely. The leader is our own child, so it stays a
// zombie until we reap it — that is the parent's business, not the
// group's.
func TestKillGroup_TerminatesTheWholeGroup(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "grandchild.pid")

	cmd := proc.NewWith(context.Background(), proc.Options{HideWindow: true},
		"/bin/sh", "-c", "sleep 60 & echo $! > "+sentinel+"; wait")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sh not available: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	// Wait for the grandchild to report its pid.
	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(sentinel)
		if err == nil {
			if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &grandchild); err == nil && grandchild > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the fixture never reported its grandchild pid")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The kill itself may have to degrade: Darwin can refuse the
	// group SIGKILL once the group leader is gone, and the fallback
	// to the leader alone then leaves the grandchild to the earlier
	// SIGTERM. Either way the grandchild has to be gone.
	if err := KillGroup(pid, false); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}

	// The grandchild only proves the group kill when the shell left
	// it in the leader's group. A shell that gives a background job
	// its own group (job control) puts it out of reach, and the
	// group kill is then not supposed to reach it.
	if pgid, err := syscall.Getpgid(grandchild); err == nil && pgid == pid {
		deadline = time.Now().Add(10 * time.Second)
		for {
			if syscall.Kill(grandchild, syscall.Signal(0)) != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("grandchild pid %d survived the group kill", grandchild)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Reap the leader so the test leaves no zombie behind.
	_ = cmd.Wait()
	reaped = true
}

// TestKillGroup_FallsBackToTheProcess is the safety property: a PID
// that does not lead its own group is signalled on its own, so an
// unrelated process group is never taken down.
func TestKillGroup_FallsBackToTheProcess(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Skipf("Getpgid: %v", err)
	}
	if pgid == cmd.Process.Pid {
		t.Skip("the fixture happens to lead its own group")
	}

	if err := KillGroup(cmd.Process.Pid, false); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
}
