//go:build windows

package registry

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// replaceFile renames src to dst on Windows. os.Rename maps to
// MoveFileEx WITHOUT MOVEFILE_REPLACE_EXISTING, so the rename
// returns ERROR_ACCESS_DENIED whenever dst exists (even transiently)
// and is being read — e.g. by antivirus or by a parallel test
// process. We use MoveFileEx with MOVEFILE_REPLACE_EXISTING so the
// rename succeeds regardless of whether the target is momentarily
// open for read elsewhere.
//
// MOVEFILE_WRITE_THROUGH asks the filesystem to flush the rename
// to disk before returning. That eliminates a tiny window where
// a concurrent reader could observe the new file before it
// existed on stable storage, at the cost of one disk sync. For
// agent_sessions.json (rewrite a few hundred bytes at most) the
// latency hit is in the microseconds and not noticeable; the
// "absolute durability" is what matters in this code path.
func replaceFile(src, dst string) error {
	const flags = windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH
	srcPtr, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return fmt.Errorf("agent_sessions: encode src %q: %w", src, err)
	}
	dstPtr, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return fmt.Errorf("agent_sessions: encode dst %q: %w", dst, err)
	}
	if err := windows.MoveFileEx(srcPtr, dstPtr, flags); err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
