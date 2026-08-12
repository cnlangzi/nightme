//go:build windows

package agent

import (
	"context"
	"os/exec"
)

// NewCmd on Windows is a thin wrapper around exec.CommandContext.
//
// Windows has no Setsid equivalent and no controlling-TTY
// inheritance problem (Windows console handles are passed via
// explicit STARTUPINFO handles, not inherited fds). The bridge
// on Windows still works for single-pid Process.Signal / Kill,
// because SignalProcessGroup's Windows fallback already collapses
// to single-pid semantics.
func NewCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
