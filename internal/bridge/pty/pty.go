// Package pty wraps aymanbagabas/go-pty behind a minimal Transport
// interface so the rest of nightme does not depend on the underlying
// library. See docs/feat/F-04-pty-simulation.md and F-19-cli-bridge.md.
package pty

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	gopty "github.com/aymanbagabas/go-pty"

	"github.com/cnlangzi/nightme/internal/proc"
)

// Transport is a transparent byte pipe to a child process running under
// a pseudo-terminal. The caller writes user input and reads CLI
// output as if it were a plain io.ReadWriteCloser; the PTY provides
// the line-buffering / signal / tty semantics the CLI needs.
type Transport interface {
	io.ReadWriteCloser
	PID() int
	Setsize(cols, rows int) error
	// Signal sends a signal to the child process. Tests can
	// stub it as a no-op; production uses go-pty's underlying
	// *os.Process.Signal. Note: this exists for the ACP bridge
	// (which sends SIGINT as its portable Ctrl-C); pty.Agent's
	// own Stop returns ErrNotSupported because Agent-level stop
	// should not be confused with raw signal delivery.
	Signal(os.Signal) error
}

// ptyTransport is the production implementation backed by
// aymanbagabas/go-pty. It owns one PTY master fd and one child *Cmd.
type ptyTransport struct {
	ptmx      gopty.Pty
	cmd       *gopty.Cmd
	closeOnce sync.Once
	closeErr  error
	// closed is closed when the transport has been torn down, by
	// whichever of Close() / the context watcher got there first.
	// The watcher selects on it so a transport closed while its
	// context is still alive does not strand the watcher goroutine.
	closed chan struct{}
}

// NewTransport spawns command with args inside a PTY rooted at
// workspace.
// env is appended to os.Environ() for the child process. cols / rows
// set the initial TTY size. Returns a Transport for byte I/O plus a
// PID() helper.
//
// ctx governs the child's lifetime: cancelling it terminates the
// child process group and reaps it, exactly as Close() does. A
// context that is already cancelled refuses to start the child at
// all — the daemon's shutdown must not produce a new agent process.
func NewTransport(ctx context.Context, workspace, command string, args []string, env []string, cols, rows int) (Transport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pty: not starting %s: %w", command, err)
	}

	ptmx, err := gopty.New()
	if err != nil {
		return nil, err
	}

	// Resize before start so the child sees the correct size on its
	// first read of TIOCGWINSZ (e.g. Claude Code / Codex render the
	// banner at startup).
	if err := ptmx.Resize(cols, rows); err != nil {
		_ = ptmx.Close()
		return nil, err
	}
	if err := setRaw(ptmx); err != nil {
		_ = ptmx.Close()
		return nil, err
	}

	cmd := ptmx.Command(command, args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), env...)
	// Apply CREATE_NO_WINDOW via proc.HideWindow so PTY-spawned
	// children on Windows don't pop a visible console window.
	// gopty.Cmd is a sibling struct to *exec.Cmd (same SysProcAttr
	// field, different concrete type) — HideWindow keys on the
	// SysProcAttr field type so it works for both shapes.
	cmd.SysProcAttr = proc.HideWindow(cmd.SysProcAttr)

	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		return nil, err
	}

	b := &ptyTransport{ptmx: ptmx, cmd: cmd, closed: make(chan struct{})}
	go b.watch(ctx)
	return b, nil
}

// watch tears the child down when ctx is cancelled.
//
// It shares Close()'s once-guard, so the two paths can race safely:
// whichever gets there first terminates the child, the other returns
// without doing anything. The b.closed arm keeps the goroutine from
// outliving a transport that was closed while its context was still
// alive.
func (b *ptyTransport) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-b.closed:
		return
	}
	b.closeOnce.Do(func() {
		defer close(b.closed)
		b.terminateChild()
		if b.ptmx != nil {
			b.closeErr = b.ptmx.Close()
		}
	})
}

// Read forwards a read from the PTY master fd.
func (b *ptyTransport) Read(p []byte) (int, error) { return b.ptmx.Read(p) }

// Write forwards a write to the PTY master fd (goes to child stdin).
func (b *ptyTransport) Write(p []byte) (int, error) { return b.ptmx.Write(p) }

// Close stops the child and closes the PTY master. Closing the master
// alone leaves the CLI running: cursor-agent ignores the SIGHUP that
// a dropped controlling terminal delivers, and its worker stays in
// the same process group. The child is the session leader (go-pty
// sets Setsid), so the whole group is signalled, then reaped.
// Idempotent.
func (b *ptyTransport) Close() error {
	b.closeOnce.Do(func() {
		defer close(b.closed)
		b.terminateChild()
		if b.ptmx != nil {
			b.closeErr = b.ptmx.Close()
		}
	})
	return b.closeErr
}

// PID returns the child process PID, or 0 if the command has not been
// started.
func (b *ptyTransport) PID() int {
	if b.cmd == nil || b.cmd.Process == nil {
		return 0
	}
	return b.cmd.Process.Pid
}

// Setsize resizes the PTY. cols / rows follow the terminal convention
// (width, height). go-pty's Resize uses the same order, so this is a
// straight pass-through.
func (b *ptyTransport) Setsize(cols, rows int) error {
	return b.ptmx.Resize(cols, rows)
}

// Signal sends a signal to the child process. The shell convention
// is SIGINT (Ctrl-C); the caller picks the signal. We pass through
// to the underlying gopty.Cmd.Process, which forwards to the
// child over the controlling terminal so the child sees the signal
// as if a user pressed the key. Used by the ACP bridge (pty.Agent
// itself returns ErrNotSupported from Stop — see agent.go).
func (b *ptyTransport) Signal(sig os.Signal) error {
	if b.cmd == nil || b.cmd.Process == nil {
		return fmt.Errorf("pty: no command")
	}
	return b.cmd.Process.Signal(sig)
}

// Compile-time guarantee that *ptyTransport satisfies Transport.
var _ Transport = (*ptyTransport)(nil)
