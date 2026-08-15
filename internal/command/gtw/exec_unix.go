//go:build !windows

// Unix-specific runCmd helpers.
//
// applyMSYSEnvNoPathConv is a no-op on Unix/macOS. The Windows-
// specific implementation in exec_windows.go sets MSYS path-
// translation env vars. On non-MSYS hosts those vars don't
// exist, and the helper's job is simply to leave env alone.
package gtw

// applyMSYSEnvNoPathConv returns env unchanged on Unix. The
// Windows version (exec_windows.go) appends MSYS_NO_PATHCONV=1
// and MSYS2_ARG_CONV_EXCL=*; see that file for the rationale
// and the caveat that these vars don't help with MSYS libc's
// getcwd() interception.
func applyMSYSEnvNoPathConv(env []string) []string {
	return env
}
