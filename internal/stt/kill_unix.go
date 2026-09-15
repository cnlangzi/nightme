//go:build !windows

package stt

import (
	"fmt"
	"os/exec"
	"strings"
)

// isWindowsExe is true on Windows, where the worker binary
// carries a .exe suffix; false on Unix. The other half of this
// build-tag pair lives in kill_windows.go.
var isWindowsExe = false

// killByName terminates any running process whose command line
// contains path. Used by the install / upgrade path to evict a
// stale worker before overwriting its binary on disk; the
// execHandle spawned by ProductionSpawner is the canonical
// shutdown path during normal operation.
//
// We deliberately match on the full path (not just "nightme-stt")
// so a co-installed build in a different dataDir cannot be
// mistaken for ours. Best-effort: a missing match returns nil
// rather than an error so the install path can run idempotently
// against fresh machines.
func killByName(path string) error {
	if path == "" {
		return fmt.Errorf("stt: killByName: empty path")
	}
	cmd := exec.Command("pkill", "-f", path)
	// pkill exits 1 when no process matches. We don't want
	// that to bubble up — the caller treats "no match" as
	// success so first-time installs don't error.
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("stt: pkill %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
