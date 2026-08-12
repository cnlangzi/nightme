//go:build windows

package agent

import (
	"os"
	"syscall"
)

// SignalProcessGroup on Windows falls back to single-pid signaling.
//
// Windows has no concept of process groups the way Unix does, so
// "broadcast to the whole tree" is not directly expressible. The
// best approximation is Process.Signal on the immediate pid —
// the cli itself exits, but a spawned `Bash` tool subprocess may
// keep running. Callers that need stricter teardown on Windows
// should track child handles explicitly and signal them.
//
// Returns nil when p is nil so call-site code can stay
// panic-free across the bridge layer.
func SignalProcessGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	return p.Signal(sig)
}
