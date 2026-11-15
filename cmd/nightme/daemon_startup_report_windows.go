//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// openReportFile opens the startup report for reading.
//
// The handle allows FILE_SHARE_DELETE. Windows refuses to replace a
// file that another process holds open without that share flag: the
// rename fails with "Access is denied" and the report goes stale. The
// daemon rewrites the report while the parent polls it, so the
// parent's handle has to allow the replace — the parent then keeps
// reading the file it opened while the daemon publishes a new one.
func openReportFile(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
