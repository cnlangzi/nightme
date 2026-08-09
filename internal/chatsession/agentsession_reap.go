package chatsession

import (
	"errors"
	"fmt"
	"syscall"
)

// reapOrphan kills a child PID left behind by a previous (crashed)
// runtime. Called from AgentSession.Spawn before launching a new
// child, to prevent accumulating zombie / orphan processes when the
// runtime dies without a graceful Close().
//
// Policy: SIGKILL direct, no grace period. The child is by definition
// unreachable (its parent is dead), so graceful shutdown serves no
// purpose. ESRCH (no such process) is treated as success — the
// process was already gone, which is the desired end state.
func reapOrphan(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return nil
}