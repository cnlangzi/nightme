//go:build windows

package me

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// transientReplaceError reports whether the error is the one a reader
// that holds the file open causes.
//
// Windows refuses to replace a file that another handle holds open
// without the delete sharing: the rename comes back with "access
// denied" or with a sharing violation until the reader lets go.
func transientReplaceError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION:
		return true
	}
	return false
}
