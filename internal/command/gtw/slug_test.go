package gtw

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestDeriveBranchFromTitle_BareTitle covers the most common
// case: issue title has no conventional-commit prefix. The whole
// title is slugified and the issue id leads the branch name.
//
// Empty / whitespace-only titles fall back to `fix-<id>`; that case
// is covered by TestDeriveBranchFromTitle_CJKFallback.
func TestDeriveBranchFromTitle_BareTitle(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"Login state expiration", "42-login-state-expiration"},
		{"Add a delete button", "42-add-a-delete-button"},
		{"Fix: foo bar baz", "42-fix-foo-bar-baz"},
	}
	for _, tc := range cases {
		got := DeriveBranchFromTitle(tc.title, 42)
		if got != tc.want {
			t.Errorf("DeriveBranchFromTitle(%q, 42) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

// TestDeriveBranchFromTitle_ConventionalCommit covers the
// conventional-commit prefix detection rules (type / type(scope):).
func TestDeriveBranchFromTitle_ConventionalCommit(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"feat(login): state expires", "42-feat-login-state-expires"},
		{"feat: state expires", "42-feat-state-expires"},
		{"fix(api): handle null", "42-fix-api-handle-null"},
		{"chore: bump deps", "42-chore-bump-deps"},
	}
	for _, tc := range cases {
		got := DeriveBranchFromTitle(tc.title, 42)
		if got != tc.want {
			t.Errorf("DeriveBranchFromTitle(%q, 42) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

// TestDeriveBranchFromTitle_BracketedTag covers `[TAG]` style
// prefixes — common in some team conventions.
func TestDeriveBranchFromTitle_BracketedTag(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"[BUG] state expires", "42-bug-state-expires"},
		{"[BUG-123] state expires", "42-bug-123-state-expires"},
		{"[FIX] handle null", "42-fix-handle-null"},
	}
	for _, tc := range cases {
		got := DeriveBranchFromTitle(tc.title, 42)
		if got != tc.want {
			t.Errorf("DeriveBranchFromTitle(%q, 42) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

// TestDeriveBranchFromTitle_CJKFallback covers the case where
// the slugified portion is empty (pure-CJK / emoji title) and
// we fall back to `fix-<id>` so the worktree can still be
// created. The "登录 only" case verifies that a partial CJK
// title with surviving ASCII produces a partial slug (NOT the
// fallback) — only fully-empty slugs fall back.
func TestDeriveBranchFromTitle_CJKFallback(t *testing.T) {
	cases := []struct {
		title string
		issue int
		want  string
	}{
		{"登录状态过期", 42, "fix-42"},
		{"🎉", 7, "fix-7"},
		{"中文标点：，。！？", 100, "fix-100"},
		{"登录 only", 42, "42-only"}, // "登录" dropped, "only" survives
	}
	for _, tc := range cases {
		got := DeriveBranchFromTitle(tc.title, tc.issue)
		if got != tc.want {
			t.Errorf("DeriveBranchFromTitle(%q, %d) = %q, want %q", tc.title, tc.issue, got, tc.want)
		}
	}
}

// TestDeriveBranchFromTitle_NonASCIITitlesStayUnique pins the
// uniqueness guarantee: slugify() drops every non-ASCII rune, so
// titles that differ only in their CJK part collapse onto one
// slug. The issue id keeps the branches apart.
func TestDeriveBranchFromTitle_NonASCIITitlesStayUnique(t *testing.T) {
	a := DeriveBranchFromTitle("用户登录 bug", 42)
	b := DeriveBranchFromTitle("用户登出 bug", 43)
	if a == b {
		t.Fatalf("distinct issues share a branch name: %q", a)
	}
	if a != "42-bug" || b != "43-bug" {
		t.Errorf("got %q and %q, want %q and %q", a, b, "42-bug", "43-bug")
	}
}

// TestDeriveBranchFromName covers the local-mode branch
// derivation: user-supplied literal name, no CC detection,
// empty input → error.
func TestDeriveBranchFromName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"login-fix", "login-fix", false},
		{"Login Fix", "login-fix", false}, // upper → lower, space → dash
		{"中文", "", true},                  // empty slug → error
		{"", "", true},                    // empty input → error
		{"__..__", "", true},              // all non-ASCII-valid → empty slug
		{"a-b-c-1", "a-b-c-1", false},
	}
	for _, tc := range cases {
		got, err := DeriveBranchFromName(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("DeriveBranchFromName(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("DeriveBranchFromName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDeriveBranchFromTitle_CapsLongTitle covers the byte cap on
// the issue-title path. A long title slugifies to a name well past
// the 255 bytes a filesystem path component holds, so the
// derivation has to cut it — and cut the tail only, so the issue
// id that leads the name survives.
func TestDeriveBranchFromTitle_CapsLongTitle(t *testing.T) {
	title := strings.Repeat("login state expiration ", 17) + "abcdefghi"
	got := DeriveBranchFromTitle(title, 42)

	if len(title) != 400 {
		t.Fatalf("test title is %d characters, want 400", len(title))
	}
	if len(got) > maxBranchBytes {
		t.Errorf("branch is %d bytes, want <= %d", len(got), maxBranchBytes)
	}
	if !strings.HasPrefix(got, "42-") {
		t.Errorf("branch %q lost the issue id prefix", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("branch %q is not valid UTF-8", got)
	}
	// The cut removes trailing content only: the result is a prefix
	// of the untruncated name.
	full := fmt.Sprintf("42-%s", slugify(title))
	if len(full) <= maxBranchBytes {
		t.Fatalf("test title slugifies to %d bytes — nothing to truncate", len(full))
	}
	if !strings.HasPrefix(full, got) {
		t.Errorf("branch %q is not a prefix of %q", got, full)
	}
	if last := got[len(got)-1]; !isASCIIAlnum(last) {
		t.Errorf("branch %q ends with %q, want an alphanumeric byte", got, string(last))
	}
}

// TestDeriveBranchFromTitle_CapCountsBytesNotRunes covers the
// limit's unit. The conventional-commit scope is carried into the
// branch unslugified, so it can hold multi-byte characters: a
// rune-based limit would let the byte length blow past 128.
func TestDeriveBranchFromTitle_CapCountsBytesNotRunes(t *testing.T) {
	scope := strings.Repeat("核", 20)
	title := "feat(" + scope + "): " + strings.Repeat("y", 200)
	got := DeriveBranchFromTitle(title, 42)

	if len(got) > maxBranchBytes {
		t.Errorf("branch is %d bytes, want <= %d", len(got), maxBranchBytes)
	}
	if !utf8.ValidString(got) {
		t.Errorf("branch %q is not valid UTF-8", got)
	}
	runes := utf8.RuneCountInString(got)
	if runes >= len(got) {
		t.Errorf("branch %q has %d runes for %d bytes — the cap did not bind on bytes",
			got, runes, len(got))
	}
	if runes > maxBranchBytes {
		t.Errorf("branch has %d runes, want <= %d", runes, maxBranchBytes)
	}
}

// TestDeriveBranchFromTitle_CutKeepsRuneBoundary covers the cut
// landing inside a multi-byte rune: the byte-exact cut would split
// the rune, so the derivation backs up to the rune boundary and
// drops the incomplete rune.
func TestDeriveBranchFromTitle_CutKeepsRuneBoundary(t *testing.T) {
	scope := strings.Repeat("核", 60)
	title := "feat(" + scope + "): tail"
	got := DeriveBranchFromTitle(title, 7)

	// "7-feat-" is 7 bytes; the scope starts there. The byte cut at
	// 128 lands 121 bytes into the scope — one byte into the 41st
	// rune — so the complete runes are the first 40.
	want := "7-feat-" + strings.Repeat("核", 40)
	if got != want {
		t.Errorf("DeriveBranchFromTitle = %q (%d bytes), want %q (%d bytes)",
			got, len(got), want, len(want))
	}
	if !utf8.ValidString(got) {
		t.Errorf("branch %q is not valid UTF-8", got)
	}
	if len(got) > maxBranchBytes {
		t.Errorf("branch is %d bytes, want <= %d", len(got), maxBranchBytes)
	}
}

// TestDeriveBranchFromTitle_CapTrimsRefInvalidTail covers the tail
// trim. The conventional-commit scope is carried unslugified, so the
// byte cut can land on a character git refuses at the end of a ref
// component — a trailing "." or space makes `git worktree add` fail
// with "invalid branch name", the same opaque failure the cap
// exists to remove.
func TestDeriveBranchFromTitle_CapTrimsRefInvalidTail(t *testing.T) {
	scope := strings.Repeat("a", 118) + "."
	title := "feat(" + scope + "): " + strings.Repeat("y", 100)
	got := DeriveBranchFromTitle(title, 42)

	// "42-feat-" is 8 bytes; the scope follows. The byte cut at 128
	// lands on the dash that closes the scope, one byte past the
	// scope's trailing dot — both are trimmed.
	want := "42-feat-" + strings.Repeat("a", 118)
	if got != want {
		t.Errorf("DeriveBranchFromTitle = %q (%d bytes), want %q (%d bytes)",
			got, len(got), want, len(want))
	}
	if last := got[len(got)-1]; !isASCIIAlnum(last) {
		t.Errorf("branch %q ends with %q, want an alphanumeric byte", got, string(last))
	}
	if _, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command("git", "check-ref-format", "--allow-onelevel", got)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("git rejects %q as a ref name: %v\n%s", got, err, out)
		}
	}
}

// TestDeriveBranchFromTitle_AtLimitUnchanged covers the boundary:
// a name that already fits the budget is returned byte-identical.
func TestDeriveBranchFromTitle_AtLimitUnchanged(t *testing.T) {
	want := "42-" + strings.Repeat("x", maxBranchBytes-3)
	got := DeriveBranchFromTitle(strings.Repeat("x", maxBranchBytes-3), 42)

	if len(want) != maxBranchBytes {
		t.Fatalf("test setup produced %d bytes, want %d", len(want), maxBranchBytes)
	}
	if got != want {
		t.Errorf("DeriveBranchFromTitle = %q, want %q (byte-identical)", got, want)
	}
}

// TestDeriveBranchFromName_CapsLongName covers the byte cap on the
// user-supplied path. The cut lands on the dash that ends the
// 16th repetition, so the trailing dash is trimmed and the result
// is 127 bytes.
func TestDeriveBranchFromName_CapsLongName(t *testing.T) {
	name := strings.Repeat("abc-def-", 20)
	got, err := DeriveBranchFromName(name)
	if err != nil {
		t.Fatalf("DeriveBranchFromName: %v", err)
	}

	if len(got) > maxBranchBytes {
		t.Errorf("branch is %d bytes, want <= %d", len(got), maxBranchBytes)
	}
	if len(got) != maxBranchBytes-1 {
		t.Errorf("branch is %d bytes, want %d (the cut dash is trimmed)",
			len(got), maxBranchBytes-1)
	}
	if !utf8.ValidString(got) {
		t.Errorf("branch %q is not valid UTF-8", got)
	}
	full := slugify(name)
	if !strings.HasPrefix(full, got) {
		t.Errorf("branch %q is not a prefix of %q", got, full)
	}
	if last := got[len(got)-1]; !isASCIIAlnum(last) {
		t.Errorf("branch %q ends with %q, want an alphanumeric byte", got, string(last))
	}
}

// TestDeriveBranchFromName_AtLimitUnchanged covers the boundary:
// a name that already fits the budget is returned byte-identical.
func TestDeriveBranchFromName_AtLimitUnchanged(t *testing.T) {
	name := strings.Repeat("a", maxBranchBytes)
	got, err := DeriveBranchFromName(name)
	if err != nil {
		t.Fatalf("DeriveBranchFromName: %v", err)
	}
	if got != name {
		t.Errorf("DeriveBranchFromName = %q, want %q (byte-identical)", got, name)
	}
}

// TestDeriveBranchFromName_Deterministic covers the agreement
// between the command's quick validation and the fix flow's
// derivation: both derive from the same pure function, so the
// same input always yields the same name — whatever the input's
// length.
func TestDeriveBranchFromName_Deterministic(t *testing.T) {
	names := []string{
		"login-fix",
		strings.Repeat("long-branch-name-", 20),
		strings.Repeat("a", maxBranchBytes),
		strings.Repeat("a", maxBranchBytes+1),
	}
	for _, name := range names {
		quick, err := DeriveBranchFromName(name)
		if err != nil {
			t.Fatalf("DeriveBranchFromName(%q): %v", name, err)
		}
		derived, err := DeriveBranchFromName(name)
		if err != nil {
			t.Fatalf("DeriveBranchFromName(%q): %v", name, err)
		}
		if quick != derived {
			t.Errorf("quick validation derived %q, fix flow derived %q", quick, derived)
		}
	}
}
