//go:build !windows

// Unix tests for the pathutil package. Mirrors the Windows test
// matrix in path_windows_test.go. The Unix side is intentionally
// short — POSIX has one absolute form and the helpers mostly
// passthrough — but the regression cases (Equal byte-exact,
// IsUnder child / dotdot escape) still need to be locked in.

package pathutil

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeForOS_Passthrough(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/foo/bar", "/foo/bar"},
		{"/foo//bar", "/foo/bar"}, // Clean collapses "//"
		{"/foo/./bar", "/foo/bar"},
		{"/foo/../bar", "/bar"},
		{"/foo/", "/foo"}, // trailing slash stripped
		{"./foo", "foo"},
		{"foo", "foo"},
		{"/", "/"},
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

func TestNormalizeForOS_Empty(t *testing.T) {
	if _, err := NormalizeForOS(""); err == nil {
		t.Error("NormalizeForOS(\"\") returned nil error; expected errEmptyPath")
	}
}

func TestNormalizeForGit_Passthrough(t *testing.T) {
	// Unix NormalizeForGit must equal NormalizeForOS byte-exact —
	// git on Unix speaks POSIX, no MSYS layer to munge.
	in := "/home/dev/code/myrepo.nightme/login-state"
	got, err := NormalizeForGit(in)
	if err != nil {
		t.Fatalf("NormalizeForGit: %v", err)
	}
	if got != in {
		t.Errorf("NormalizeForGit(%q) = %q, want passthrough", in, got)
	}
}

func TestFromSlash_NoopOnUnix(t *testing.T) {
	cases := []string{"/foo/bar", "/home/dev", ""}
	for _, tc := range cases {
		if got := FromSlash(tc); got != tc {
			t.Errorf("FromSlash(%q) = %q, want passthrough", tc, got)
		}
	}
}

func TestToSlash_NoopOnUnix(t *testing.T) {
	cases := []string{"/foo/bar", "/home/dev", ""}
	for _, tc := range cases {
		if got := ToSlash(tc); got != tc {
			t.Errorf("ToSlash(%q) = %q, want passthrough", tc, got)
		}
	}
}

func TestEqual_ByteExact(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/foo", "/foo", true},
		{"/foo/", "/foo", true},                 // Clean strips trailing slash
		{"/foo//bar", "/foo/bar", true},         // Clean collapses //
		{"/foo/./bar", "/foo/bar", true},        // Clean collapses .
		{"/foo", "/bar", false},
		{"/foo", "/foobar", false},
		{"", "", true}, // Clean("") returns "."
		{"foo", "FOO", true}, // Unix IS case-sensitive but Clean doesn't lowercase — Equal here is byte-exact
	}
	for _, tc := range cases {
		if got := Equal(tc.a, tc.b); got != tc.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestEqual_CaseSensitiveOnUnix locks in the documented Unix
// behaviour (Unix file systems are case-sensitive, unlike Windows).
// Some users copy paths across machines; if we ever silently case-
// folded on Unix we'd break `/home/foo` vs `/home/Foo` distinctions.
func TestEqual_CaseSensitiveOnUnix(t *testing.T) {
	if Equal("/home/Foo", "/home/foo") {
		t.Error("Equal must be case-sensitive on Unix")
	}
}

func TestIsUnder_ChildOf(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/foo/bar", "/foo", true},
		{"/foo/bar/baz", "/foo", true},
		{"/foo", "/foo", true},   // same path
		{"/foo", "/foo/", true},  // trailing slash on parent
		{"/foobar", "/foo", false}, // naive-prefix trap
		{"/foo", "/bar", false},
		{"/foo/bar", "", false}, // empty parent
		{"", "/foo", false},     // empty child
		{"", "", false},
	}
	for _, tc := range cases {
		if got := IsUnder(tc.child, tc.parent); got != tc.want {
			t.Errorf("IsUnder(%q, %q) = %v, want %v", tc.child, tc.parent, got, tc.want)
		}
	}
}

// TestIsUnder_DotDotEscape is the regression for the classic
// "naive HasPrefix misses /.." trap. IsUnder("/foo/../bar", "/foo")
// must return false even though "/foo/../bar" starts with "/foo".
func TestIsUnder_DotDotEscape(t *testing.T) {
	// /foo/../bar normalises to /bar; IsUnder should detect that
	// the cleaned child escaped the parent.
	if IsUnder("/foo/../bar", "/foo") {
		t.Error("IsUnder(/foo/../bar, /foo) = true; want false (dotdot escapes parent)")
	}
}

func TestCleanJoinDirBase_ThinWrappers(t *testing.T) {
	// These are thin wrappers; verify they behave like filepath.*.
	if Clean("/foo//bar/") != filepath.Clean("/foo//bar/") {
		t.Error("Clean wrapper diverges from filepath.Clean")
	}
	if Join("/foo", "bar", "baz") != filepath.Join("/foo", "bar", "baz") {
		t.Error("Join wrapper diverges from filepath.Join")
	}
	if IsAbs("/foo") != filepath.IsAbs("/foo") {
		t.Error("IsAbs wrapper diverges from filepath.IsAbs")
	}
	if IsAbs("foo") != filepath.IsAbs("foo") {
		t.Error("IsAbs wrapper diverges from filepath.IsAbs (relative)")
	}
	if Base("/foo/bar") != filepath.Base("/foo/bar") {
		t.Error("Base wrapper diverges from filepath.Base")
	}
	if Dir("/foo/bar") != filepath.Dir("/foo/bar") {
		t.Error("Dir wrapper diverges from filepath.Dir")
	}
}

func TestNormalizeIMRichText_StripsAnchorTags(t *testing.T) {
	in := `<a href="https://example.com/foo">/home/dev/code</a>`
	want := "/home/dev/code"
	if got := NormalizeIMRichText(in); got != want {
		t.Errorf("NormalizeIMRichText(%q) = %q, want %q", in, got, want)
	}
}

func TestNormalizeIMRichText_FallsBackToHref(t *testing.T) {
	// Empty inner text → use href (with surrounding quotes stripped).
	in := `<a href="/home/dev">  </a>`
	got := NormalizeIMRichText(in)
	if got != "/home/dev" {
		t.Errorf("NormalizeIMRichText empty-inner fallback = %q, want /home/dev", got)
	}
}

func TestNormalizeInput_FullWidthASCII(t *testing.T) {
	// ／ (U+FF0F) is the full-width '/' — IME artefact.
	// NormalizeInput should turn it into '/'.
	in := "／foo／bar"
	if got := NormalizeInput(in); !strings.HasPrefix(got, "/") {
		t.Errorf("NormalizeInput(%q) = %q, expected '/' prefix", in, got)
	}
}

func TestNormalizeInput_PassesThroughCJKIdeographs(t *testing.T) {
	// Chinese directory names must pass through unchanged.
	in := "/home/dev/代码"
	if got := NormalizeInput(in); got != in {
		t.Errorf("NormalizeInput clobbered CJK ideographs: %q → %q", in, got)
	}
}
