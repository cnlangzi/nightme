//go:build windows

package main

import (
	"io"
	"os"
	"syscall"
	"unsafe"

	"github.com/mattn/go-isatty"
)

// styleEnabled on Windows probes the host console for
// ENABLE_VIRTUAL_TERMINAL_PROCESSING via GetConsoleMode —
// WITHOUT calling SetConsoleMode.
//
// Why no SetConsoleMode: readline reads the parent console
// state at startup (its ConPTY / raw-mode setup depends on
// it). A side-effecting SetConsoleMode here breaks the
// REPL — the y/N prompt paints correctly but readline
// never produces the nightme> prompt. Confirmed via two
// commit attempts on fix-windows-cli-style.
//
// Detection only: if the host has VT enabled (Windows
// Terminal, PowerShell 7+, Win10 1607+ cmd.exe), paint()
// emits real ANSI. If not (legacy conhost), paint() emits
// the SGR parameter as visible ASCII ("[33m") so the user
// can read which style would have applied, instead of
// seeing a raw ESC byte that the terminal renders as
// invisible garbage next to the character.
const enableVirtualTerminalProcessing = 0x0004

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
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
	// Cygwin/MSYS2 pty always speaks VT.
	if isatty.IsCygwinTerminal(fd) {
		return true
	}
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(fd, uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		// Not a console handle (file, pipe, closed fd).
		return false
	}
	return mode&enableVirtualTerminalProcessing != 0
}