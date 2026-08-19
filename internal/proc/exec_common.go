// Cross-platform helpers shared by exec_unix.go and
// exec_windows.go. No build tag — compiled on every platform.
//
// Keep this file dependency-light (only stdlib) and add to it
// only when a helper is genuinely cross-platform. Per-platform
// spawn recipe / sysattrs / window flags belong in the
// platform-tagged files.

package proc

import (
	"fmt"
	"os"
)

// currentExePath returns the absolute path of the running
// nightme binary. Centralised so the three OpenTerminal
// implementations (macOS / Linux / Windows) share the same
// os.Executable() call + error wrap; one fix lands in one
// place.
//
// The tray only fires from inside the daemon child, which IS
// nightme, so the running binary's path is the right one to
// re-spawn from every tray click — sidestepping the
// `go install` / scoop / Homebrew PATH ambiguity that
// exec.LookPath cannot always resolve.
func currentExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("proc: resolve executable: %w", err)
	}
	return exe, nil
}
