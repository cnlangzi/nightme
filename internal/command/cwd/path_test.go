package cwd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePath_Absolute_KeptAsIs verifies that an absolute
// path produced by t.TempDir() is preserved by resolvePath
// (i.e. it is NOT joined with $HOME). This is the regression
// guard for the Windows bug — on Windows, "/" was previously
// not classified as absolute by filepath.IsAbs, so paths like
// "/Users/me/foo" were silently prefixed with $HOME. With the
// platform-specific resolvePath in place, absolute inputs are
// kept verbatim on every OS.
func TestResolvePath_Absolute_KeptAsIs(t *testing.T) {
	tmp := t.TempDir() // absolute on every platform
	got, err := resolvePath(tmp)
	if err != nil {
		t.Fatalf("resolvePath(%q): %v", tmp, err)
	}
	// filepath.Abs cleans the path (resolves symlinks etc.
	// on macOS, normalises separators); compare the cleaned
	// form, not the raw t.TempDir() string.
	want, _ := filepath.Abs(tmp)
	if got != want {
		t.Fatalf("resolvePath(%q) = %q, want %q", tmp, got, want)
	}
}

// TestResolvePath_Absolute_DoesNotPrefixHome is the focused
// regression for the Windows "/foo joined with $HOME" bug:
// an absolute input must not be re-prefixed with $HOME just
// because the prefix happens to match. We compare the
// prefixes of input and output: if the input wasn't already
// under $HOME, the output shouldn't be either.
func TestResolvePath_Absolute_DoesNotPrefixHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no HOME on this system: %v", err)
	}

	tmp := t.TempDir()
	got, err := resolvePath(tmp)
	if err != nil {
		t.Fatalf("resolvePath(%q): %v", tmp, err)
	}

	// The check: if the output starts with $HOME but the
	// input didn't, something rewrote it. The t.TempDir()
	// exception covers CI runners that stage /tmp under the
	// user's home directory.
	inputUnderHome := strings.HasPrefix(tmp, home+string(filepath.Separator))
	outputUnderHome := strings.HasPrefix(got, home+string(filepath.Separator))
	if outputUnderHome && !inputUnderHome {
		t.Fatalf("resolvePath(%q) = %q looks $HOME-joined but input was absolute", tmp, got)
	}
}

// TestResolvePath_TildeExpanded_JoinsHome verifies that
// after expandTilde turns "~/foo" into "$HOME/foo",
// resolvePath treats the result as a relative path and
// keeps it under $HOME. (expandTilde itself is tested in
// commands_test.go; this test exercises the handoff
// between the two stages.)
func TestResolvePath_TildeExpanded_JoinsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no HOME on this system: %v", err)
	}

	expanded, err := expandTilde("~/some-tilde-subdir-xyz")
	if err != nil {
		t.Fatalf("expandTilde: %v", err)
	}
	// expandTilde on every supported platform joins with
	// filepath.Separator, which is fine.
	if !strings.HasPrefix(expanded, home) {
		t.Fatalf("expandTilde(\"~/...\") = %q, expected to start with $HOME=%q", expanded, home)
	}

	got, err := resolvePath(expanded)
	if err != nil {
		t.Fatalf("resolvePath(%q): %v", expanded, err)
	}
	want, _ := filepath.Abs(filepath.Join(home, "some-tilde-subdir-xyz"))
	if got != want {
		t.Fatalf("resolvePath(%q) = %q, want %q", expanded, got, want)
	}
}

// TestVerifyDirectory_ExistingDir_OK is the happy path: a
// directory that exists should verify without error.
func TestVerifyDirectory_ExistingDir_OK(t *testing.T) {
	tmp := t.TempDir()
	if err := verifyDirectory(tmp, "/some-raw-input"); err != nil {
		t.Fatalf("verifyDirectory(%q): %v", tmp, err)
	}
}

// TestVerifyDirectory_NonExistent_Rejected covers the
// "Path does not exist" branch. We use a path inside t.TempDir
// that we don't create, so the error message will reference
// both the absolute resolved path and the original raw input.
func TestVerifyDirectory_NonExistent_Rejected(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "definitely-not-there-xyz")
	const raw = "/some/user/typed/path"

	err := verifyDirectory(missing, raw)
	if err == nil {
		t.Fatal("verifyDirectory: expected error for non-existent path")
	}
	if !strings.Contains(err.Error(), "Path does not exist") {
		t.Errorf("error missing 'Path does not exist': %v", err)
	}
	if !strings.Contains(err.Error(), raw) {
		t.Errorf("error missing raw input %q: %v", raw, err)
	}
}

// TestVerifyDirectory_RegularFile_Rejected covers the
// "Not a directory" branch — a file that exists but isn't
// a directory must be rejected before we attempt to /cwd
// into it.
func TestVerifyDirectory_RegularFile_Rejected(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "regular.txt")
	if err := os.WriteFile(file, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	err := verifyDirectory(file, "regular.txt")
	if err == nil {
		t.Fatal("verifyDirectory: expected error for regular file")
	}
	if !strings.Contains(err.Error(), "Not a directory") {
		t.Errorf("error missing 'Not a directory': %v", err)
	}
}

// TestResolvePath_Relative_JoinsHome checks the documented
// /cwd semantics: a relative argument becomes $HOME/arg.
// Lives in path_test.go because the behaviour is identical
// on every supported platform — resolvePath delegates the
// IsAbs decision to the OS, and only the truly-relative
// branch reaches this code.
func TestResolvePath_Relative_JoinsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no HOME on this system: %v", err)
	}

	got, err := resolvePath("relative-subdir-name-xyz")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	want, _ := filepath.Abs(filepath.Join(home, "relative-subdir-name-xyz"))
	if got != want {
		t.Fatalf("resolvePath(%q) = %q, want %q", "relative-subdir-name-xyz", got, want)
	}
}
