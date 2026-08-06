package gtw

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// SlugMaxLen caps the length of the slug portion of branch / worktree
// names. Git refuses refs over 100 bytes, and 30 leaves room for the
// `fix/<id>-` prefix and the `-v<N>` suffix on retry.
const SlugMaxLen = 30

// IssueIDPrefix is the constant prefix on every /gtw fix branch
// name (`fix/42-...`). Hard-coded; teams that need a different
// namespace should fork this constant.
const IssueIDPrefix = "fix"

// DeriveBranch builds the canonical branch name for a /gtw fix claim.
//
//	issue=42, title="Login state expiration" → "fix/42-login-state-expiration"
//	issue=42, title=""                      → "fix/42"   (no slug)
//
// The slug portion is the lowercase, ASCII-only, dash-joined form
// of the title, truncated to SlugMaxLen runes. The full name is
// guaranteed to be a valid git ref-name component (no spaces, no
// leading dashes, no trailing slashes).
func DeriveBranch(issueID int, title string) string {
	slug := DeriveSlug(issueID, title)
	// DeriveSlug returns either "<id>" (no usable title letters)
	// or a clean slug. If the slug already starts with the issue
	// id (legacy callers / future expansion), drop the duplicate.
	if strings.HasPrefix(slug, fmt.Sprintf("%d-", issueID)) || slug == fmt.Sprintf("%d", issueID) {
		// Slug already includes the id; don't double-prepend.
		return fmt.Sprintf("%s/%s", IssueIDPrefix, slug)
	}
	return fmt.Sprintf("%s/%d-%s", IssueIDPrefix, issueID, slug)
}

// DeriveSlug returns the slug portion of the worktree name
// (the part after "<issueID>-"). Returns the bare issue id as
// a string when the title has no usable ASCII letters. Used
// both for the branch name and for the worktree directory
// name (F-45 §6.4); DeriveBranch / WorktreePath compose the
// full string with the issue-id prefix.
func DeriveSlug(issueID int, title string) string {
	cleaned := slugify(title)
	if cleaned == "" {
		return fmt.Sprintf("%d", issueID)
	}
	// Reserve room for the "<id>-" prefix that the caller will
	// prepend. We truncate the slug component (not the final
	// composed string) so DeriveBranch can compute its own length.
	budget := max(SlugMaxLen-len(fmt.Sprintf("%d-", issueID)), 0)
	if len(cleaned) > budget {
		cleaned = cleaned[:budget]
		cleaned = strings.TrimRight(cleaned, "-")
	}
	return cleaned
}

// slugify converts an issue title into a git-safe slug component.
// The transformation is:
//
//  1. Lowercase.
//  2. Replace any non-[a-z0-9] run with a single "-" (collapse
//     runs to avoid "foo--bar" from "foo - bar").
//  3. Strip leading and trailing "-" so the result does not start
//     or end with a separator (which would produce a ref like
//     "fix/42--foo" or "fix/42-foo-").
//
// Empty result is returned for titles that have no ASCII letter
// or digit at all (CJK, emoji-only, whitespace-only). Callers
// should treat empty as "use bare issue id".
func slugify(title string) string {
	if title == "" {
		return ""
	}
	lower := strings.ToLower(title)
	var b strings.Builder
	b.Grow(len(lower))
	prevDash := false
	for _, r := range lower {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case unicode.IsSpace(r) || r == '_' || r == '.' || r == '/' || r == '\\':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// Drop non-ASCII runes (CJK, accented Latin, emoji)
			// rather than transliterating — we have no
			// transliteration library in v1, and dropping
			// preserves reversibility (no info loss on the
			// platform side; the title is still in the issue).
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := b.String()
	out = strings.Trim(out, "-")
	return out
}

// WorktreePath builds the absolute path of the per-fix worktree
// directory under <sibling-of-repo>/<repo>.nightme/<slug>/. F-45 §6.1.
//
//	repoRoot = "/home/dev/code/nightme"
//	slug     = "42-login-state-expiration"
//	→ "/home/dev/code/nightme.nightme/42-login-state-expiration"
//
// The sibling layout puts the worktree OUTSIDE the repository root
// so build tools, linters, and codegraph don't see duplicate
// sources (F-45 §6.2).
func WorktreePath(repoRoot, slug string) string {
	repoRoot = filepath.Clean(repoRoot)
	parent := filepath.Dir(repoRoot)
	repoName := filepath.Base(repoRoot)
	return filepath.Join(parent, repoName+"."+LabelPrefix, slug)
}

// BranchVariant returns a `-v2`/`-v3`/... variant of the branch
// name. Used to handle the "branch already exists" decision card
// (§5.3.1) without overwriting the user's existing work.
func BranchVariant(branch string, n int) string {
	if n < 2 {
		return branch
	}
	return fmt.Sprintf("%s-v%d", branch, n)
}
