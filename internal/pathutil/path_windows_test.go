//go:build windows

// Windows tests for the pathutil package. Mirrors the Unix test
// matrix in path_unix_test.go. The Windows side is heavier because
// of the multiple notions of "absolute" (drive-rooted, root-
// relative, UNC, drive-relative-without-separator, forward-slash
// variants). The cases below are the regressions for the gtw-close
// Invalid-argument bug plus the cwd::resolvePath semantics that
// NormalizeForOS deliberately mirrors.
//
// See F-PATHUTIL-001 §6.1 for the test matrix this file locks in.

package pathutil

import (
	"os"
	"strings"
	"testing"
)

// TestNormalizeForOS_DriveRootedBackslash — happy path Windows
// absolute form: backslash already, drive-rooted. Must be kept
// (modulo trailing-separator cleanup).
func TestNormalizeForOS_DriveRootedBackslash(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`C:\foo`, `C:\foo`},
		{`C:\foo\`, `C:\foo`},
		{`C:\foo\bar`, `C:\foo\bar`},
		{`c:\foo`, `c:\foo`}, // case preserved (Clean does not lowercase)
		{`C:\`, `C:\`},
	}
	for _, tc := range cases {
		got, err := NormalizeForOS(tc.in)
		if err != nil {
			t.Errorf("NormalizeForOS(%q) returned err: %v", tc.in, err)
			continue
		}
		if !strings.EqualFold(got, tc.want) {
			t.Errorf("NormalizeForOS(%q) = %q, want %q (case-insensitive)", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeForOS_DriveRootedForward is the **key regression**
// for the gtw-close bug: forward-slash drive-rooted paths must be
// turned into the canonical backslash form. The yml written by
// `git rev-parse --show-toplevel` arrives as "F:/foo" on Windows;
// downstream git argv with "F:/foo" triggers ERROR_INVALID_PARAMETER.
// NormalizeForOS collapses it to "F:\foo".
func TestNormalizeForOS_DriveRootedForward(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`C:/foo`, `C:\foo`},
		{`C:/foo/bar`, `C:\foo\bar`},
		{`C:/foo/`, `C:\foo`},
		{`F:/nightme.nightme/fix-windows-cli-style`, `F:\nightme.nightme\fix-windows-cli-style`},
	}
	for _, tc := range cases {
		got, err := NormalizeForOS(tc.in)
		if err != nil {
			t.Errorf("NormalizeForOS(%q) returned err: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeForOS(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeForOS_RootRelative — leading "/" or "\" gets the
// current drive prepended. Mirrors cwd::resolvePath's behaviour
// (see internal/command/cwd/path_windows.go).
func TestNormalizeForOS_RootRelative(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Skipf("no cwd on this system: %v", err)
	}
	if len(wd) < 2 || wd[1] != ':' {
		t.Skipf("cwd %q is not drive-rooted, skipping", wd)
	}
	drive := wd[:2] // "C:"

	cases := []struct {
		in, want string
	}{
		{`/some-root-relative-dir`, drive + `\some-root-relative-dir`},
		{`\some-root-relative-dir`, drive + `\some-root-relative-dir`},
		{`/foo/bar`, drive + `\foo\bar`},
	}
	for _, tc := range cases {
		got, err := NormalizeForOS(tc.in)
		if err != nil {
			t.Errorf("NormalizeForOS(%q) returned err: %v", tc.in, err)
			continue
		}
		// Case-insensitive compare for the drive prefix; rest
		// must match exactly.
		if len(got) < 2 || !strings.EqualFold(got[:2], drive) || got[2:] != tc.want[2:] {
			t.Errorf("NormalizeForOS(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeForOS_DriveRelative_Rejected — drive letter with no
// separator is ambiguous on Windows (means "relative to current dir
// on that drive"); we reject explicitly rather than silently join
// $HOME (which would produce a misleading "C:foo → C:\home\user\
// C:foo" result).
func TestNormalizeForOS_DriveRelative_Rejected(t *testing.T) {
	cases := []string{`C:foo`, `C:`, `c:foo`, `Z:bar`, `C:.`, `C:..`}
	for _, tc := range cases {
		_, err := NormalizeForOS(tc)
		if err == nil {
			t.Errorf("NormalizeForOS(%q) returned nil err; want drive-relative rejection", tc)
			continue
		}
		if !strings.Contains(err.Error(), "drive-relative") {
			t.Errorf("NormalizeForOS(%q) err = %v, want 'drive-relative' message", tc, err)
		}
	}
}

// TestNormalizeForOS_UNC — UNC paths must pass through (after
// Clean). We don't try to resolve \\server\share against a real
// server; the test just asserts the function doesn't error and
// doesn't try to join onto $HOME.
func TestNormalizeForOS_UNC(t *testing.T) {
	in := `\\server\share\path`
	got, err := NormalizeForOS(in)
	if err != nil {
		t.Fatalf("NormalizeForOS(UNC): %v", err)
	}
	if !strings.HasPrefix(got, `\\server\share`) {
		t.Errorf("NormalizeForOS(UNC) = %q, want \\server\\share prefix", got)
	}
}

// TestNormalizeForOS_LongPathPrefix — the "\\?\" prefix bypasses
// MAX_PATH checks downstream; Clean would mishandle some edge
// cases with it, so we preserve it verbatim.
func TestNormalizeForOS_LongPathPrefix(t *testing.T) {
	in := `\\?\F:\foo\bar`
	got, err := NormalizeForOS(in)
	if err != nil {
		t.Fatalf("NormalizeForOS(long-path): %v", err)
	}
	if got != in {
		t.Errorf("NormalizeForOS(%q) = %q, want verbatim passthrough", in, got)
	}
}

// TestNormalizeForOS_Relative_Passthrough — relative paths are NOT
// silently joined with $HOME. pathutil only does shape
// normalization; HOME-relative resolution is cwd::resolvePath's job.
func TestNormalizeForOS_Relative_Passthrough(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`foo`, `foo`},
		{`./foo`, `foo`},
		{`foo/bar`, `foo\bar`}, // Clean converts separators
	}
	for _, tc := range cases {
		got, err := NormalizeForOS(tc.in)
		if err != nil {
			t.Errorf("NormalizeForOS(%q) returned err: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeForOS(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeForOS_Empty — empty input is an error.
func TestNormalizeForOS_Empty(t *testing.T) {
	if _, err := NormalizeForOS(""); err == nil {
		t.Error("NormalizeForOS(\"\") returned nil err; want errEmptyPath")
	}
}

// TestNormalizeForGit_ForcesBackslash — the regression for the
// gtw-close Invalid-argument bug at the NormalizeForGit level.
// Even if NormalizeForOS were ever to return a mixed-separator
// path, NormalizeForGit must guarantee backslashes only.
func TestNormalizeForGit_ForcesBackslash(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`F:/nightme.nightme/fix-windows-cli-style`, `F:\nightme.nightme\fix-windows-cli-style`},
		{`C:/foo/bar`, `C:\foo\bar`},
		{`C:\foo/bar`, `C:\foo\bar`}, // mixed → all backslash
		{`C:\foo`, `C:\foo`},
	}
	for _, tc := range cases {
		got, err := NormalizeForGit(tc.in)
		if err != nil {
			t.Errorf("NormalizeForGit(%q) returned err: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeForGit(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.ContainsRune(got, '/') {
			t.Errorf("NormalizeForGit(%q) = %q, contains forward slash (must be backslash only)", tc.in, got)
		}
	}
}

func TestFromSlash_ConvertsSlashes(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`C:/foo/bar`, `C:\foo\bar`},
		{`/foo`, `\foo`},
		{`C:\foo`, `C:\foo`}, // already backslash → unchanged
		{"", ""},
	}
	for _, tc := range cases {
		if got := FromSlash(tc.in); got != tc.want {
			t.Errorf("FromSlash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestToSlash_ConvertsBackslashes(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`C:\foo\bar`, `C:/foo/bar`},
		{`\foo`, `/foo`},
		{`C:/foo`, `C:/foo`}, // already forward slash → unchanged
	}
	for _, tc := range cases {
		if got := ToSlash(tc.in); got != tc.want {
			t.Errorf("ToSlash(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEqual_CaseInsensitive — Windows drives / filenames are case-
// insensitive. Without this, IsUnder / Equal comparisons against
// git's output (which preserves case) vs. shell-derived paths (which
// also preserve case but may differ in drive letter case) would
// spuriously miss-match.
func TestEqual_CaseInsensitive(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`C:\Foo`, `c:\foo`, true},
		{`C:\FOO`, `C:\foo`, true},
		{`C:\Foo\bar`, `c:\foo\BAR`, true},
		{`C:\Foo`, `C:\bar`, false},
		{`C:\Foo`, `D:\Foo`, false}, // different drive
	}
	for _, tc := range cases {
		if got := Equal(tc.a, tc.b); got != tc.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestEqual_SlashInsensitive — forward-slash and backslash forms
// must compare equal. Critical for the gtw fix → gtw close flow:
// `git rev-parse --show-toplevel` emits forward slashes; downstream
// callers may produce backslashes; both must collapse to the same
// path identity.
func TestEqual_SlashInsensitive(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`C:\foo`, `C:/foo`, true},
		{`C:\foo\bar`, `C:/foo\bar`, true},
		{`C:\foo\bar`, `C:/foo/bar`, true},
	}
	for _, tc := range cases {
		if got := Equal(tc.a, tc.b); got != tc.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestEqual_TrailingSeparator — "C:\foo" and "C:\foo\" refer to
// the same directory and must compare equal.
func TestEqual_TrailingSeparator(t *testing.T) {
	if !Equal(`C:\foo`, `C:\foo\`) {
		t.Error(`Equal(C:\foo, C:\foo\) returned false; want true`)
	}
}

// TestIsUnder_SamePath — Equal semantics include reflexive identity.
func TestIsUnder_SamePath(t *testing.T) {
	if !IsUnder(`C:\foo`, `C:\foo`) {
		t.Error("IsUnder(C:\\foo, C:\\foo) = false; want true (reflexive)")
	}
}

// TestIsUnder_ChildOf — the common case.
func TestIsUnder_ChildOf(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{`C:\foo\bar`, `C:\foo`, true},
		{`C:\foo\bar\baz`, `C:\foo`, true},
		{`C:\foo\bar`, `C:\foo\`, true},
		{`C:\foo`, `C:\bar`, false},
		// Naive-prefix trap: "C:\foobar" must NOT match
		// "C:\foo" via HasPrefix("C:\foobar", "C:\foo\").
		{`C:\foobar`, `C:\foo`, false},
	}
	for _, tc := range cases {
		if got := IsUnder(tc.child, tc.parent); got != tc.want {
			t.Errorf("IsUnder(%q, %q) = %v, want %v", tc.child, tc.parent, got, tc.want)
		}
	}
}

// TestIsUnder_DifferentDrive — child and parent on different drives
// cannot be ancestor/descendant.
func TestIsUnder_DifferentDrive(t *testing.T) {
	cases := []struct {
		child, parent string
	}{
		{`D:\foo`, `C:\foo`},
		{`D:\foo\bar`, `C:\foo`},
		{`C:\foo`, `D:\foo`},
	}
	for _, tc := range cases {
		if IsUnder(tc.child, tc.parent) {
			t.Errorf("IsUnder(%q, %q) = true; want false (cross-drive)", tc.child, tc.parent)
		}
	}
}

// TestIsUnder_CaseInsensitive — Windows paths are case-insensitive;
// "C:\Foo\bar" is Under "c:\foo".
func TestIsUnder_CaseInsensitive(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{`C:\Foo\bar`, `c:\foo`, true},
		{`C:\foo\BAR`, `C:\Foo`, true},
		{`C:\Foo`, `c:\FOO`, true}, // reflexive
	}
	for _, tc := range cases {
		if got := IsUnder(tc.child, tc.parent); got != tc.want {
			t.Errorf("IsUnder(%q, %q) = %v, want %v", tc.child, tc.parent, got, tc.want)
		}
	}
}

// TestIsUnder_Empty — empty inputs return false (no sensible answer).
func TestIsUnder_Empty(t *testing.T) {
	cases := []struct {
		child, parent string
	}{
		{"", `C:\foo`},
		{`C:\foo`, ""},
		{"", ""},
	}
	for _, tc := range cases {
		if IsUnder(tc.child, tc.parent) {
			t.Errorf("IsUnder(%q, %q) = true; want false (empty input)", tc.child, tc.parent)
		}
	}
}

func TestIsWindowsDriveRel(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		// Drive-relative — rejected.
		{`C:`, true},
		{`C:foo`, true},
		{`C:.`, true},
		{`C:..`, true},
		{`c:`, true},    // case-insensitive
		{`c:foo`, true},
		{`Z:foo`, true},
		// Drive-rooted — NOT drive-relative.
		{`C:\foo`, false},
		{`C:/foo`, false},
		{`C:\`, false},
		{`C:/`, false},
		{`c:\foo`, false},
		{`c:/foo`, false},
		// Not drive paths at all.
		{"", false},
		{`/foo`, false},
		{`\foo`, false},
		{`foo`, false},
		{`:foo`, false},    // not a drive letter
		{`1:foo`, false},   // digit prefix
		{`\C:foo`, false},  // separator first
		{`\\server\share`, false},
	}
	for _, tc := range tests {
		if got := isWindowsDriveRel(tc.in); got != tc.want {
			t.Errorf("isWindowsDriveRel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeIMRichText_StripsAnchorTags(t *testing.T) {
	in := `<a href="https://example.com/foo">C:\dev\proj</a>`
	want := `C:\dev\proj`
	if got := NormalizeIMRichText(in); got != want {
		t.Errorf("NormalizeIMRichText(%q) = %q, want %q", in, got, want)
	}
}

func TestNormalizeInput_FullWidthSlash(t *testing.T) {
	// ／ (U+FF0F) is the full-width '/'. IME artefact.
	in := "／foo／bar"
	if got := NormalizeInput(in); !strings.HasPrefix(got, "/") {
		t.Errorf("NormalizeInput(%q) = %q, want '/' prefix", in, got)
	}
}

func TestNormalizeInput_PassesThroughCJKIdeographs(t *testing.T) {
	in := `C:\dev\代码`
	if got := NormalizeInput(in); got != in {
		t.Errorf("NormalizeInput clobbered CJK ideographs: %q → %q", in, got)
	}
}
