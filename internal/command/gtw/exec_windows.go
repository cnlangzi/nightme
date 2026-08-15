//go:build windows

// Windows-specific runCmd helpers.
//
// runCmd (defined in exec.go) is the canonical CLI runner for
// every git/gh/glab/hook call. On Windows + Git for Windows
// bash (the default shell on GitHub Actions windows-latest
// runner, and what most Windows nightme users have), MSYS
// translates path-like argv entries before they reach
// CreateProcess. That can silently re-write what nightme set
// in cmd.Env or command arguments, e.g. `--git-dir=C:\foo`
// becomes `--git-dir=/c/foo` in the child process.
//
// The fix is to set MSYS env vars that disable the translation.
// Two env vars cover both MSYS generations:
//
//   - MSYS_NO_PATHCONV=1    — MSYS / Git for Windows v1 switch.
//     Disables path conversion in argv passed to CreateProcess
//     via the bash layer.
//   - MSYS2_ARG_CONV_EXCL=* — MSYS2 / Git for Windows v2
//     switch (added in 2024). Disables the newer pattern-based
//     converter that v1's MSYS_NO_PATHCONV didn't fully cover.
//     `*` excludes every arg from conversion.
//
// These env vars are no-ops on native Windows (no MSYS layer),
// Linux, macOS, and WSL bash. The one place they don't help:
// MSYS libc's interception of getcwd() (which is why the
// pwd-based runCmd tests would still fail). We use sentinel
// files to test cmd.Dir instead of parsing pwd output, so
// that's not a problem in the test suite.
//
// The helper below is the single place these env vars are
// added to a child's env. Other code (tests, callers) does
// NOT need to set them — only runCmd does, since it's the
// one path that bridges nightme's logical paths to MSYS's
// translated paths.
package gtw

// applyMSYSEnvNoPathConv appends the two MSYS path-conversion
// env vars to env on Windows, and returns env unchanged on
// non-MSYS hosts. On non-Windows this is a no-op; on Windows
// without MSYS (e.g. cmd.exe, PowerShell-launched children)
// the env vars are read by nobody and are harmless.
func applyMSYSEnvNoPathConv(env []string) []string {
	return append(env,
		"MSYS_NO_PATHCONV=1",
		"MSYS2_ARG_CONV_EXCL=*",
	)
}
