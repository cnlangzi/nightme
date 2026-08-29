//go:build windows

package proc

import (
	"os/exec"
	"time"
)

// KillGrace mirrors grace_unix.go's const so callers (gtw/runCmd,
// future ports) don't need build tags to reference it. On Windows
// it is informational only — there is no SIGTERM analogue and
// exec.CommandContext already runs TerminateProcess on ctx-fire,
// which is the same effective behaviour as plain SIGKILL. See
// docs/feat/F-CLAUDE-PRINT-002 §后续 2026-08-29 for the cross-
// platform rationale.
const KillGrace = 1 * time.Second

// WithGrace is a no-op on Windows.
//
// Why no-op: Windows has no process group in the POSIX sense and
// no SIGTERM analogue. The closest signal is the same
// TerminateProcess that exec.CommandContext already invokes when
// ctx fires — i.e. today's behaviour, which has no grace period
// but does terminate the child. Spinning up a watcher goroutine
// for nothing would be wasted work.
//
// If a future Windows-only graceful-cancel story lands (e.g.
// CTRL_BREAK_EVENT on its own console), the implementation lives
// here. Today the symbol exists only to keep gtw/runCmd's
// call site platform-agnostic.
func WithGrace(_ *exec.Cmd, _ time.Duration) {}
