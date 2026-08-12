//go:build !windows

package agent

import (
	"context"
	"testing"
)

// TestNewCmd_SetsSysProcAttrSetsid pins the platform-specific
// SysProcAttr wiring so a future regression that drops the
// Setsid flag (and re-introduces the F-54 / stop hang on macOS)
// is caught by CI before it reaches the daemon.
func TestNewCmd_SetsSysProcAttrSetsid(t *testing.T) {
	cmd := NewCmd(context.Background(), "/bin/true")
	if cmd.SysProcAttr == nil {
		t.Fatal("NewCmd left SysProcAttr nil; the cli will inherit the daemon TTY")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("SysProcAttr.Setsid = false; cli will not be its own session/pg leader")
	}
}
