//go:build windows

package agentsession

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// reapOrphan kills a child PID left behind by a previous (crashed)
// runtime. Called from AgentSession.Spawn before launching a new
// child, to prevent accumulating zombie / orphan processes when
// the runtime dies without a graceful Close().
//
// Policy: TerminateProcess direct, no grace period. The child is
// by definition unreachable (its parent is dead), so graceful
// shutdown serves no purpose.
//
// Windows has no POSIX signals, so we use the Win32 API directly:
// OpenProcess with PROCESS_TERMINATE, then TerminateProcess. We
// CloseHandle in defer to avoid leaking the kernel handle.
//
// Error semantics:
//   - OpenProcess fails (any reason) → return nil. The Win32 API
//     returns ERROR_INVALID_PARAMETER for non-existent PIDs and
//     ERROR_ACCESS_DENIED for PIDs the caller can't reach — both
//     mean "the process is gone or unreachable from us", which
//     is the same end state we wanted. Mirrors the Unix version's
//     ESRCH-as-success policy.
//   - TerminateProcess fails → return wrapped error so the caller
//     sees that something went wrong.
func reapOrphan(pid int) error {
	if pid <= 0 {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		// Process is gone or unreachable — same end state as
		// the Unix ESRCH branch. Best-effort reap: bail out
		// silently rather than block Spawn on a stuck child.
		return nil
	}
	defer windows.CloseHandle(h)

	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminate pid %d: %w", pid, err)
	}
	return nil
}