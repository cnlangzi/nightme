//go:build windows

package main

import (
	"os"
	"time"
)

// replaceReportFile moves src onto dst.
//
// Windows refuses to rename over a file another process holds open:
// MoveFileEx returns ERROR_ACCESS_DENIED. It does so even when the
// holder opened the file with FILE_SHARE_DELETE — the Win32 rename
// has strict semantics, not the POSIX ones, so the replacement is
// refused rather than pulling the file out from under the reader.
//
// The parent polls the report while the daemon rewrites it, so the
// daemon's replace has to wait for the parent to let go. The parent's
// hold is short — it opens the report, reads it and closes it again —
// so a short retry settles it. A replace that gives up leaves the
// report at the version it had before the channels were tried, and
// the operator never sees which channel came up.
func replaceReportFile(src, dst string) error {
	const attempts = 20
	const delay = 10 * time.Millisecond

	var err error
	for i := range attempts {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}
	return err
}
