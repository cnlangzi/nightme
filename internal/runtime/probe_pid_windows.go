//go:build windows

package runtime

import "golang.org/x/sys/windows"

// pidAliveOS probes a Windows PID via OpenProcess +
// GetExitCodeProcess.
//
// OpenProcess returning ERROR_INVALID_PARAMETER (errno 87) means
// the PID does not exist. Any other error (notably
// ERROR_ACCESS_DENIED for SYSTEM-owned / cross-session /
// AppContainer PIDs) is conservatively reported as DEAD — this
// differs from the Unix policy, which treats EPERM as ALIVE.
// Rationale: cross-user agents are rare on Windows (the CLI is
// typically run as the same user who started the agent), and a
// process we cannot even query is more likely to be a recycled
// PID owned by another user than a SYSTEM-owned long-lived
// nightme child. Fail-closed here matches the surface-level
// "kill(pid, 0) returns permission error → can't be sure → treat
// as gone" intuition. Revisit if a real Windows user reports
// mass-reaps of long-lived SYSTEM children.
//
// STILL_ACTIVE (259) is the magic value GetExitCodeProcess returns
// for a live process; anything else is a real exit code (which can
// also include small numbers like 0 for a process that exited
// cleanly after OpenProcess was opened — handled conservatively as
// dead).
func pidAliveOS(pid int) bool {
	const stillActive = 259

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}
