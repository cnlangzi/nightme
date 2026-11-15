//go:build !windows

package main

import "os"

// replaceReportFile moves src onto dst.
//
// On Unix the rename goes through the same name the reader holds
// open: the reader keeps reading the old inode and the new file
// takes the name. Nothing has to be waited for.
func replaceReportFile(src, dst string) error {
	return os.Rename(src, dst)
}
