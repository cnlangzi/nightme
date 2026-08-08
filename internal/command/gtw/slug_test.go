package gtw

import "testing"

// TestDeriveBranchFromTitle_BareTitle covers the most common
// case: issue title has no conventional-commit prefix; the
// whole title is slugified and returned verbatim (no `fix-`
// prefix — that was the old behaviour).
//
// Note: empty / whitespace-only titles fall back to `fix-<id>`
// per the F-XX rule "if conversion result is empty, use
// fix-<id>". This test only covers non-empty bare titles.
func TestDeriveBranchFromTitle_BareTitle(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"Login state expiration", "login-state-expiration"},
		{"Add a delete button", "add-a-delete-button"},
		{"Fix: foo bar baz", "fix-foo-bar-baz"},
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
		{"feat(login): state expires", "feat-login-state-expires"},
		{"feat: state expires", "feat-state-expires"},
		{"fix(api): handle null", "fix-api-handle-null"},
		{"chore: bump deps", "chore-bump-deps"},
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
		want string
	}{
		{"[BUG] state expires", "bug-state-expires"},
		{"[BUG-123] state expires", "bug-123-state-expires"},
		{"[FIX] handle null", "fix-handle-null"},
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
		{"登录 only", 42, "only"}, // "登录" dropped, "only" survives
	}
	for _, tc := range cases {
		got := DeriveBranchFromTitle(tc.title, tc.issue)
		if got != tc.want {
			t.Errorf("DeriveBranchFromTitle(%q, %d) = %q, want %q", tc.title, tc.issue, got, tc.want)
		}
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
		{"中文", "", true},                 // empty slug → error
		{"", "", true},                     // empty input → error
		{"__..__", "", true},               // all non-ASCII-valid → empty slug
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