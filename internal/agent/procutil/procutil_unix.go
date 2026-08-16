//go:build unix

package procutil

import (
	"errors"
	"syscall"
)

// platformAlivePID is the Unix implementation of AlivePID. It
// uses syscall.Kill(pid, 0) — the cheapest possible existence
// probe; the kernel returns nil for "alive and ours",
// EPERM for "alive but owned by another user", ESRCH for
// "no such process". We surface all three so the bridge's
// Keepalive can distinguish "needs recovery" (ESRCH) from
// "process is alive but invisible" (EPERM / nil).
func platformAlivePID(pid int) error {
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil {
		return nil
	}
	// EPERM is "alive but not ours" — bridges should treat
	// that as a normal "alive" signal so a permission boundary
	// doesn't trigger an unnecessary respawn. We pass through
	// the original errno so the bridge can decide its policy.
	if errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}