//go:build !windows

package main

import "os"

// openReportFile opens the startup report for reading.
//
// On Unix a reader never keeps the writer from replacing the file:
// the rename is atomic and the reader keeps reading the old inode.
// The plain open is enough.
func openReportFile(path string) (*os.File, error) {
	return os.Open(path)
}
