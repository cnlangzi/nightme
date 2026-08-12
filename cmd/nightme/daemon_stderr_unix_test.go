//go:build unix

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestChildExitDetail_CrashVsKill is the whole point of reporting
// the child's ProcessState in the readiness-failure message: an
// operator must be able to tell "the daemon crashed on its own"
// (exit status N — look at the stderr capture) from "something
// else killed it" (signal: ...), which are entirely different
// investigations.
func TestChildExitDetail_CrashVsKill(t *testing.T) {
	t.Run("nonzero exit", func(t *testing.T) {
		cmd := exec.Command("/bin/sh", "-c", "exit 3")
		if err := cmd.Start(); err != nil {
			t.Skipf("/bin/sh not available: %v", err)
		}
		_ = cmd.Wait()
		if got := childExitDetail(cmd.ProcessState); got != "exit status 3" {
			t.Errorf("childExitDetail = %q, want %q", got, "exit status 3")
		}
	})

	t.Run("killed by signal", func(t *testing.T) {
		cmd := exec.Command("/bin/sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Skipf("/bin/sleep not available: %v", err)
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("kill: %v", err)
		}
		_ = cmd.Wait()
		got := childExitDetail(cmd.ProcessState)
		if !strings.HasPrefix(got, "signal:") {
			t.Errorf("childExitDetail = %q, want a signal description", got)
		}
	})
}
