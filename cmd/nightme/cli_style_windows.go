//go:build windows

package main

import (
	"io"
	"os"
	"syscall"
	"unsafe"

	"github.com/mattn/go-isatty"
)

// styleEnabled on Windows probes the host console for VT support
// (ENABLE_VIRTUAL_TERMINAL_PROCESSING). The standard Go isatty
// check only asks "is this a console handle" — it does not check
// the VT bit, so legacy conhost builds return true but render
// ANSI escape codes as literal `\x1b[31m` text.
//
// We:
//   1. Trust IsCygwinTerminal unconditionally — MSYS2/Cygwin
//      ptys always speak VT.
//   2. Read the current console mode via GetConsoleMode. A
//      non-zero return with ENABLE_VIRTUAL_TERMINAL_PROCESSING
//      already set means the host handles ANSI natively.
//   3. If VT isn't set, try SetConsoleMode to enable it on
//      demand (works on Win10 1607+). A failed SetConsoleMode
//      means the host really doesn't support VT — we fall back
//      to plain text (paint becomes identity).
//
// Honouring NO_COLOR is the caller's job (kept here too for
// symmetry with the Unix path).
const enableVirtualTerminalProcessing = 0x0004

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

func styleEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fd := f.Fd()
	if isatty.IsCygwinTerminal(fd) {
		return true
	}
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(fd, uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		// Not a console handle — file, pipe, closed fd. No style.
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	// Try to turn on VT. Success means the host supports it; we
	// return true and the process keeps the new mode for the
	// remaining lifetime of this console.
	r, _, _ = procSetConsoleMode.Call(fd, uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}