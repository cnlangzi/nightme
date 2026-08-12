//go:build windows

// Package main — process termination primitives for `nightme kill`
// on Windows.
package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// errProcessGone marks a PID that no longer exists. Windows has no
// ESRCH, so we synthesize the same signal for the shared kill path.
var errProcessGone = errors.New("process not found")

// killProcess terminates pid via the Win32 API.
//
// Windows has no POSIX signals, so the SIGTERM-then-SIGKILL grace
// window has no equivalent: TerminateProcess is the only portable
// way to stop an arbitrary child, and the `--force` flag is
// therefore a no-op here. This mirrors internal/agentsession's
// reapOrphan, which makes the same trade-off.
//
// ERROR_INVALID_PARAMETER from OpenProcess means the PID does not
// exist — reported as errProcessGone so the caller prints "already
// exited" instead of a failure. Other OpenProcess errors (notably
// ERROR_ACCESS_DENIED, i.e. a live process we may not touch) are
// surfaced as real errors.
func killProcess(pid int, _ bool) error {
	if pid <= 0 {
		return errProcessGone
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return errProcessGone
		}
		return fmt.Errorf("open pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminate pid %d: %w", pid, err)
	}
	return nil
}

// isProcessGone reports whether err means "the process no longer
// exists", which kill treats as success.
func isProcessGone(err error) bool {
	return errors.Is(err, errProcessGone)
}

// verifyPIDOwner is the Windows counterpart of the Unix PID-identity
// check. It is currently a no-op (always "verified"): the Unix
// version shells out to `ps -o comm=`, and the equivalent here
// (`tasklist` parsing, or a Toolhelp32 snapshot) is enough extra
// surface that it deserves its own change with its own tests.
//
// Consequence, stated plainly: on Windows `nightme kill` can still
// signal a recycled PID. The exposure is the same as the runtime's
// own reapOrphan, which has never verified identity on any platform.
func verifyPIDOwner(_ int, _ string) (string, bool) {
	return "", true
}
