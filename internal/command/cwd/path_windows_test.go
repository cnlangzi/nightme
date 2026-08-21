//go:build windows

// Windows-specific resolvePath tests.
//
// The platform-specific resolvePath lives in path_windows.go.
// On Unix these cases don't exist (no drive letters, no
// UNC roots, no concept of root-relative forward-slash), so
// the build tag keeps the assertions out of the Unix test
// run entirely.
package cwd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePath_RootRelativeForwardSlash is the regression
// for the original bug: "/foo" on Windows used to be treated
// as relative and joined with $HOME. With path_windows.go in
// place, "/foo" is recognised as root-relative and resolved
// to <current-drive>:\foo.
func TestResolvePath_RootRelativeForwardSlash(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no HOME on this system: %v", err)
	}

	got, err := resolvePath("/some-root-relative-dir")
	if err != nil {
		t.Fatalf("resolvePath(/some-root-relative-dir): %v", err)
	}

	// The fix: the result must NOT be under $HOME. It must
	// be the current drive + \some-root-relative-dir.
	if strings.HasPrefix(got, home+string(filepath.Separator)) ||
		strings.HasPrefix(got, home+"/") {
		t.Fatalf("resolvePath(\"/some-root-relative-dir\") = %q — still joined with $HOME %q", got, home)
	}
	if !strings.HasSuffix(strings.ToLower(got), `\some-root-relative-dir`) {
		t.Fatalf("resolvePath(\"/some-root-relative-dir\") = %q — expected current-drive:\\some-root-relative-dir", got)
	}
}

// TestResolvePath_DriveRelative_Rejected covers the
// drive-relative-without-separator case ("C:foo"). This is
// ambiguous on Windows and almost always a user typo — we
// reject it explicitly rather than silently mis-resolving.
func TestResolvePath_DriveRelative_Rejected(t *testing.T) {
	_, err := resolvePath("C:foo")
	if err == nil {
		t.Fatal("resolvePath(\"C:foo\") returned nil; expected drive-relative rejection")
	}
	if !strings.Contains(err.Error(), "drive-relative") {
		t.Fatalf("expected drive-relative error, got: %v", err)
	}
}

// TestResolvePath_DriveRootedBackslash verifies that the
// happy-path Windows absolute forms still work after the
// refactor: "C:\foo" must be kept as-is, not joined with
// $HOME.
func TestResolvePath_DriveRootedBackslash(t *testing.T) {
	got, err := resolvePath(`C:\some\absolute\path`)
	if err != nil {
		t.Fatalf("resolvePath(C:\\...): %v", err)
	}
	// filepath.Clean may normalise separators; we compare
	// case-insensitively because Windows drive letters are
	// case-insensitive but Go's filepath returns them as-given.
	if !strings.EqualFold(got, `C:\some\absolute\path`) {
		t.Fatalf("resolvePath(C:\\some\\absolute\\path) = %q", got)
	}
}

// TestResolvePath_DriveOnly_Rejected covers the bare-drive
// case "C:" (drive letter with no path component). Like
// "C:foo" this is drive-relative and ambiguous; isWindowsDriveRel
// must catch it before filepath.Clean gets a chance to
// rewrite it (Clean("C:") → "C:." on some Go versions,
// which would defeat a naive [1] == ':' check).
func TestResolvePath_DriveOnly_Rejected(t *testing.T) {
	_, err := resolvePath("C:")
	if err == nil {
		t.Fatal("resolvePath(\"C:\") returned nil; expected drive-relative rejection")
	}
	if !strings.Contains(err.Error(), "drive-relative") {
		t.Fatalf("expected drive-relative error, got: %v", err)
	}
}

// TestResolvePath_UNC verifies that UNC paths are kept as
// absolute. We don't actually resolve \\server\share
// against a real server — filepath.Abs on Windows just
// normalises and prefixes the current drive if the UNC
// is malformed — but we can at least assert that the
// function returns without error and doesn't try to join
// onto $HOME.
func TestResolvePath_UNC(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no HOME on this system: %v", err)
	}

	got, err := resolvePath(`\\server\share\path`)
	if err != nil {
		t.Fatalf("resolvePath(\\\\%s...): %v", "server\\share", err)
	}
	if strings.HasPrefix(got, home) {
		t.Fatalf("resolvePath(UNC) = %q — joined with $HOME %q", got, home)
	}
}

// TestIsWindowsDriveRel: REMOVED in F-PATHUTIL-001. The helper
// lived here to drive resolvePath's drive-relative rejection;
// after F-PATHUTIL-001 it lives in internal/pathutil/
// path_windows.go::isWindowsDriveRel (tested in
// internal/pathutil/path_windows_test.go::TestIsWindowsDriveRel
// with the same exhaustive table). Keeping it here would
// require a parallel implementation; the test is in pathutil now.

// TestVerifyDirectory_DriveRoot covers the Windows drive
// root case (C:\). The drive root IS a directory and must
// verify successfully — otherwise users couldn't /cwd to a
// path that intentionally targets the volume root.
func TestVerifyDirectory_DriveRoot(t *testing.T) {
	// We don't know the test runner's current drive on a
	// Windows CI box, but the system drive is reliably
	// available. Read %SystemDrive% if set, fall back to C:\.
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	root := drive + `\`
	if err := verifyDirectory(root, root); err != nil {
		t.Fatalf("verifyDirectory(%q): drive root should be a directory, got %v", root, err)
	}
}
