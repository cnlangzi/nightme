//go:build windows

package main

import (
	"io"
	"os"
	"syscall"
	"unsafe"

	"github.com/mattn/go-isatty"
)

// styleEnabled + the VT save/restore helpers live here on
// Windows. The probe path is identical to Plan C, but
// SetConsoleMode is no longer called from inside paint()
// — it's invoked once by saveAndEnableVT, then reversed
// by restoreConsoleMode. Leaving VT on across the boundary
// into readline was the root cause of the hang on legacy
// conhost: readline reads the parent console mode at
// ConPTY / raw-mode setup time and doesn't expect VT to
// already be enabled.
const enableVirtualTerminalProcessing = 0x0004

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// styleEnabled on Windows returns true unconditionally
// for the styled prompt path; the caller (runREPLInteractive)
// has already called saveAndEnableVT to turn on VT for the
// prompt's duration. We trust the caller to call
// restoreConsoleMode before readline takes over.
//
// Why unconditional: if saveAndEnableVT succeeded, VT is
// on for this prompt. If it failed (no console handle,
// SetConsoleMode rejected), paint() falls back to visible
// ASCII via stripCSI in cli_style.go. We don't re-probe
// here because probing during paint would call
// GetConsoleMode on every helper invocation, which is
// wasteful and not informative once we've decided.
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
	return consoleVTEnabled(fd)
}

// consoleVTEnabled is the GetConsoleMode probe. Returns
// true only when the host console has the VT bit set.
// We don't mutate the mode here — that's the caller's job
// (saveAndEnableVT).
func consoleVTEnabled(fd uintptr) bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(fd, uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return false
	}
	return mode&enableVirtualTerminalProcessing != 0
}

// saveAndEnableVT reads the current console mode for the
// writer's fd, then turns on ENABLE_VIRTUAL_TERMINAL_PROCESSING.
// Returns the saved mode so the caller can pass it back to
// restoreConsoleMode after the styled prompt finishes.
//
// Returns ok=false when the writer isn't a *os.File (test
// path), the handle isn't a console (file/pipe), or
// SetConsoleMode rejected the new mode. The caller should
// skip the styled prompt in those cases — paint() falls
// back to visible ASCII on its own via stripCSI.
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
		// Already on — saving the current mode is a no-op
		// restore, but keep the value for symmetry.
		return mode, true
	}
	r, _, _ = procSetConsoleMode.Call(fd, uintptr(mode|enableVirtualTerminalProcessing))
	if r == 0 {
		return mode, false
	}
	return mode, true
}

// restoreConsoleMode puts the writer's fd back to the
// mode captured by saveAndEnableVT. Best-effort: errors
// are swallowed because there's nothing useful we can do
// at this point — the user already answered the prompt.
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