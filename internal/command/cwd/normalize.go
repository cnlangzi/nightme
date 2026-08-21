// Cross-platform path-input normalization for /cwd.
//
// F-PATHUTIL-001 §13.3.3: this used to inline a regex for IM-
// emitted <a> tags, a full-width-ASCII mapping table, and a CJK
// punctuation mapping. All three now live in pathutil
// (pathutil.NormalizeIMRichText strips <a> tags,
// pathutil.NormalizeInput handles the full-width and CJK
// mappings). cwd's normalizePathInput becomes a one-line
// delegate so the "must use pathutil" rule (SPEC §13.3.1) has
// zero exceptions, and any future IM client whose markup pollutes
// paths is handled by a single fix in pathutil rather than a
// scattered set of copies in cwd / gtw / etc.
//
// The mapping table is documented inline at pathutil.NormalizeInput;
// the regex is at pathutil.NormalizeIMRichText. See F-PATHUTIL-001
// §3.5 for the API contract.
package cwd

import "github.com/cnlangzi/nightme/internal/pathutil"

// normalizePathInput rewrites IME-mangled input so users with
// a CJK input method active get the path they meant.
//
// After F-PATHUTIL-001 this is a one-line delegate to
// pathutil.NormalizeInput, which itself composes
// pathutil.NormalizeIMRichText + the full-width-ASCII / CJK
// punctuation mapping. The behavior is byte-identical to the
// pre-migration implementation; the table moved to pathutil
// so future commands that accept path arguments (gtw fix,
// attachment uploads, etc.) can call it without taking a
// dependency on the cwd package.
//
// CJK ideographs (汉字) and other non-ASCII runes are passed
// through unchanged — we don't transliterate, and paths that
// legitimately contain non-ASCII characters (e.g. "我的项目")
// are left intact so the OS-level path resolver sees them.
//
// The function is a no-op for already-clean ASCII, but we
// don't bother short-circuiting: a /cwd invocation is rare
// compared to the per-character cost, and keeping the
// single-pass implementation makes the mapping table easy to
// audit.
func normalizePathInput(s string) string {
	return pathutil.NormalizeInput(s)
}
