package agent

import (
	"errors"
	"os"
	"syscall"
)

// SignalProcessGroup delivers sig to the entire OS process group
// of p. This is the "Ctrl-C in a TTY" equivalent: the terminal
// driver sends SIGINT to the foreground process group, so every
// descendant (e.g. a Claude Code cli + its spawned `Bash` tool
// subprocess) gets interrupted together. With bridge launcher
// SysProcAttr{Setsid: true} the cli is the session/pg leader, so
// -p.Pid is its own pgid and the broadcast lands everywhere.
//
// Falls back to a single-pid signal when the OS rejects the
// broadcast (e.g. a transient ESRCH between the SIGCHLD and the
// caller observing the reaped cli). Returns nil when p is nil so
// the helpers can stay call-site-clean.
func SignalProcessGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return p.Signal(sig)
		}
		return err
	}
	return nil
}
