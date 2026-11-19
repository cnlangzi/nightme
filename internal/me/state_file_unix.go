//go:build !windows

package me

// transientReplaceError reports whether the error is the one a reader
// that holds the file open causes.
//
// A POSIX rename replaces a file that a reader holds open: the reader
// goes on reading the file it opened, and the name points at the new
// file from then on. No error of a rename is transient here.
func transientReplaceError(err error) bool {
	return false
}
