//go:build !windows

package main

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// styleEnabled on POSIX hosts trusts isatty — pty /
// terminal fd implies full VT support via the terminal
// driver. The Windows path is hard-wired to false (see
// cli_style_windows.go) because some Windows hosts have
// isatty=true but no actual VT processing, and the
// fallback path (Plan C / C-fb / transient VT) all had
// regressions on at least one host.
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