//go:build windows

package procsweep

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
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

	image := processImageName(cmd.Process.Pid)
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

// TestVerifyOwner_RefusesRecycledPID is the safety net for stale
// registry entries. Windows hands a freed PID straight back into the
// pool, so a naive sweep would TerminateProcess whatever owns that
// number now — the user's editor, a build, another agent. The entry
// claims "claude" while the live PID belongs to something else: the
// sweep must refuse and name what it found.
func TestVerifyOwner_RefusesRecycledPID(t *testing.T) {
	cmd, image := startPing(t)
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	owner := VerifyOwner(cmd.Process.Pid, "claude")
	if owner.Match {
		t.Errorf("VerifyOwner = %q, match — a recycled PID must be refused", owner.Binary)
	}

	// The innocent process must still be alive.
	if got := processImageName(cmd.Process.Pid); got != image {
		t.Errorf("unrelated pid %d was terminated: image = %q, want %q", cmd.Process.Pid, got, image)
	}
}

// TestVerifyOwner_FailsOpen pins the fail-open policy: when we cannot
// obtain a contradiction, the sweep proceeds. Refusing on "cannot
// verify" would make the cleanup useless on any system where the
// probe doesn't work.
func TestVerifyOwner_FailsOpen(t *testing.T) {
	cmd, image := startPing(t)
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	if owner := VerifyOwner(cmd.Process.Pid, ""); !owner.Match {
		t.Errorf("unknown agent command must not block the kill")
	}
	// A dead PID cannot be probed either; Kill reports it as
	// "already exited", so verification must not swallow it first.
	if owner := VerifyOwner(1<<30, "claude"); !owner.Match {
		t.Errorf("unprobeable pid must not block the kill")
	}
	// Positive match still passes — the image name without its
	// ".exe" suffix is what a configured bare command looks like.
	owner := VerifyOwner(cmd.Process.Pid, stripExeSuffix(image))
	if !owner.Match || !owner.Verified {
		t.Errorf("matching image name %q must verify: %+v", image, owner)
	}
}

// TestVerifyOwner_HostInterpreter pins the exemption that keeps the
// sweep usable on Windows: an agent installed with npm is a .cmd shim,
// so the PID we recorded belongs to cmd.exe. The image name cannot
// say WHICH agent the session runs, so the check must fail open.
func TestVerifyOwner_HostInterpreter(t *testing.T) {
	cmd := proc.New(context.Background(), proc.ComSpecOrDefault(), "/c", "ping", "-t", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cmd.exe not available: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	image := processImageName(pid)
	if image == "" {
		t.Skipf("no image name for pid %d", pid)
	}
	if !strings.EqualFold(image, "cmd.exe") {
		t.Skipf("fixture image %q is not cmd.exe", image)
	}

	if owner := VerifyOwner(pid, "claude"); !owner.Match {
		t.Errorf("a host interpreter must not block the sweep — every npm-installed agent runs under one")
	}
}

// TestVerifyOwner_ForeignInterpreter is the other side of that
// exemption: an interpreter that is NOT the host for this command
// proves nothing about the agent, so the check must refuse. A
// recycled PID landing on the user's node.exe or powershell.exe is
// exactly the case the identity check exists to catch.
func TestVerifyOwner_ForeignInterpreter(t *testing.T) {
	cmd := proc.New(context.Background(), "powershell.exe", "-NoProfile", "-Command", "Start-Sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("powershell.exe not available: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	image := processImageName(pid)
	if image == "" {
		t.Skipf("no image name for pid %d", pid)
	}
	if !strings.EqualFold(image, "powershell.exe") {
		t.Skipf("fixture image %q is not powershell.exe", image)
	}

	// "claude" is a bare command name, so its host is cmd.exe —
	// powershell.exe cannot be the agent's host.
	if owner := VerifyOwner(pid, "claude"); owner.Match {
		t.Errorf("an interpreter that cannot host this agent must not fail open")
	}
}

// TestHostInterpreterFor pins the command-to-host mapping. It
// mirrors internal/proc's launchOnWindowsWith, which decides the host
// by the extension of the file it runs.
func TestHostInterpreterFor(t *testing.T) {
	cases := []struct {
		want string
		host string
	}{
		// A bare command name: npm installs the agent as a .cmd
		// shim, so cmd.exe is the only host it can have.
		{"claude", "cmd.exe"},
		{"codex", "cmd.exe"},
		// Explicit shim paths name their host.
		{`C:\Users\x\AppData\Roaming\npm\claude.cmd`, "cmd.exe"},
		{`C:\Users\x\AppData\Roaming\npm\claude.bat`, "cmd.exe"},
		{`C:\agents\run.ps1`, "powershell.exe"},
		{`C:\agents\run.js`, "node.exe"},
		// A native binary IS the spawned process — no host, so no
		// exemption: a recycled PID on cmd.exe must be caught.
		{`C:\Program Files\claude\claude.exe`, ""},
		{"claude.exe", ""},
	}
	for _, c := range cases {
		if got := hostInterpreterFor(c.want); got != c.host {
			t.Errorf("hostInterpreterFor(%q) = %q, want %q", c.want, got, c.host)
		}
	}
}

// TestVerifyOwner_AbsolutePath pins the base-name rule for a
// configured absolute path: the command may be spelled as
// "C:\Program Files\claude\claude.exe" while the OS only reports the
// image name "claude.exe".
func TestVerifyOwner_AbsolutePath(t *testing.T) {
	const want = `C:\Program Files\claude\claude.exe`
	if !sameCommandFold("claude.exe", want) {
		t.Errorf("a configured absolute path must match its image name")
	}
	if sameCommandFold("codex.exe", want) {
		t.Errorf("a different image name must not match %q", want)
	}
}

// TestKill_TerminatesRunningChild asserts a real child is
// terminated.
func TestKill_TerminatesRunningChild(t *testing.T) {
	cmd, _ := startPing(t)
	// The test reaps the child itself below; this only covers the
	// path where the sweep fails before the child dies.
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	if err := Kill(cmd.Process.Pid, false); err != nil {
		t.Fatalf("Kill: %v", err)
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
}
