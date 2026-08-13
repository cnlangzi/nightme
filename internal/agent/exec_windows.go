//go:build windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NewCmd on Windows is a thin wrapper around exec.CommandContext
// with one critical fix: when the resolved binary ends in .cmd
// or .bat, Go's exec passes that path as lpApplicationName to
// CreateProcess, which returns ERROR_INVALID_PARAMETER (87) for
// batch files — the OS expects cmd.exe to be invoked explicitly.
//
// Without this wrapping, every Windows install whose agent binary
// is shipped as a Node-style .cmd shim (pi-node's current\pi.cmd,
// npm's shim.cmd, etc.) fails to spawn with:
//
//	fork/exec <path>.cmd: The parameter is incorrect.
//
// We do the LookPath ourselves (mirroring what exec.Command
// would do internally) so we can inspect the resolved extension
// before letting CreateProcess see it. The wrapping mirrors
// Microsoft's documented workaround:
//
//	lpApplicationName = C:\Windows\System32\cmd.exe
//	lpCommandLine     = cmd /d /c "<resolved>" <args...>
//
// We also rely on Go's exec argv/escape logic to quote the
// resolved path correctly if it contains spaces.
//
// Windows has no Setsid equivalent and no controlling-TTY
// inheritance problem (Windows console handles are passed via
// explicit STARTUPINFO handles, not inherited fds). The bridge
// on Windows still works for single-pid Process.Signal / Kill,
// because SignalProcessGroup's Windows fallback already collapses
// to single-pid semantics.
func NewCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	// LookPath mirrors exec.Command's internal behaviour: it
	// resolves a bare name ("pi") against %PATH% and PATHEXT,
	// returning the absolute path with the matched extension.
	// Errors here are surfaced by exec.CommandContext itself
	// (via cmd.Err on the returned *Cmd), so we deliberately
	// let any error fall through to the raw-name path.
	lp := name
	if resolved, err := exec.LookPath(name); err == nil {
		lp = resolved
	}

	if isWindowsBatchExt(filepath.Ext(lp)) {
		// Locate cmd.exe. ComSpec is set on every standard
		// Windows install; the explicit fallback below covers
		// the rare case where it's been cleared by the user.
		comspec := os.Getenv("ComSpec")
		if comspec == "" {
			comspec = `C:\Windows\System32\cmd.exe`
		}
		// /d skips AutoRun registry commands, matching
		// cmd.exe's interactive default. /c is "run the
		// following command and exit".
		argv := make([]string, 0, 3+len(args))
		argv = append(argv, "/d", "/c", lp)
		argv = append(argv, args...)
		return exec.CommandContext(ctx, comspec, argv...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// isWindowsBatchExt reports whether ext (the result of
// filepath.Ext, with the leading dot) names a Windows batch
// extension that CreateProcess can't run as lpApplicationName.
// We case-fold because NTFS preserves case but Windows path
// comparisons are case-insensitive.
func isWindowsBatchExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".cmd", ".bat":
		return true
	}
	return false
}
