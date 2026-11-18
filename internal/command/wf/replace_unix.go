//go:build !windows

package wf

import "os"

// replaceFile renames src to dst, replacing dst if it exists. On
// POSIX this is a single os.Rename call.
func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
