package gtw

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

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

// maxBranchBytes caps a derived branch name.
//
// 255 bytes is the hard per-component limit (NAME_MAX on ext4 /
// APFS); the same limit applies to the loose ref file under
// .git/refs/heads/ and to .git/worktrees/<branch>/. 128 is half of
// that, which also leaves the Windows MAX_PATH budget about 132
// characters for <parent>\<repo>.nightme\ — enough for repository
// paths up to roughly 115 characters — while still fitting every
// realistic conventional-commit title.
const maxBranchBytes = 128

// isASCIIAlnum reports whether b is an ASCII letter or digit.
func isASCIIAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// capBranchBytes truncates a derived branch name to maxBranchBytes.
//
// The cut drops trailing content only — the head of the name (the
// issue id in ID mode) is what identifies the branch. The cut lands
// on a UTF-8 rune boundary (the conventional-commit scope is
// carried unslugified and can hold multi-byte characters), and the
// ASCII tail git refuses at the end of a ref component is trimmed:
// a trailing "." or space makes `git worktree add` fail with
// "invalid branch name", and a trailing dash reads as a cut slug.
// A multi-byte rune is kept — git accepts one at the end of a ref.
func capBranchBytes(name string) string {
	if len(name) <= maxBranchBytes {
		return name
	}
	cut := name[:maxBranchBytes]
	// A byte-exact cut can split a multi-byte rune; drop the
	// incomplete trailing rune so the result stays valid UTF-8.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	for len(cut) > 0 && cut[len(cut)-1] < utf8.RuneSelf && !isASCIIAlnum(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// DeriveBranchFromTitle produces the branch name for the ID-mode
// `/gtw fix <issue-id>` flow. The branch is derived from the
// remote issue's title with these rules:
//
//  1. Try conventional-commit / bracketed-tag prefix detection
//     (see ccPrefixRE). If matched, extract the prefix
//     components and convert the `:` / `()` separators to `-`.
//     Then slugify the remainder and concatenate. The branch
//     ends up looking like `42-feat-login-state-expires` (or
//     `42-bug-state-expires` for `[BUG] state expires`).
//  2. If no prefix matches, run the whole title through
//     slugify() (drop non-ASCII, lowercase, dash-collapse).
//  3. If the result is empty (e.g. the title was pure CJK / emoji),
//     fall back to `fix-<id>` — this preserves the "we still need
//     to make progress on issue #N" signal even when the title
//     yields no usable ref characters.
//
// The issue id leads the branch name. slugify() drops every
// non-ASCII rune, so two distinct titles can collapse onto the
// same slug — "用户登录 bug" and "用户登出 bug" both yield "bug".
// The id makes the branch unique per issue regardless of how the
// title degrades, and lets `git branch | grep '^<id>-'` list
// every branch belonging to one issue.
//
// The result is capped at maxBranchBytes (128 bytes, measured in
// bytes — the conventional-commit scope is carried unslugified and
// can hold multi-byte characters). A branch name is the last path
// component of the fix's worktree directory, of
// .git/worktrees/<branch>/ and of the loose ref file, and the
// filesystem allows 255 bytes per component. An over-long title
// loses its tail; the issue id survives.
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
		// The id is already in the name, so the branch stays unique.
		return capBranchBytes(fmt.Sprintf("fix-%d", issueID))
	}
	return capBranchBytes(fmt.Sprintf("%d-%s", issueID, prefix+slug))
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
//
// The slug is the whole branch name here — no issue id leads it,
// because local mode is not tied to an issue. Two local fixes whose
// names slugify alike therefore land on one branch; the
// branch-exists check rejects the second one.
//
// The result is capped at maxBranchBytes (128 bytes, measured in
// bytes); an over-long name loses its tail.
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
	return capBranchBytes(slug), nil
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
