//go:build windows

package gtw

import (
	"errors"

	"golang.org/x/sys/windows"
)

// lockHasLiveHolder reports whether a live process currently has the
// lock file open.
//
// Git holds the lock file open from the moment it creates it until it
// commits or rolls the lock back, so an open lock means a live git
// owns it — the file's age says nothing (a checkout keeps the lock
// open for the whole worktree update, and the lock's mtime stays at
// its creation time). A killed git closes its handle, leaving the
// file behind but no longer open.
//
// Windows: opening the file without sharing it fails with a sharing
// violation while another process holds it open.
func lockHasLiveHolder(path string) bool {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	handle, err := windows.CreateFile(
		pathPtr,
		0, // no access requested: the open itself is the probe
		0, // no sharing: another process holding the file makes this fail
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err == nil {
		_ = windows.CloseHandle(handle)
		return false
	}
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return false
	}
	// ERROR_SHARING_VIOLATION means someone holds it; anything else
	// is unknown, and unknown is treated as "held".
	return true
}
