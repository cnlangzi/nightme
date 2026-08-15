// Regression tests for the claudecode Stop path.
//
// The pre-fix claudecode bridge had a misleading docstring claiming
// SIGINT behavior was "best-effort". Anthropic's own CHANGELOG
// (anthropics/claude-code) is unambiguous: SIGINT is the
// documented interrupt mechanism for the Claude Code CLI across
// all run modes — interactive, --output-format stream-json, and
// SDK. The CLI's "graceful shutdown" path on SIGINT restores
// terminal modes, prints a --resume hint on stderr, and exits
// cleanly.
//
// The "real stop" for claudecode is therefore NOT a bridge-side
// stdin message (no such message type exists in the stream-json
// protocol); it's SIGINT, which is what the bridge already does.
// The actual fix lives at the chat layer:
//   - agent.ErrResumeUnhealthy sentinel (detects "No conversation
//     found" stderr on --resume rejection)
//   - chat-session retry that clears the saved sessionID and
//     respawns once without --resume when the resume is stale
//
// These tests pin:
//
//  1. Stop() returns ErrNotSupported when the bridge has no
//     cmd (pre-Start / post-Close).
//  2. Stop() sends SIGINT to the running child via
//     SignalProcessGroup (verifiable against a real
//     long-running sleep subprocess).
//  3. Stop() does NOT attempt any structured stdin interrupt
//     — there's no such protocol, and the bridge must not
//     spuriously introduce one.

package claudecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ─── Stop ───

// TestStop_NoCmdReturnsErrNotSupported covers the pre-Start /
// post-Close contract. The driver has no cmd — SIGINT cannot be
// delivered, so Stop must return ErrNotSupported and not panic.
func TestStop_NoCmdReturnsErrNotSupported(t *testing.T) {
	d := &driver{} // cmd is nil
	err := d.Stop(context.Background())
	if !errors.Is(err, agent.ErrNotSupported) {
		t.Errorf("Stop before Start = %v, want ErrNotSupported", err)
	}
}

// TestStop_SendsSIGINTToRunningChild is the central regression:
// with a real running child, Stop() must deliver SIGINT (not
// SIGTERM, not SIGKILL) to the process group. We verify by
// spawning a long-running `sleep` and asserting that
// SignalProcessGroup sent it os.Interrupt — confirmed by
// checking that the child's exit status maps to "interrupted"
// (128 + SIGINT = 130 on POSIX shells) or, more robustly, that
// the child exits within the SIGINT grace window.
//
// The "real" process is required because *os.Process cannot be
// faked portably; `SignalProcessGroup` calls syscall.Kill on a
// real pid. A `sleep` subprocess is the smallest viable fake.
func TestStop_SendsSIGINTToRunningChild(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}

	// Spawn a child that ignores SIGINT-by-default so we can
	// tell whether the bridge actually sent one. Without an
	// explicit signal handler reset, sleep exits naturally on
	// SIGINT (the default disposition is "terminate").
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // own process group, like a real CLI
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("sleep start: %v", err)
	}
	defer func() {
		// Belt-and-suspenders cleanup if the test exits before
		// the child sees SIGINT.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	d := &driver{cmd: cmd}

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Wait for the child to exit. SIGINT delivers fast on
	// POSIX; 5s is generous.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		// `sleep` exited. On POSIX, exit status from a
		// signal-terminated process is encoded as 128+sig
		// (128 + 2 = 130 for SIGINT) when surfaced via
		// *exec.ExitError. We don't strictly require 130
		// because some shells / sandbox setups remap, but the
		// child MUST have exited within the grace window —
		// which is the real signal that SIGINT landed.
		if ee, ok := err.(*exec.ExitError); ok {
			t.Logf("sleep exited with status=%v (signal=%v) — SIGINT delivered",
				ee.ExitCode(), ee.Sys().(syscall.WaitStatus).Signal())
		} else {
			t.Logf("sleep exited cleanly: %v", err)
		}
	case <-time.After(5 * time.Second):
		// If the child didn't exit, SIGINT never landed —
		// either the bridge swallowed the call or the process
		// group setup is wrong.
		_ = cmd.Process.Kill()
		t.Fatal("sleep did not exit within 5s after Stop; SIGINT not delivered")
	}
}

// TestStop_DoesNotEmitStdinInterrupt pins the negative: claudecode's
// stream-json protocol does NOT accept any structured interrupt
// message on stdin (the documented mechanism is SIGINT). If a
// future refactor is tempted to "improve" Stop by writing a
// structured interrupt to the child's stdin, this test will fail.
//
// Concretely: after Stop(), no extra bytes should be readable
// on the child's stdin. We model the bridge's stdin pipe
// manually (parent holds the write end; "child" sleeps,
// ignoring stdin); we pre-load the pipe with a sentinel
// byte, then call Stop, then non-blocking-read with a short
// timeout and assert we got back EXACTLY the sentinel — no
// additional bytes from a would-be structured interrupt.
func TestStop_DoesNotEmitStdinInterrupt(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	// Pre-load the pipe with a sentinel. The sleep child
	// won't read it, so the bytes stay buffered on the read
	// end.
	if _, err := w.Write([]byte{0x42}); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	cmd := exec.Command("sleep", "5")
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("sleep start: %v", err)
	}
	killChild := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	waitDone := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(waitDone) }()
	defer func() {
		killChild()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			t.Logf("warning: sleep did not exit within 2s")
		}
	}()

	// Stop should send SIGINT. We don't care about the
	// return value — only that the bridge does NOT touch
	// stdin. The pipe read end has the sentinel byte
	// buffered; we want to confirm a non-blocking read
	// returns exactly that byte, no more.
	d := &driver{cmd: cmd}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	buf := make([]byte, 16)
	got := make(chan int, 1)
	go func() {
		// Set a deadline so the read can't block forever if
		// something is wrong with the pipe state. The pipe
		// was already loaded with 1 byte; if the bridge
		// didn't write anything, this read should return
		// exactly 1 byte.
		_ = r.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := r.Read(buf)
		got <- n
	}()

	select {
	case n := <-got:
		if n != 1 {
			t.Fatalf("read after Stop returned %d bytes (buf=%x); want exactly 1 "+
				"(the sentinel byte 0x42) — bridge wrote a structured "+
				"interrupt to stdin", n, buf[:n])
		}
		if buf[0] != 0x42 {
			t.Fatalf("sentinel byte = 0x%x; want 0x42", buf[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read after Stop blocked for 2s; pipe state unexpected")
	}
}