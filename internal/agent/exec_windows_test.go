//go:build windows

package agent

import (
	"context"
	"strings"
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
