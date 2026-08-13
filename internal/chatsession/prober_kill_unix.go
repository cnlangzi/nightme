//go:build unix

package chatsession

import (
	"errors"
	"syscall"
)

// errEPERMValue is the sentinel returned when kill(pid, 0) hits a
// process owned by another user (still considered "alive" by the
// prober).
var errEPERMValue = errors.New("epERM")

// platformKill0 invokes syscall.Kill(pid, 0). Returns nil on
// success, errEPERMValue on EPERM, or the underlying errno
// otherwise.
func platformKill0(pid int) error {
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EPERM) {
		return errEPERMValue
	}
	return err
}