//go:build !windows

package runtime

import (
	"errors"
	"syscall"
)

// pidAliveOS uses signal 0 — the canonical POSIX liveness probe.
// It performs the kernel's existence + permission check without
// delivering anything.
//
// Failure modes:
//
//   - nil: process exists, signal would be deliverable. Alive.
//   - EPERM: process exists but is owned by another user (e.g. an
//     agent launched by root from inside the user's container, or
//     a sudo child). We report ALIVE — recycling EPERM as "dead"
//     would silently demote every cross-user agent to exited(-3)
//     on the next daemon restart. Matches procutil.Probe and
//     chatsession.proberKill — both treat EPERM as live.
//   - ESRCH: no such PID. Dead.
//   - anything else (EINTR, EAGAIN on a dying process): dead.
//     Indeterminate signals should not strand an entry as
//     "running" forever; flipping it to exited is the safer
//     default — list / kill both observe the corrected state.
func pidAliveOS(pid int) bool {
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
