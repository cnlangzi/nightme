//go:build windows

package main

import (
	"io"
	"os"
	"syscall"
	"unsafe"

	"github.com/mattn/go-isatty"
)

// styleEnabled on Windows is hard-wired to false: the
// styled prompt path always falls back to visible ASCII
// ("[33m[1m▲[0m" instead of "\x1b[33m\x1b[1m▲\x1b[0m").
//
// Why no SetConsoleMode here: every previous attempt to
// enable ENABLE_VIRTUAL_TERMINAL_PROCESSING from inside
// the paint helpers (or from the prompt wrapper) broke
// readline's takeover on the user's machine. The symptom
// was either an outright hang (cursor just blinking,
// nightme> never appearing) or readline's CSI sequences
// rendered as literal text ("[?25l[120D...") instead of
// being interpreted by the terminal.
//
// The fix: keep paint() in ASCII fallback mode always,
// and let saveAndEnableVT (called once, right before
// readline.NewShell) toggle the console mode JUST for
// readline's lifetime. paint() never sees VT on, so the
// prompt stays readable as visible ASCII; readline takes
// over with VT on, so its CSI sequences are interpreted
// and the terminal can accept keystrokes normally.
func styleEnabled(_ io.Writer) bool {
	return false
}

// saveAndEnableVT reads the current console mode for the
// writer's fd, then turns on ENABLE_VIRTUAL_TERMINAL_PROCESSING.
// Returns the saved mode so the caller can pass it back to
// restoreConsoleMode after readline exits.
//
// Returns ok=false when the writer isn't a *os.File (test
// path), the handle isn't a console (file/pipe), or
// SetConsoleMode rejected the new mode. The caller should
// skip enabling in those cases — readline would still work,
// just without CSI interpretation.
func saveAndEnableVT(w io.Writer) (savedMode uint32, ok bool) {
	f, ok := w.(*os.File)
	if !ok {
		return 0, false
	}
	fd := f.Fd()
	if isatty.IsCygwinTerminal(fd) {
		// Cygwin/MSYS2 pty already has VT; nothing to save.
		return 0, true
	}
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(fd, uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return 0, false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		// Already on — save current mode for symmetry.
		return mode, true
	}
	r, _, _ = procSetConsoleMode.Call(fd, uintptr(mode|enableVirtualTerminalProcessing))
	if r == 0 {
		return mode, false
	}
	return mode, true
}

// restoreConsoleMode puts the writer's fd back to the
// mode captured by saveAndEnableVT. Best-effort.
func restoreConsoleMode(w io.Writer, savedMode uint32) {
	f, ok := w.(*os.File)
	if !ok {
		return
	}
	fd := f.Fd()
	if isatty.IsCygwinTerminal(fd) {
		return
	}
	_, _, _ = procSetConsoleMode.Call(fd, uintptr(savedMode))
}

const enableVirtualTerminalProcessing = 0x0004

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)