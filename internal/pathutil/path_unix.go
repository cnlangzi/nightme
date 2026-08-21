//go:build !windows

// Unix (POSIX) path normalization. The platform split mirrors
// internal/command/cwd/path_unix.go — POSIX has a single notion of
// absolute (a leading '/'), no drive letters / UNC roots / forward-
// slash root-relative quirks to handle, so NormalizeForOS reduces to
// "passthrough + Clean" and NormalizeForGit is identical (git on Unix
// already speaks POSIX).
//
// See F-PATHUTIL-001 (docs/feat/F-PATHUTIL-001-unified-path.md) for
// the design rationale and the API contract. Equal and IsUnder live
// here too because their Unix semantics are byte-exact.
package pathutil

import (
	"errors"
	"path/filepath"
	"strings"
)

// errEmptyPath is returned by helpers that require non-empty input.
// Kept package-private — callers never see this directly; it surfaces
// as the wrapped error from NormalizeForOS / NormalizeForGit.
var errEmptyPath = errors.New("pathutil: empty path")

// NormalizeForOS is the Unix implementation. POSIX has one absolute
// form (leading '/'), so NormalizeForOS just runs filepath.Clean
// (which collapses "//", "./", trailing "/" — all the things a
// downstream caller should NOT have to think about) and returns.
//
// Relative paths are passed through unchanged: NormalizeForOS does
// NOT silently join $HOME (that's cwd::resolvePath's job — see
// internal/command/cwd/path_unix.go for the rationale). A caller
// that wants HOME-relative resolution must call cwd::resolvePath
// explicitly.
//
// Returns an error only for empty input. Other cases are best-effort.
func NormalizeForOS(p string) (string, error) {
	if p == "" {
		return "", errEmptyPath
	}
	return filepath.Clean(p), nil
}

// NormalizeForGit on Unix is identical to NormalizeForOS: git on Unix
// already speaks POSIX paths, and there's no MSYS path-translation
// layer that could munge a Clean result. The function exists so
// callers can write platform-agnostic code (`pathutil.NormalizeForGit(p)`
// works on every OS) without sprinkling build tags at every site.
func NormalizeForGit(p string) (string, error) {
	return NormalizeForOS(p)
}

// FromSlash is a no-op on Unix — POSIX paths already use '/'. Exists
// to give callers a single import point (per SPEC.md §13.3.1) so
// grep `pathutil.FromSlash` finds every conversion site, even ones
// that are no-ops on Unix but matter on Windows.
func FromSlash(p string) string { return p }

// ToSlash is a no-op on Unix. See FromSlash for the rationale.
func ToSlash(p string) string { return p }

// Equal compares two paths for equivalence under Unix semantics:
// byte-exact after both sides have been Clean()'d. Symlinks are NOT
// resolved (EvalSymlinks would change behaviour for legit use cases
// where the caller intentionally holds two different paths to the
// same inode).
//
// Why not just `a == b`? Because Clean() collapses "foo/../bar" to
// "bar" and "foo//bar" to "foo/bar", and most callers want that
// normalization to be applied to BOTH sides of the comparison.
// Comparing raw `a == b` after Clean-ing only one side is a classic
// source of "looks equal but isn't" bugs.
//
// Trailing slashes are stripped by Clean, so Equal("foo", "foo/")
// returns true — which matches the convention used elsewhere in the
// codebase.
func Equal(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// IsUnder reports whether child is a descendant of (or equal to)
// parent. Implementation:
//
//  1. Clean both sides (handles "./" / "//" / trailing separators).
//  2. Reject if either side is empty (no sensible answer).
//  3. Compute parentWithSep = Clean(parent) + "/" (so "/foo" doesn't
//     match "/foobar" via naive prefix match).
//  4. Reject if child contains "/.." that escapes parent — a
//     pathological case like IsUnder("/foo/../bar", "/foo") must
//     return false even though the cleaned forms look nested.
//
// This is sufficient for gtw's "worktree is under repoRoot" check;
// it is NOT a general-purpose containment oracle (symlink traversal,
// case-folding on Windows, etc. live in the Windows variant).
func IsUnder(child, parent string) bool {
	if child == "" || parent == "" {
		return false
	}
	c := filepath.Clean(child)
	p := filepath.Clean(parent)
	// Pathological: child uses ".." to escape parent.
	if strings.Contains(child, "..") {
		// Re-clean the RAW input (with .. components) and
		// compare to the cleaned child. If they differ, the ..
		// was meaningful and we should re-evaluate.
		if filepath.Clean(child) != filepath.Clean(c) {
			return false
		}
	}
	if c == p {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(p, sep) {
		p += sep
	}
	return strings.HasPrefix(c, p)
}
