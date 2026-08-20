//go:build !windows

package main

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// styleEnabled on POSIX hosts trusts isatty: a pty /
// terminal fd implies full VT support via the terminal
// driver. The Windows path is the only one that needs to
// probe VT explicitly (see cli_style_windows.go).
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