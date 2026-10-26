//go:build windows

package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
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
	if got := processImageName(cmd.Process.Pid); got != image {
		t.Errorf("unrelated pid %d was terminated: image = %q, want %q", cmd.Process.Pid, got, image)
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
	cmd, image := startPing(t)
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	if _, ok := verifyPIDOwner(cmd.Process.Pid, ""); !ok {
		t.Errorf("unknown agent command must not block the kill")
	}
	// A dead PID cannot be probed either; killProcess reports it as
	// "already exited", so verification must not swallow it first.
	if _, ok := verifyPIDOwner(1<<30, "claude"); !ok {
		t.Errorf("unprobeable pid must not block the kill")
	}
	// Positive match still passes — the image name without its
	// ".exe" suffix is what a configured bare command looks like.
	if _, ok := verifyPIDOwner(cmd.Process.Pid, stripExeSuffix(image)); !ok {
		t.Errorf("matching image name %q must verify", image)
	}
}

// TestKill_VerifyPIDOwnerHostInterpreter pins the exemption that
// keeps `nightme kill` usable on Windows: an agent installed with
// npm is a .cmd shim, so the PID we recorded belongs to cmd.exe.
// The image name cannot say WHICH agent the session runs, so the
// check must fail open.
func TestKill_VerifyPIDOwnerHostInterpreter(t *testing.T) {
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

	if _, ok := verifyPIDOwner(pid, "claude"); !ok {
		t.Errorf("a host interpreter must not block the sweep — every npm-installed agent runs under one")
	}
}

// TestKill_VerifyPIDOwnerForeignInterpreter is the other side of that
// exemption: an interpreter that is NOT the host for this command
// proves nothing about the agent, so the check must refuse. A
// recycled PID landing on the user's node.exe or powershell.exe is
// exactly the case the identity check exists to catch.
func TestKill_VerifyPIDOwnerForeignInterpreter(t *testing.T) {
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
	if _, ok := verifyPIDOwner(pid, "claude"); ok {
		t.Errorf("an interpreter that cannot host this agent must not fail open")
	}
}

// TestKill_HostInterpreterFor pins the command-to-host mapping. It
// mirrors internal/proc's launchOnWindowsWith, which decides the host
// by the extension of the file it runs.
func TestKill_HostInterpreterFor(t *testing.T) {
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

// TestKill_VerifyPIDOwnerAbsolutePath pins the base-name rule for a
// configured absolute path: the command may be spelled as
// "C:\Program Files\claude\claude.exe" while the OS only reports
// the image name "claude.exe".
func TestKill_VerifyPIDOwnerAbsolutePath(t *testing.T) {
	const want = `C:\Program Files\claude\claude.exe`
	if !sameCommandFold("claude.exe", want) {
		t.Errorf("a configured absolute path must match its image name")
	}
	if sameCommandFold("codex.exe", want) {
		t.Errorf("a different image name must not match %q", want)
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
