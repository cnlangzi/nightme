// Package pty wraps aymanbagabas/go-pty behind a minimal Transport
// interface so the rest of nightme does not depend on the underlying
// library. See docs/feat/F-04-pty-simulation.md and F-19-cli-bridge.md.
package pty

import (
	"io"
	"os"

	gopty "github.com/aymanbagabas/go-pty"
)

// Transport is a transparent byte pipe to a child process running under
// a pseudo-terminal. The caller writes user input and reads CLI
// output as if it were a plain io.ReadWriteCloser; the PTY provides
// the line-buffering / signal / tty semantics the CLI needs.
type Transport interface {
	io.ReadWriteCloser
	PID() int
	Setsize(cols, rows int) error
}

// ptyTransport is the production implementation backed by
// aymanbagabas/go-pty. It owns one PTY master fd and one child *Cmd.
type ptyTransport struct {
	ptmx gopty.Pty
	cmd  *gopty.Cmd
}

// New spawns command with args inside a PTY rooted at workspace.
// env is appended to os.Environ() for the child process. cols / rows
// set the initial TTY size. Returns a Transport for byte I/O plus a
// PID() helper.
func NewTransport(workspace, command string, args []string, env []string, cols, rows int) (Transport, error) {
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

	cmd := ptmx.Command(command, args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), env...)

	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		return nil, err
	}

	return &ptyTransport{ptmx: ptmx, cmd: cmd}, nil
}

// Read forwards a read from the PTY master fd.
func (b *ptyTransport) Read(p []byte) (int, error) { return b.ptmx.Read(p) }

// Write forwards a write to the PTY master fd (goes to child stdin).
func (b *ptyTransport) Write(p []byte) (int, error) { return b.ptmx.Write(p) }

// Close closes the PTY master fd. The child process is not waited on
// here; callers that need an exit code should call cmd.Wait() via the
// underlying gopty.Cmd. In practice, the SessionManager handles
// reaping via the AgentSession lifecycle.
func (b *ptyTransport) Close() error { return b.ptmx.Close() }

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

// Compile-time guarantee that *ptyTransport satisfies Transport.
var _ Transport = (*ptyTransport)(nil)
