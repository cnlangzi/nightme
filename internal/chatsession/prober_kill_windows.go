//go:build windows

package chatsession

import (
	"errors"
	"syscall"
)

var (
	modkernel32             = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess         = modkernel32.NewProc("OpenProcess")
	procCloseHandle         = modkernel32.NewProc("CloseHandle")
	processQueryLimitedInfo = 0x1000
)

// errEPERMValue is the sentinel for "exists but not ours" —
// on Windows OpenProcess access-denied maps to syscall.Errno(5)
// which is ERROR_ACCESS_DENIED. The prober treats that as "alive".
var errEPERMValue = errors.New("epERM")

// platformKill0 invokes Windows OpenProcess + CloseHandle as a
// process-existence probe (mirrors the Unix kill(pid, 0) semantics).
// Returns nil on success, errEPERMValue on access denied, or the
// underlying error otherwise. ERROR_INVALID_PARAMETER (87) means
// the PID does not exist.
func platformKill0(pid int) error {
	if pid <= 0 {
		return errors.New("invalid pid")
	}
	h, _, e1 := procOpenProcess.Call(uintptr(processQueryLimitedInfo), 0, uintptr(pid))
	if h != 0 {
		_, _, _ = procCloseHandle.Call(h)
		return nil
	}
	const errorAccessDenied = 5
	const errorInvalidParameter = 87
	if errno, ok := e1.(syscall.Errno); ok {
		switch uint32(errno) {
		case errorAccessDenied:
			return errEPERMValue
		case errorInvalidParameter:
			return errors.New("process not found")
		}
		return errno
	}
	if e1 != nil {
		return e1
	}
	return errors.New("OpenProcess failed")
}