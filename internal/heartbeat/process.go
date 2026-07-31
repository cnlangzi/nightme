package heartbeat

import (
	"io"
	"os"
	"os/exec"
	"sync/atomic"
)

// OSProcessProbe is the production ProcessProbe backed by an *exec.Cmd and
// a stdout io.Reader. It uses two facts to determine liveness:
//
//  1. signal 0 succeeds → process is still in the process table
//  2. stdout pipe is not EOF → process is still writing (or hasn't closed
//     its stdout yet)
//
// Neither fact is time-based. Both are determined at call time.
type OSProcessProbe struct {
	cmd    *exec.Cmd
	stdout io.Reader

	eofFlag atomic.Bool // set true when stdout io.Copy finishes (EOF reached)
}

// NewOSProcessProbe wraps an *exec.Cmd + its stdout pipe. The caller is
// responsible for starting the command before passing it in.
//
// NewOSProcessProbe spawns a background goroutine that drains stdout into
// io.Discard and flips the EOF flag once io.EOF is returned. This is the
// standard pattern for "monitor pipe EOF" in Go — there's no portable
// non-blocking "is this pipe still open?" syscall.
func NewOSProcessProbe(cmd *exec.Cmd, stdout io.Reader) *OSProcessProbe {
	p := &OSProcessProbe{
		cmd:    cmd,
		stdout: stdout,
	}
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
		p.eofFlag.Store(true)
	}()
	return p
}

// Signal forwards to the underlying process. signal 0 (the heartbeat
// liveness probe) is a no-op at the OS level but returns an error if the
// process has been reaped.
func (p *OSProcessProbe) Signal(sig os.Signal) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return os.ErrProcessDone
	}
	return p.cmd.Process.Signal(sig)
}

// StdoutEOF reports whether the stdout pipe has been closed. This is
// set by the background drain goroutine once io.Copy returns.
func (p *OSProcessProbe) StdoutEOF() bool {
	return p.eofFlag.Load()
}

// ExitCode returns the process exit code, or -1 if the process has not
// exited yet. Useful for error messages ("exit code: 137" = OOM killed).
func (p *OSProcessProbe) ExitCode() int {
	if p.cmd == nil || p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}
