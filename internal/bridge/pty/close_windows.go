//go:build windows

package pty

import "log/slog"

func (b *ptyTransport) terminateChild() {
	if b.cmd == nil || b.cmd.Process == nil {
		return
	}
	p := b.cmd.Process
	slog.Info("pty: terminate child", "pid", p.Pid)
	_ = p.Kill()
	_ = b.cmd.Wait()
	slog.Info("pty: child exited", "pid", p.Pid, "forced", true)
}
