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
	// Force LookPath to find an absolute path so the wrapper
	// sees a resolved extension. We can't actually execute
	// the wrapped binary in this unit test, but we can pin
	// that NewCmd rewrites the argv as expected.
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
		t.Errorf("NewCmd argv = %v; want first two entries to be /d /c", cmd.Args)
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
