//go:build !windows

package copilot

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCopilotCLI writes a stand-in copilot binary that answers
// `--help` with a usage text and leaves a background process
// holding its output stream — the shape the real CLI has on Windows,
// where cmd.exe spawns node which spawns the binary.
//
// Unix-only: the fake is a `#!/bin/sh` script.
func fakeCopilotCLI(t *testing.T, supportsTelemetryFlag bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copilot")
	body := "#!/bin/sh\necho 'Usage: copilot [OPTIONS] [COMMAND]'\n"
	if supportsTelemetryFlag {
		body += "echo '      --disable-client-telemetry'\n"
	}
	// A lingering grandchild that inherits stdout: the direct child
	// exits, but the pipe stays open.
	body += "(sleep 60) &\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake copilot: %v", err)
	}
	return path
}

// TestProbeTelemetryFlag_ReturnsDespiteALingeringGrandchild is the
// regression guard for the probe wedging the daemon's startup.
//
// The probe reads the CLI's help output. If the CLI leaves a
// background process holding that output — which the Windows
// cmd.exe → node → binary chain does — the read waits for an EOF
// that never arrives, and the probe never returns. os/exec's
// WaitDelay is what bounds it.
func TestProbeTelemetryFlag_ReturnsDespiteALingeringGrandchild(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		done <- probeTelemetryFlag(fakeCopilotCLI(t, true))
	}()

	select {
	case supported := <-done:
		if !supported {
			t.Error("probeTelemetryFlag = false, want true (the fake's help lists the flag)")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("probeTelemetryFlag hung on a CLI that left its output stream open")
	}
}
