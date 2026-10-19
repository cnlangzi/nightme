//go:build !windows

package pty

import (
	"log/slog"
	"os"
	"syscall"
	"time"
)

// childExitGrace is how long Close waits after SIGTERM before
// SIGKILL. A cursor-agent that has finished initialize and
// session/new, including one with session/prompt in flight,
// exits in under 50ms. The ceiling stays longer so a session
// flushing a large store can finish before it is killed.
const childExitGrace = 5 * time.Second

func (b *ptyTransport) terminateChild() {
	if b.cmd == nil || b.cmd.Process == nil {
		return
	}
	p := b.cmd.Process
	slog.Info("pty: terminate child", "pid", p.Pid)
	signalGroup(p, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = b.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("pty: child exited", "pid", p.Pid, "forced", false)
		return
	case <-time.After(childExitGrace):
	}
	signalGroup(p, syscall.SIGKILL)
	<-done
	slog.Info("pty: child exited", "pid", p.Pid, "forced", true)
}

func signalGroup(p *os.Process, sig syscall.Signal) {
	// go-pty sets Setsid, so pid is the process group leader.
	if err := syscall.Kill(-p.Pid, sig); err != nil {
		_ = p.Signal(sig)
	}
}
