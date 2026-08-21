//go:build windows

// Windows path normalization. The platform split mirrors
// internal/command/cwd/path_windows.go — Windows has multiple
// notions of "absolute" (drive-rooted, root-relative, UNC,
// drive-relative-without-separator, forward-slash variants) that
// Go's filepath.IsAbs only partly understands. NormalizeForOS here
// is the single entry point that decides which form a path is in
// and produces the canonical "\\" form for the current OS.
//
// See F-PATHUTIL-001 (docs/feat/F-PATHUTIL-001-unified-path.md) for
// the design rationale and the API contract. The test matrix lives
// in path_windows_test.go.
package pathutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errEmptyPath is returned by helpers that require non-empty input.
// Kept package-private — callers never see this directly.
var errEmptyPath = errors.New("pathutil: empty path")

// NormalizeForOS turns any source-string path into the canonical
// Windows form (drive-rooted, backslash separators, no trailing
// junk). Classification rules:
//
//	"C:\foo"            → "C:\foo"            (drive-rooted, kept)
//	"C:/foo"            → "C:\foo"            (drive-rooted forward-slash variant)
//	"C:foo"             → ERROR               (drive-relative ambiguity; rejected)
//	"C:"                → ERROR               (bare drive, rejected)
//	"\foo" / "/foo"     → "<drv>:\foo"        (root-relative → current-drive)
//	"\\?\F:\foo"        → "\\?\F:\foo"        (long-path prefix, kept)
//	"\\server\share"    → "UNC canonicalized" (UNC path)
//	"foo", "./foo"      → Clean("foo")        (relative, passthrough — no HOME join)
//	""                  → ERROR               (empty)
//
// Rationale for each rule (cross-references cwd/path_windows.go,
// where the same rules live in resolvePath):
//
//   - Drive-relative ("C:foo") is ambiguous on Windows: it means
//     "relative to the current directory on drive C:" which we have
//     no way to determine from path-string input. Reject loudly
//     rather than silently joining $HOME.
//
//   - Bare drive ("C:") is similarly ambiguous (current dir on that
//     drive). Same rejection.
//
//   - Root-relative ("\foo" or "/foo") is "root of the current
//     drive" — a shell convention, not a Win32 one. We bridge to
//     the drive-prefixed form so downstream Win32 APIs (and git
//     for Windows) see a fully-qualified path.
//
//   - Relative paths ("foo", "./foo") are NOT joined with $HOME.
//     That is the cwd::resolvePath contract; pathutil only does
//     "shape normalization", not "where does this directory live".
//
//   - Long-path prefix "\\?\" is preserved verbatim; downstream
//     Win32 APIs accept it as-is and bypass MAX_PATH checks.
//
// UNC paths ("\\server\share\path") are passed to filepath.Clean,
// which preserves the UNC root and normalizes the rest.
func NormalizeForOS(p string) (string, error) {
	if p == "" {
		return "", errEmptyPath
	}

	// Drive-relative check on RAW input (before Clean). Clean on
	// its own is unreliable here: Clean("C:foo") returns "C:foo"
	// verbatim, but Clean("C:") returns "C:." on some Go versions
	// — we'd rather not depend on that quirk. The explicit
	// pre-check rejects both cases before any Clean runs.
	if isWindowsDriveRel(p) {
		return "", fmt.Errorf(
			"pathutil: drive-relative path %q is ambiguous on Windows; "+
				"use %q\\foo (or %q/foo) for an absolute path",
			p, p, p)
	}

	// Long-path prefix "\\?\" — preserve verbatim and skip ALL
	// processing. filepath.Clean's behaviour with the "\\?\"
	// prefix is not stable across Go versions (some collapse it,
	// some don't; some drop the leading "\\?\" entirely, as we
	// observed in path_windows_test.go). Downstream Win32 APIs
	// accept the prefix as-is and bypass MAX_PATH checks, so we
	// never want to mangle it.
	if strings.HasPrefix(p, `\\?\`) {
		return p, nil
	}

	// UNC path ("\\server\share\...") — pass through to Clean
	// which understands the UNC root and normalises the tail.
	// We can't use the root-relative prepending rule below
	// because "\\server\share" starts with a backslash but is
	// already an absolute, fully-qualified path (UNC).
	if strings.HasPrefix(p, `\\`) {
		return FromSlash(filepath.Clean(p)), nil
	}

	// Root-relative ("\foo" or "/foo") → prepend current drive
	// letter. We can't use filepath.IsAbs for this because Go's
	// IsAbs requires a volume prefix; bare backslash paths have
	// volumeNameLen == 0 and IsAbs returns false. The shell
	// bridges the "root of current drive" translation; we have
	// to do it ourselves.
	normalised := p
	if len(normalised) > 0 && (normalised[0] == '/' || normalised[0] == '\\') {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("pathutil: cannot resolve current drive: %w", err)
		}
		if len(wd) < 2 || wd[1] != ':' {
			return "", fmt.Errorf("pathutil: cwd %q is not drive-rooted", wd)
		}
		// wd = "C:\foo\bar" → drive = "C:" → prefix the leading
		// separator with drive + "\" so the result is drive-
		// rooted absolute.
		normalised = wd[:2] + "\\" + normalised[1:]
	}

	cleaned := filepath.Clean(normalised)

	// Defensive: Clean on Windows should always produce a path
	// with backslashes (filepath.toNorm replaces '/' with '\').
	// We don't trust this 100% across Go versions, so the
	// FromSlash below is belt-and-suspenders.
	return FromSlash(cleaned), nil
}

// NormalizeForGit is NormalizeForOS + an extra guarantee: the
// returned path uses ONLY backslashes as separators. On Windows this
// is the same as NormalizeForOS today (filepath.Clean already
// produces backslashes), but the explicit step makes the contract
// obvious at call sites and survives any future Clean regression.
//
// Why force backslashes for git? Git for Windows accepts both '/' and
// '\' in argv paths, but in some argv / Win32-syscall combinations
// the forward-slash form triggers ERROR_INVALID_PARAMETER when git
// forwards the path to RemoveDirectoryW / CreateFileW. Concretely:
// `git worktree remove F:/foo` has been observed to fail with
// "Invalid argument" while `git worktree remove F:\foo` succeeds
// against the same directory. The MSYS_NO_PATHCONV=1 env var
// (set in internal/command/gtw/exec_windows.go) reduces but does
// not eliminate this risk; the safest path is to give git only
// backslashes in the first place.
func NormalizeForGit(p string) (string, error) {
	n, err := NormalizeForOS(p)
	if err != nil {
		return "", err
	}
	return FromSlash(n), nil
}

// FromSlash replaces every '/' in p with '\\'. On Windows it's
// always safe to call — even if p is already backslash-separated —
// because the function only acts on '/'. The result uses the OS
// native separator.
//
// Exists per SPEC.md §13.3.1 so callers can grep `pathutil.FromSlash`
// and find every conversion site, even on Unix where it's a no-op.
func FromSlash(p string) string {
	return strings.ReplaceAll(p, "/", "\\")
}

// ToSlash replaces every '\\' in p with '/'. Inverse of FromSlash.
// Useful when emitting paths to tooling (logs, URLs, JSON wire
// format) that prefer POSIX form.
func ToSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

// Equal compares two paths under Windows semantics:
//
//   - Case-insensitive on every component (drive letter, directory
//     names, extension). NTFS preserves case but compares case-
//     insensitively; matching Win32 semantics means case-folding
//     before compare.
//   - Slash-insensitive: forward slashes and backslashes are
//     equivalent — both are accepted by every Win32 API. Without
//     this normalization, "C:/foo" and "C:\foo" would compare
//     unequal and break callers that read paths from git (which
//     emits forward slashes) vs. callers that read paths from the
//     shell (which emits backslashes).
//   - Trailing separator: stripped by Clean (Win32 treats "C:\foo"
//     and "C:\foo\" as the same directory).
//
// Symlinks are NOT resolved. NTFS symlinks are followed
// transparently by Win32 APIs but the path-string identity is
// preserved; callers wanting symlink resolution should call
// filepath.EvalSymlinks explicitly.
func Equal(a, b string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	// filepath.Clean on Windows produces backslashes, so both
	// sides are already separator-normalized. We still need
	// case-folding.
	return strings.EqualFold(ca, cb)
}

// IsUnder reports whether child is a descendant of (or equal to)
// parent on Windows. Implementation:
//
//  1. Clean both sides (handles "./" / "//" / trailing separators).
//  2. Same-drive check: child and parent must share the drive
//     letter (case-insensitive). Cross-drive paths cannot be
//     ancestor/descendant.
//  3. Strip volume prefix from both sides for prefix comparison.
//  4. Compute parentWithSep = parent + "\" (so "C:\foo" doesn't
//     match "C:\foobar" via naive prefix match).
//
// Equal semantics: case-insensitive, separator-insensitive (same
// rules as Equal above).
func IsUnder(child, parent string) bool {
	if child == "" || parent == "" {
		return false
	}
	c := filepath.Clean(child)
	p := filepath.Clean(parent)

	// Cross-drive check.
	cd, pd := windowsDrive(c), windowsDrive(p)
	if cd != "" && pd != "" && !strings.EqualFold(cd, pd) {
		return false
	}

	// Strip the drive prefix for the prefix compare (the prefix
	// compare must be on the path-tail only, otherwise the
	// drive letter itself contributes to HasPrefix in confusing
	// ways — "C:" vs "D:").
	cTail := strings.TrimPrefix(c, cd)
	pTail := strings.TrimPrefix(p, pd)

	if strings.EqualFold(cTail, pTail) {
		return true
	}

	sep := "\\"
	if !strings.HasSuffix(pTail, sep) {
		pTail += sep
	}
	return strings.HasPrefix(strings.ToLower(cTail), strings.ToLower(pTail))
}

// windowsDrive extracts the drive-letter prefix from a Windows path:
// "C:\foo" → "C:", "\\?\C:\foo" → "C:" (UNC and \\?\ stripped),
// "\\server\share\foo" → "\\server\share" (UNC root is treated as
// a "drive" for IsUnder purposes), "foo" → "" (no drive).
//
// Used by IsUnder's cross-drive check; not exported because the
// exact extraction rules are implementation details and may change.
func windowsDrive(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return p[:2]
	}
	if strings.HasPrefix(p, `\\?\`) && len(p) >= 6 && p[5] == ':' {
		return `\\?\` + p[4:6]
	}
	// UNC root: "\\server\share" — everything up to and including
	// the second backslash.
	if strings.HasPrefix(p, `\\`) {
		parts := strings.SplitN(p[2:], "\\", 3)
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return `\\` + parts[0] + `\` + parts[1]
		}
	}
	return ""
}

// isWindowsDriveRel mirrors the helper in internal/command/cwd/
// path_windows.go. We can't share it across packages without an
// awkward shared-test-export dance, and the rule is one screenful
// of straightforward code — duplication is cheaper than coupling.
//
// Returns true when p is a Windows drive-relative path — a drive
// letter followed by ':' with no separator immediately after.
//
//	"C:"        → true   (just the drive)
//	"C:foo"     → true   (drive + path, no separator)
//	"C:."       → true
//	"c:foo"     → true   (case-insensitive)
//	"C:\\foo"   → false  (drive-rooted — absolute)
//	"C:/foo"    → false
//	"foo"       → false
//	""          → false
func isWindowsDriveRel(p string) bool {
	if len(p) < 2 {
		return false
	}
	c := p[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	if p[1] != ':' {
		return false
	}
	if len(p) == 2 {
		return true
	}
	return p[2] != '/' && p[2] != '\\'
}
