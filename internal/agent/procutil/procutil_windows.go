//go:build windows

package procutil

import (
	"errors"
	"syscall"
)

// Windows process liveness uses kernel32!OpenProcess with
// PROCESS_QUERY_LIMITED_INFO. Same access mask used by
// internal/chatsession/prober_kill_windows.go — we mirror
// the established pattern rather than reinventing it.
//
// OpenProcess returns a non-zero handle when the PID exists
// (and our process has the right to query it). We always
// CloseHandle on success so we don't leak; the handle is
// only used as a presence signal, never read.
//
// Error mapping:
//   - success → nil (alive)
//   - ERROR_ACCESS_DENIED (5) → nil (alive but not ours;
//     same policy as Unix EPERM — bridges should NOT trigger
//     respawn on a permission boundary)
//   - ERROR_INVALID_PARAMETER (87) → "process not found" err
//     (the real "dead" signal that triggers Keepalive recovery)
//   - any other error → wrapped syscall.Errno
func platformAlivePID(pid int) error {
	const processQueryLimitedInfo = 0x1000
	const errorAccessDenied = 5
	const errorInvalidParameter = 87

	modkernel32 := syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess := modkernel32.NewProc("OpenProcess")
	procCloseHandle := modkernel32.NewProc("CloseHandle")

	h, _, e1 := procOpenProcess.Call(
		uintptr(processQueryLimitedInfo),
		0, // bInheritHandle
		uintptr(pid),
	)
	if h != 0 {
		_, _, _ = procCloseHandle.Call(h)
		return nil
	}
	if errno, ok := e1.(syscall.Errno); ok {
		switch uint32(errno) {
		case errorAccessDenied:
			return nil
		case errorInvalidParameter:
			return errors.New("process not found")
		}
		return errno
	}
	if e1 != nil {
		return e1
	}
	return errors.New("procutil: OpenProcess returned no handle and no error")
}