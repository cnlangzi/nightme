// Cross-platform helpers that don't depend on platform-specific
// rules. Platform-specific logic (drive letters, UNC roots, root-
// relative, drive-relative rejection) lives in path_unix.go and
// path_windows.go. This file holds:
//
//   - IME / CJK input normalization (mirrors cwd/normalize.go's
//     contract — kept here for callers that want path-shape cleanup
//     without the cwd-specific HOME-relative semantics)
//   - IsAbs / Join / Clean / Base / Dir: thin wrappers over the
//     stdlib's filepath package that exist for the same reason
//     FromSlash / ToSlash do — so grep `pathutil.Clean` finds every
//     caller and the "must use pathutil" rule (SPEC.md §13.3.1)
//     has zero exceptions.
//
// See F-PATHUTIL-001 §13.3.1 for the full mandatory-use table.
package pathutil

import (
	"path/filepath"
	"regexp"
	"strings"
)

// htmlLinkTag mirrors cwd/normalize.go::htmlLinkTag — kept in sync
// because the same IM-emitted markup pollutes path arguments
// everywhere users can paste a path (cwd, gtw fix, future
// attachment upload commands, etc.). Sharing the regex across
// packages would require either an awkward import or an
// extract-to-string-constant dance, so we duplicate it. The two
// copies drift if either side's chat-client list changes — bump
// both when adding a new IM markup pattern.
var htmlLinkTag = regexp.MustCompile(`(?i)<a\b[^>]*href=["']([^"']*)["'][^>]*>(.*?)</a>`)

// NormalizeIMRichText strips IM-emitted rich-text markup that
// pollutes path arguments:
//
//  1. <a ...>visible text</a> — when the user pastes a URL into
//     chat (feishu / lark / slack / teams all do this). Inner
//     text is what the user saw; we keep it.
//  2. Bare URLs (no link-card wrapper): passed through unchanged
//     here; URL-vs-path disambiguation is the resolver's job.
//
// Multiple <a> tags concatenate inner text in source order.
// Nested tags (rare) are not handled — the outer <a> is stripped
// and inner tag literals pass through; the path resolver will
// reject them with a clearer message than this regex would.
//
// Returns the cleaned string. Always returns non-empty when input
// was non-empty (the regex fallback uses href as inner text when
// inner is empty).
func NormalizeIMRichText(s string) string {
	return htmlLinkTag.ReplaceAllStringFunc(s, func(match string) string {
		sm := htmlLinkTag.FindStringSubmatch(match)
		if len(sm) < 3 {
			return ""
		}
		inner := strings.TrimSpace(sm[2])
		if inner == "" {
			inner = strings.Trim(sm[1], `"'`)
		}
		return inner
	})
}

// NormalizeInput runs NormalizeIMRichText + full-width-ASCII →
// half-width + CJK punctuation → English. Mirrors cwd/normalize.go
// ::normalizePathInput's contract (see that file for the
// full mapping table); duplicated here so pathutil callers can
// normalize without taking a dependency on the cwd package.
//
// This is "shape" normalization — it doesn't decide whether the
// string is an absolute path or a $HOME-relative one. That's
// NormalizeForOS / cwd::resolvePath's job.
func NormalizeInput(s string) string {
	s = NormalizeIMRichText(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0) // full-width ASCII → half-width
		case r == '　':
			b.WriteRune(' ') // full-width space
		case r == '。':
			b.WriteRune('.')
		case r == '，', r == '、':
			b.WriteRune(',')
		case r == '；':
			b.WriteRune(';')
		case r == '：':
			b.WriteRune(':')
		case r == '？':
			b.WriteRune('?')
		case r == '！':
			b.WriteRune('!')
		case r == '“', r == '”':
			b.WriteRune('"')
		case r == '‘', r == '’':
			b.WriteRune('\'')
		case r == '（':
			b.WriteRune('(')
		case r == '）':
			b.WriteRune(')')
		case r == '《':
			b.WriteRune('<')
		case r == '》':
			b.WriteRune('>')
		case r == '【':
			b.WriteRune('[')
		case r == '】':
			b.WriteRune(']')
		case r == '—':
			b.WriteRune('-')
		case r == '…':
			b.WriteString("...")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Clean is a thin wrapper over filepath.Clean. See F-PATHUTIL-001
// §13.3.1 — every site that wants "cleaned path" must call
// pathutil.Clean, not filepath.Clean, so the platform-specific
// helpers can grow additional rules (long-path handling, UNC
// canonicalization, etc.) without touching every call site.
func Clean(p string) string { return filepath.Clean(p) }

// Join is a thin wrapper over filepath.Join. Same rationale as
// Clean: one import point so the centralized rule has zero
// exceptions.
func Join(elem ...string) string { return filepath.Join(elem...) }

// IsAbs is a thin wrapper over filepath.IsAbs. Same rationale.
// On Windows, returns true for "C:\foo", "C:/foo", "\\foo", "\foo"
// (after Clean), but false for "C:foo" (drive-relative). Use
// NormalizeForOS when you want to ALSO canonicalize the form.
func IsAbs(p string) bool { return filepath.IsAbs(p) }

// Base is a thin wrapper over filepath.Base.
func Base(p string) string { return filepath.Base(p) }

// Dir is a thin wrapper over filepath.Dir.
func Dir(p string) string { return filepath.Dir(p) }
