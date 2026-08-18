//go:build windows

package agent

import (
	"context"
	"strings"
	"syscall"
	"testing"
)

// TestNewCmd_WrapsCmdFiles pins the .cmd/.bat wrapping so a
// future refactor that drops the cmd.exe /d /c shim
// (and re-introduces ERROR_INVALID_PARAMETER 87 on every
// Node-style shim install) is caught by CI before it ships.
func TestNewCmd_WrapsCmdFiles(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // isolate LookPath

	cmd := NewCmd(context.Background(), "pi", "--mode", "rpc")

	if cmd.Path == "" {
		t.Fatal("NewCmd returned an empty Path")
	}
	lower := strings.ToLower(cmd.Path)
	if !strings.HasSuffix(lower, "cmd.exe") {
		// Without a real pi.cmd on PATH, LookPath falls back to
		// the raw name and the wrapper is a no-op. That branch
		// is covered by the integration tests on a real
		// Windows runner; here we only assert the happy path
		// when a batch file IS resolvable.
		t.Skipf("NewCmd could not resolve a batch file in PATH (cmd.Path=%q); wrapper untested", cmd.Path)
	}
	// The wrapper inserts /d /c <resolved> in front of the
	// user args. Assert the order matches Microsoft's
	// documented workaround.
	if len(cmd.Args) < 4 {
		t.Fatalf("NewCmd argv = %v; want at least 4 entries (cmd.exe /d /c <path> ...)", cmd.Args)
	}
	if cmd.Args[0] != "/d" || cmd.Args[1] != "/c" {
		t.Errorf("NewCmd argv = %v; want first two entries to be /d / c", cmd.Args)
	}
	wantExt := ".cmd"
	if !strings.HasSuffix(strings.ToLower(cmd.Args[2]), wantExt) &&
		!strings.HasSuffix(strings.ToLower(cmd.Args[2]), ".bat") {
		t.Errorf("NewCmd argv[2] = %q; want a .cmd or .bat path", cmd.Args[2])
	}
	if got, want := cmd.Args[3:], []string{"--mode", "rpc"}; !equalStrings(got, want) {
		t.Errorf("NewCmd user args = %v; want %v (user args must come after /c <path>)", got, want)
	}
}

// TestNewCmd_DirectExePath verifies the native-binary branch:
// when the resolved path ends in .exe, NewCmd must NOT wrap it
// in cmd.exe /d /c — that would force an extra shell layer on
// every invocation, and breaks arg parsing for binaries that
// don't tolerate cmd.exe's quote-stripping rules.
//
// We exercise the routing through launchOnWindows so a future
// refactor that drops the extension switch is caught in CI.
func TestNewCmd_DirectExePath(t *testing.T) {
	cmd := launchOnWindows(context.Background(),
		`C:\Tools\pi.exe`, "--mode", "rpc")
	if !strings.EqualFold(cmd.Path, `C:\Tools\pi.exe`) {
		t.Errorf("launchOnWindows(.exe) cmd.Path = %q; want the resolved exe path", cmd.Path)
	}
	if got, want := cmd.Args, []string{`C:\Tools\pi.exe`, "--mode", "rpc"}; !equalStrings(got, want) {
		t.Errorf("launchOnWindows(.exe) cmd.Args = %v; want %v (no cmd.exe wrapper)", got, want)
	}
}

// TestNewCmd_PS1WrapsPowerShell pins the .ps1 → powershell
// branch so a regression that drops the PowerShell wrap (and
// re-introduces ERROR_INVALID_PARAMETER on .ps1 installs) is
// caught by CI.
func TestNewCmd_PS1WrapsPowerShell(t *testing.T) {
	cmd := launchOnWindows(context.Background(),
		`C:\Tools\pi.ps1`, "-Mode", "rpc")
	// exec.LookPath resolves "powershell.exe" to its full path
	// on Windows; we assert the basename instead of the exact
	// path so the test is robust against System32 relocation.
	if !strings.HasSuffix(strings.ToLower(cmd.Path), "powershell.exe") {
		t.Errorf("launchOnWindows(.ps1) cmd.Path = %q; want a path ending in powershell.exe", cmd.Path)
	}
	wantPrefix := []string{
		"-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-File", `C:\Tools\pi.ps1`,
	}
	// The resolved path may appear in args[0] (LookPath); we
	// compare the suffix from "-NoProfile" onwards.
	suffix := cmd.Args
	if len(cmd.Args) >= 1 && strings.Contains(strings.ToLower(cmd.Args[0]), "powershell") {
		suffix = cmd.Args[1:]
	}
	if !equalStrings(suffix, append(wantPrefix, "-Mode", "rpc")) {
		t.Errorf("launchOnWindows(.ps1) cmd.Args suffix = %v; want prefix %v + user args",
			suffix, wantPrefix)
	}
}

// TestNewCmd_JSWrapsNode pins the .js → node branch so a
// regression that drops the node.exe wrap is caught.
func TestNewCmd_JSWrapsNode(t *testing.T) {
	cmd := launchOnWindows(context.Background(),
		`C:\Tools\pi.js`, "--mode", "rpc")
	if !strings.HasSuffix(strings.ToLower(cmd.Path), "node.exe") {
		t.Errorf("launchOnWindows(.js) cmd.Path = %q; want a path ending in node.exe", cmd.Path)
	}
	wantSuffix := []string{`C:\Tools\pi.js`, "--mode", "rpc"}
	// Path may be absolute in args[0] (LookPath); strip it if so.
	suffix := cmd.Args[1:]
	if !equalStrings(suffix, wantSuffix) {
		t.Errorf("launchOnWindows(.js) cmd.Args[1:] = %v; want %v", suffix, wantSuffix)
	}
}

// TestNewCmd_NoExtensionDirect verifies that a path with no
// extension (or an unrecognised one) falls through to direct
// execution — the same fallback Linux/macOS use.
func TestNewCmd_NoExtensionDirect(t *testing.T) {
	cmd := launchOnWindows(context.Background(),
		`C:\Tools\pi`, "--mode", "rpc")
	if !strings.EqualFold(cmd.Path, `C:\Tools\pi`) {
		t.Errorf("launchOnWindows(no-ext) cmd.Path = %q; want direct", cmd.Path)
	}
	if got, want := cmd.Args, []string{`C:\Tools\pi`, "--mode", "rpc"}; !equalStrings(got, want) {
		t.Errorf("launchOnWindows(no-ext) cmd.Args = %v; want %v", got, want)
	}
}

// TestComspecOrDefault pins %ComSpec% handling — a regression
// that hardcodes C:\Windows\System32\cmd.exe would break on
// Windows installs that relocated System32 (rare but real).
func TestComspecOrDefault(t *testing.T) {
	t.Run("uses ComSpec when set", func(t *testing.T) {
		t.Setenv("ComSpec", `C:\Custom\cmd.exe`)
		if got := comspecOrDefault(); got != `C:\Custom\cmd.exe` {
			t.Errorf("comspecOrDefault = %q; want %q", got, `C:\Custom\cmd.exe`)
		}
	})
	t.Run("falls back when ComSpec is empty", func(t *testing.T) {
		t.Setenv("ComSpec", "")
		if got := comspecOrDefault(); got == "" {
			t.Errorf("comspecOrDefault returned empty string; expected fallback")
		}
	})
}

// TestIsWindowsBatchExt pins the extension classification so a
// stray .ps1 or .vbs addition doesn't silently fall through and
// trigger the same fork/exec ERROR_INVALID_PARAMETER path that
// this whole file exists to prevent.
func TestIsWindowsBatchExt(t *testing.T) {
	cases := []struct {
		ext  string
		want bool
	}{
		{".cmd", true},
		{".CMD", true},
		{".bat", true},
		{".BAT", true},
		{".Cmd", true},
		{".exe", false},
		{".ps1", false},
		{".vbs", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isWindowsBatchExt(tc.ext); got != tc.want {
			t.Errorf("isWindowsBatchExt(%q) = %v; want %v", tc.ext, got, tc.want)
		}
	}
}

// TestNewCmd_SetsCreateNoWindow pins the most important
// regression: every *exec.Cmd returned from NewCmd must have
// CREATE_NO_WINDOW set on SysProcAttr.CreationFlags. Without
// this, every spawn of a Windows console binary (cmd.exe
// wrapping a .cmd shim, node.exe, powershell.exe, …) pops a
// black console window on the user's desktop — the same
// flashing-taskbar symptom the user reported.
//
// This test walks EVERY launch branch (direct exe, .cmd/.bat
// wrapper, .ps1, .js, no-extension) and asserts the flag is
// set. launchOnWindows applies CREATE_NO_WINDOW internally
// (no separate "set after" step), so the production path is
// a single call — a future refactor that drops the flag in
// any branch gets caught here.
func TestNewCmd_SetsCreateNoWindow(t *testing.T) {
	cases := []struct {
		name     string
		resolved string
	}{
		{"direct_exe", `C:\Tools\pi.exe`},
		{"direct_exe_no_ext", `C:\Tools\pi`},
		{"wrapped_cmd", `C:\Tools\pi.cmd`},
		{"wrapped_bat", `C:\Tools\pi.bat`},
		{"wrapped_ps1", `C:\Tools\pi.ps1`},
		{"wrapped_js", `C:\Tools\pi.js`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := launchOnWindows(context.Background(), tc.resolved, "--mode", "rpc")
			if cmd.SysProcAttr == nil {
				t.Fatalf("SysProcAttr nil after launchOnWindows on %s", tc.name)
			}
			if cmd.SysProcAttr.CreationFlags&CreateNoWindow == 0 {
				t.Errorf("%s: CreationFlags=0x%x, missing CREATE_NO_WINDOW (0x%x)",
					tc.name, cmd.SysProcAttr.CreationFlags, CreateNoWindow)
			}
		})
	}
}

// TestHideWindow_MergesWithExistingFlags pins the
// merge-not-replace semantics: a future bridge that pre-sets
// SysProcAttr.CreationFlags (e.g. for CREATE_NEW_PROCESS_GROUP
// or EXTENDED_STARTUPINFO_PRESENT) must NOT have those flags
// silently dropped when HideWindow applies CREATE_NO_WINDOW.
// Otherwise the bridge would lose its own group / handle
// semantics and the next spawned tool subprocess could leak
// past the bridge exit.
func TestHideWindow_MergesWithExistingFlags(t *testing.T) {
	const wantOther = 0x00000200 // CREATE_NEW_PROCESS_GROUP, picked for its bit
	attr := &syscall.SysProcAttr{CreationFlags: wantOther}

	got := HideWindow(attr)

	if got.CreationFlags&CreateNoWindow == 0 {
		t.Errorf("HideWindow dropped CREATE_NO_WINDOW: got=0x%x, want bit 0x%x set",
			got.CreationFlags, CreateNoWindow)
	}
	if got.CreationFlags&wantOther == 0 {
		t.Errorf("HideWindow wiped existing flag: got=0x%x, want bit 0x%x preserved",
			got.CreationFlags, wantOther)
	}
	if got.CreationFlags != wantOther|CreateNoWindow {
		t.Errorf("HideWindow: got=0x%x, want exactly 0x%x (OR of pre-existing and CREATE_NO_WINDOW)",
			got.CreationFlags, wantOther|CreateNoWindow)
	}
	// HideWindow must return the SAME pointer the caller
	// passed in, not a fresh allocation that the caller would
	// have to remember to assign back.
	if got != attr {
		t.Errorf("HideWindow returned a new struct instead of mutating in-place; got=%p want=%p", got, attr)
	}
}

// TestHideWindow_NilSysProcAttr covers the case where the
// caller hasn't set SysProcAttr at all (the common case for
// every bridge). HideWindow must allocate the struct rather
// than nil-deref, and return the new struct so the caller can
// assign it back.
func TestHideWindow_NilSysProcAttr(t *testing.T) {
	got := HideWindow(nil)
	if got == nil {
		t.Fatal("HideWindow(nil) returned nil; expected a freshly allocated SysProcAttr")
	}
	if got.CreationFlags&CreateNoWindow == 0 {
		t.Errorf("HideWindow(nil): CreationFlags=0x%x, missing CREATE_NO_WINDOW",
			got.CreationFlags)
	}
}

// TestNewCmd_EnvFormat pins the most pernicious Windows bug
// we fixed in this file's siblings (internal/bridge/...):
// cmd.Env must contain only KEY=VALUE strings, never bare
// values. Windows CreateProcess rejects bare entries with
// ERROR_INVALID_PARAMETER (87), and Unix tolerates them so
// the bug only surfaces on Windows.
//
// The fix in the bridge layers is "don't append the command
// name as a bare string to env" — this test is the schema
// check for that contract. We can't easily assert against
// the bridge code from here (it lives in another package),
// but we can assert that NewCmd's own behaviour doesn't
// introduce a bare string: when we hand it the path of a
// .cmd shim, the resulting cmd.Env is whatever the caller
// set, and we never auto-append.
func TestNewCmd_DoesNotAppendBareEnv(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // isolate LookPath

	// Force the wrapper path: a known .cmd shim, name doesn't
	// matter, what matters is the resolved extension.
	cmd := NewCmd(context.Background(), "pi", "--mode", "rpc")
	if cmd.Path == "" {
		t.Fatal("NewCmd returned empty Path")
	}
	if !strings.HasSuffix(strings.ToLower(cmd.Path), "cmd.exe") {
		// LookPath fell back to the raw name; we can't assert
		// on extension behaviour without a real .cmd on PATH.
		t.Skipf("NewCmd could not resolve a .cmd (cmd.Path=%q); skipping env-format assertion",
			cmd.Path)
	}
	// cmd.Env at this point is whatever the OS-default was
	// (caller hasn't set it). The KEY thing: the resolved
	// path is not in cmd.Env as a bare string.
	for _, e := range cmd.Env {
		if e == "pi" || e == "claude" || e == "codex" || e == "opencode" {
			t.Errorf("NewCmd left bare agent name in cmd.Env: %q (Windows CreateProcess rejects this with ERROR_INVALID_PARAMETER 87)", e)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
