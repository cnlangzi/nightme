//go:build !windows

package main

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// styleEnabled on POSIX hosts trusts isatty: a pty/terminal fd
// implies full VT support. This is the long-standing behaviour;
// the Windows split is purely additive.
func styleEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}