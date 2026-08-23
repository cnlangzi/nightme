//go:build !windows

package registry

import "os"

// replaceFile renames src to dst, replacing dst if it exists. On
// POSIX this is a single os.Rename call. On Windows, os.Rename
// maps to MoveFileEx WITHOUT the MOVEFILE_REPLACE_EXISTING flag,
// so rename-over-existing fails with ERROR_ACCESS_DENIED when
// the target file exists and is held open by a concurrent reader
// (e.g. an antivirus scanner, a parallel test process, or
// the test's own deferred os.Remove firing mid-rename). The
// Windows variant in replace_file_windows.go uses MoveFileEx with
// MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH so a
// concurrent reader gets an explicit sharing-violation rather
// than a generic access-denied, and the rename completes without
// retry loops.
func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
