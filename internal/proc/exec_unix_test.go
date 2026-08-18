//go:build !windows

package proc

import (
	"context"
	"testing"
)

// TestNew_SetsSysProcAttrSetsid pins the platform-specific
// SysProcAttr wiring so a future regression that drops the
// Setsid flag (and re-introduces the F-54 / stop hang on macOS)
// is caught by CI before it reaches the daemon.
func TestNew_SetsSysProcAttrSetsid(t *testing.T) {
	cmd := New(context.Background(), "/bin/true")
	if cmd.SysProcAttr == nil {
		t.Fatal("New left SysProcAttr nil; the cli will inherit the daemon TTY")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("SysProcAttr.Setsid = false; cli will not be its own session/pg leader")
	}
}
