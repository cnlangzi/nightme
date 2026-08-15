//go:build !windows

// Unix-only regression tests for the claudecode Stop path.
//
// The cross-platform test (TestStop_NoCmdReturnsErrNotSupported)
// lives in agent_interrupt_test.go. The tests below need real
// subprocesses + `syscall.SysProcAttr.Setpgid` + signal(0)
// liveness checks, all of which are Unix-only.
//
// See agent_interrupt_test.go for the protocol-level docstring
// (control_request{interrupt} stdin wire format, the reverse-
// engineering trail, and the per-bridge design rationale).
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestStop_WritesControlRequestInterruptOnStdin is the
// central fix-stop regression: with a live stdin pipe,
// Stop() must write the documented control_request payload
// — NOT SIGINT.
//
// We model the bridge's stdin pipe manually (parent holds
// the write end; a `sleep` child holds the read end). The
// test reads back exactly the bytes Stop() wrote and parses
// them as JSON to assert on the wire shape.
func TestStop_WritesControlRequestInterruptOnStdin(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	cmd := exec.Command("sleep", "30")
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
		}
	}()

	d := newDriverForStdinTest(w, cmd)

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Read back the line written by Stop().
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := readLine(r)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if got != interruptPayload {
		t.Errorf("stdin payload = %q; want %q", got, interruptPayload)
	}

	// Parse as JSON and assert on structure.
	var msg map[string]any
	if err := json.Unmarshal([]byte(got), &msg); err != nil {
		t.Fatalf("stdin payload is not JSON: %v", err)
	}
	if msg["type"] != "control_request" {
		t.Errorf("stdin type = %v; want control_request", msg["type"])
	}
	req, _ := msg["request"].(map[string]any)
	if req == nil || req["subtype"] != "interrupt" {
		t.Errorf("request.subtype = %v; want interrupt", req)
	}
}

// TestStop_DoesNotSendSIGINT pins the negative: with stdin
// wired correctly, Stop() must NOT deliver SIGINT on the
// happy path. SIGINT is only the last-resort fallback when
// the stdin pipe is broken (e.g., the CLI exited
// unexpectedly).
//
// We verify by checking the child process stays alive for
// ≥1s after Stop() — SIGINT to the process group would
// terminate sleep within ~50ms.
func TestStop_DoesNotSendSIGINT(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	cmd := exec.Command("sleep", "30")
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("sleep start: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	d := newDriverForStdinTest(w, cmd)

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Drain the line Stop wrote so the pipe doesn't back up.
	_ = r.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = readLine(r)

	// Verify child is still alive.
	time.Sleep(1 * time.Second)
	if cmd.Process == nil {
		t.Fatal("cmd.Process is nil")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("child exited after Stop (signal 0 → %v); Stop() "+
			"must not deliver SIGINT on the happy path", err)
	}
}

// TestStop_FallsBackToSIGINTWhenStdinBroken covers the
// fallback: the stdin pipe is broken (e.g., the CLI exited
// unexpectedly and the parent can't write anymore). Stop()
// must detect the writeLine failure and fall back to SIGINT
// to surface a graceful shutdown via the legacy path.
func TestStop_FallsBackToSIGINTWhenStdinBroken(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// Close the write end immediately so every subsequent
	// write returns EPIPE.
	w.Close()
	defer r.Close()

	cmd := exec.Command("sleep", "30")
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("sleep start: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	// Wire stdin to the BROKEN write end — every Write
	// will fail with EPIPE.
	d := newDriverForStdinTest(w, cmd)

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// SIGINT fallback should fire and the child should
	// exit via signal within a short window.
	select {
	case <-waitDone:
		// Child exited (via SIGINT). We don't strictly
		// assert exit reason here — the point is that
		// Stop did NOT silently swallow the broken-pipe
		// error.
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit within 5s; SIGINT fallback did not fire")
	}
}

// ─── helpers ───

// newDriverForStdinTest wires a driver with a real stdin
// pipe (w is the write end; cmd.Stdin takes the read end)
// and a real *exec.Cmd (cmd). The bufio.Writer mirrors
// what claudecode.NewDriver does in production.
func newDriverForStdinTest(w io.WriteCloser, cmd *exec.Cmd) *driver {
	d := &driver{
		closed: make(chan struct{}),
		cmd:    cmd,
	}
	d.stdin = bufio.NewWriter(w)
	return d
}

// readLine reads one LF-delimited line from r. Returns the
// bytes before the LF (excluding the LF itself).
func readLine(r io.Reader) (string, error) {
	var buf bytes.Buffer
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 0 {
			if err == io.EOF {
				return buf.String(), nil
			}
			return buf.String(), err
		}
		if one[0] == '\n' {
			return buf.String(), nil
		}
		buf.WriteByte(one[0])
	}
}