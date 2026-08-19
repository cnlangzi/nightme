package cwd

import (
	"regexp"
	"strings"
)

// htmlLinkTag matches <a ...>text</a> and captures:
//   - group 1: the href="..." attribute value (or href='...' value)
//   - group 2: the inner text content
//
// This is the form IM clients (feishu, lark, slack, teams)
// emit when a user pastes a URL: <a href="https://...">cnlangzi/nightme</a>.
// Without stripping, the bot sees the raw markup and treats
// "<a ...>" as part of the path, which fails path validation.
// We extract the inner text — that's what the user actually
// sees in their chat and what they intend as the path.
//
// Note: the opening tag is NOT captured as a single group;
// href="..." is its own capture (group 1) so the fallback
// for empty inner text can use href directly. Inner text is
// group 2.
// (?i) keeps the regex tag-name case-insensitive: Slack / Teams
// sometimes emit <A HREF=...>label</A> (uppercase) — without (?i)
// the strip would miss them and /cwd would receive raw markup.
var htmlLinkTag = regexp.MustCompile(`(?i)<a\b[^>]*href=["']([^"']*)["'][^>]*>(.*?)</a>`)

// normalizePathInput rewrites IME-mangled input so users with
// a CJK input method active get the path they meant.
//
// Three transformations are applied:
//
//  0. Strip HTML link tags first. IM clients (feishu, lark,
//     slack, teams) wrap pasted URLs as <a href="...">visible
//     text</a>; we extract the visible text (group 2 of
//     htmlLinkTag). After this, "no workspace" / "set" links
//     in the chat show up as the bare path the user clicked.
//
//  1. The full-width ASCII block (U+FF01..U+FF5E) is mapped
//     to its half-width counterpart (U+0021..U+007E). This
//     catches the IME's habit of producing ／、：；（）"
//     when the user typed /, :, ;, (, ).
//
//  2. A short list of CJK punctuation marks with no ASCII
//     counterpart are mapped to the closest English
//     equivalent. This catches 。→., ,→, , ：→:, ?→?, etc.
//
// CJK ideographs (汉字) and other non-ASCII runes are passed
// through unchanged — we don't transliterate, and paths that
// legitimately contain non-ASCII characters (e.g. "我的项目")
// are left intact so the OS-level path resolver sees them.
//
// The function is a no-op for already-clean ASCII, but we
// don't bother short-circuiting: a /cwd invocation is rare
// compared to the per-character cost, and keeping the
// single-pass implementation makes the mapping table easy
// to audit.
func normalizePathInput(s string) string {
	s = stripIMRichText(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		// Full-width ASCII block → half-width ASCII.
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0)

		// Full-width space (U+3000) — not in the FF01..FF5E
		// block above, so we handle it explicitly.
		case r == '　':
			b.WriteRune(' ')

		// CJK punctuation → English equivalent. Ordered to
		// group by visual similarity so the table reads top-
		// to-bottom like a "what would the user have meant"
		// cheat sheet.
		case r == '。':
			b.WriteRune('.') // Chinese full stop
		case r == '，', r == '、':
			b.WriteRune(',') // Chinese comma / enumeration comma
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
			b.WriteRune('-') // em-dash → hyphen
		case r == '…':
			b.WriteString("...") // ellipsis → three dots

		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripIMRichText strips IM-emitted rich text markup that
// pollutes path arguments. The two patterns we care about:
//
//  1. <a ...>visible text</a> — emitted when the user pastes a
//     URL into the chat input (feishu/lark/slack/teams all do
//     this). The visible text is what the user sees and
//     intended as the path; the markup itself is IM-internal.
//
//  2. Bare URLs that the IM sent without link-card wrapping
//     (when the input method itself inserts a URL string).
//     We don't try to map https://host/path to /path here —
//     path resolution at the OS level handles absolute URLs
//     poorly and the user almost certainly meant the bare
//     path. The caller can extend this later if a real
//     use-case surfaces.
//
// Multiple <a> tags are handled: each becomes its inner text,
// concatenated in source order (the IM normally emits one).
// Nested tags (rare; would require recursive parsing) are not
// supported — the simple regex strips the outer <a> and leaves
// any inner tag literals in the inner text, which the path
// resolver will reject with "no such directory" and the error
// reply tells the user to re-type. Good enough for v1.
func stripIMRichText(s string) string {
	return htmlLinkTag.ReplaceAllStringFunc(s, func(match string) string {
		// Keep the inner-text group (the user-visible label).
		// htmlLinkTag is non-greedy by virtue of the (.*?) capture;
		// submatch[1] is href, submatch[2] is inner text.
		// When the inner text itself is empty (some clients
		// emit <a href="..."></a> with no label), fall back to
		// the href — still better than passing the markup
		// through to the path resolver.
		sm := htmlLinkTag.FindStringSubmatch(match)
		if len(sm) < 3 {
			return ""
		}
		inner := strings.TrimSpace(sm[2])
		if inner == "" {
			// href is sm[1]; strip surrounding quotes if
			// present (the regex's [^>]* capture is greedy so
			// the leading/trailing quotes are part of the
			// captured text).
			href := strings.Trim(sm[1], `"'`)
			inner = href
		}
		return inner
	})
}
