//go:build windows

package wf

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// replaceFile renames src to dst on Windows. os.Rename maps to
// MoveFileEx WITHOUT MOVEFILE_REPLACE_EXISTING, so the rename
// returns ERROR_ACCESS_DENIED whenever dst exists — and a workflow
// that is installed twice has to overwrite the first one. MoveFileEx
// with MOVEFILE_REPLACE_EXISTING does the rename in one step, so the
// destination is never missing.
func replaceFile(src, dst string) error {
	const flags = windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH
	srcPtr, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return fmt.Errorf("wf: encode src %q: %w", src, err)
	}
	dstPtr, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return fmt.Errorf("wf: encode dst %q: %w", dst, err)
	}
	if err := windows.MoveFileEx(srcPtr, dstPtr, flags); err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
