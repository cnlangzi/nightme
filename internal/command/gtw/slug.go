package gtw

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/cnlangzi/nightme/internal/pathutil"
)

// ccPrefixRE matches conventional-commit / bracketed-tag prefixes
// at the start of an issue title:
//
//	"feat(login): state expires" → matches; prefix=feat, scope=login
//	"feat: state expires"        → matches; prefix=feat, scope=""
//	"[BUG] state expires"        → matches; bracket=BUG
//	"[BUG-123] state expires"    → matches; bracket=BUG-123
//	"Login state expires"        → no match
//
// Detection is intentionally minimal — we accept what's standard
// in the wild, not a full grammar. Edge cases fall through to
// the whole-title slugify.
var ccPrefixRE = regexp.MustCompile(
	`^(?:` +
		`\[(?P<bracket>[A-Z][A-Z0-9_-]*)\]` +
		`|(?P<type>[a-z]+)(?:\((?P<scope>[^)]+)\))?:` +
		`)\s*`,
)

// slugify converts an input string into a git-safe slug component.
// The transformation is:
//
//  1. Lowercase.
//  2. Replace any non-[a-z0-9] run with a single "-" (collapse
//     runs to avoid "foo--bar" from "foo - bar").
//  3. Strip leading and trailing "-" so the result does not start
//     or end with a separator.
//
// Empty result is returned for inputs that have no ASCII letter
// or digit at all (CJK, emoji-only, whitespace-only). Callers
// should treat empty as a special-case signal (e.g. fall back to
// `fix-<id>` for ID mode, or return an error for local mode).
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

// DeriveBranchFromTitle produces the branch name for the ID-mode
// `/gtw fix <issue-id>` flow. The branch is derived from the
// remote issue's title with these rules:
//
//  1. Try conventional-commit / bracketed-tag prefix detection
//     (see ccPrefixRE). If matched, extract the prefix
//     components and convert the `:` / `()` separators to `-`.
//     Then slugify the remainder and concatenate. The branch
//     ends up looking like `feat-login-state-expires` (or
//     `bug-state-expires` for `[BUG] state expires`).
//  2. If no prefix matches, run the whole title through
//     slugify() (drop non-ASCII, lowercase, dash-collapse).
//  3. If the result is empty (e.g. the title was pure CJK / emoji),
//     fall back to `fix-<id>` — this preserves the "we still need
//     to make progress on issue #N" signal even when the title
//     yields no usable ref characters.
//
// F-XX: this replaces the old DeriveBranch which always emitted
// `fix/<id>-<slug>`. The new convention puts no `fix/` ref
// namespace — the slug itself is the branch name.
func DeriveBranchFromTitle(title string, issueID int) string {
	prefix := ""
	rest := title

	if m := ccPrefixRE.FindStringSubmatchIndex(title); m != nil {
		// Pull matched groups out of title via the FindStringSubmatch
		// capture groups. SubmatchIndex returns [start,end] pairs in
		// the order the named groups appear in the regex; we index
		// by subexp index (1-based) to read each named group.
		groups := ccPrefixRE.FindStringSubmatch(title)
		bracket := groups[ccPrefixRE.SubexpIndex("bracket")]
		typ := groups[ccPrefixRE.SubexpIndex("type")]
		scope := groups[ccPrefixRE.SubexpIndex("scope")]

		switch {
		case bracket != "":
			prefix = strings.ToLower(bracket) + "-"
		case typ != "":
			if scope != "" {
				prefix = typ + "-" + scope + "-"
			} else {
				prefix = typ + "-"
			}
		}
		// Slice off the matched prefix + trailing whitespace from
		// the input that we'll slugify.
		rest = title[m[1]:]
	}

	slug := slugify(rest)
	if slug == "" {
		// Whole-title fallback when even the prefix is unusable.
		return fmt.Sprintf("fix-%d", issueID)
	}
	return prefix + slug
}

// DeriveBranchFromName produces the branch name for the local-mode
// `/gtw fix --name <branch>` flow. The user supplies the literal
// name; we slugify it directly (no conventional-commit detection —
// the user is choosing the name themselves, not naming an issue).
//
// Returns an error when the slug is empty (the input was empty /
// pure-CJK / pure-symbol). Local mode has no issue id to fall
// back to, so we surface a clear error and let the user retry
// with a valid name.
func DeriveBranchFromName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("branch name is empty")
	}
	slug := slugify(name)
	if slug == "" {
		return "", fmt.Errorf(
			"branch name %q produces empty slug — use ASCII letters / digits / dashes",
			name,
		)
	}
	return slug, nil
}

// WorktreePath builds the absolute path of the per-fix worktree
// directory under <sibling-of-repo>/<repo>.nightme/<slug>/. F-45 §6.1.
//
//	repoRoot = "/home/dev/code/nightme"
//	slug     = "login-state-expiration"     (F-XX; was "42-login-state-expiration")
//	→ "/home/dev/code/nightme.nightme/login-state-expiration"
//
// The sibling layout puts the worktree OUTSIDE the repository root
// so build tools, linters, and codegraph don't see duplicate
// sources (F-45 §6.2).
func WorktreePath(repoRoot, slug string) string {
	// F-PATHUTIL-001 §5.2: route through pathutil so the
	// platform-specific normalization (forward-slash → backslash on
	// Windows) is consistent with every other path operation in the
	// package. RepoRoot is what `git rev-parse --show-toplevel`
	// returns, which on Windows is "F:/foo" (forward slashes);
	// without the normalization the worktree path is a mixed-form
	// string that confuses downstream `git worktree add` /
	// `git worktree remove`.
	repoRoot, _ = pathutil.NormalizeForOS(repoRoot)
	parent := pathutil.Dir(repoRoot)
	repoName := pathutil.Base(repoRoot)
	return pathutil.Join(parent, repoName+"."+LabelPrefix, slug)
}


