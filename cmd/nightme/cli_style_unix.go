//go:build !windows

package main

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// styleEnabled on POSIX hosts trusts isatty — pty /
// terminal fd implies full VT support via the terminal
// driver. The Windows split (cli_style_windows.go) is the
// one that needs to probe + save/restore explicitly.
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

// saveAndEnableVT and restoreConsoleMode are no-ops on
// POSIX. The Unix terminal driver interprets ANSI codes
// unconditionally for pty fds; there's nothing to
// temporarily enable or restore.
func saveAndEnableVT(_ io.Writer) (uint32, bool) {
	return 0, true
}

func restoreConsoleMode(_ io.Writer, _ uint32) {}