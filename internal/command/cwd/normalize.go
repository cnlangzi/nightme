package cwd

import "strings"

// normalizePathInput rewrites IME-mangled input so users with
// a CJK input method active get the path they meant.
//
// Two transformations are applied:
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
