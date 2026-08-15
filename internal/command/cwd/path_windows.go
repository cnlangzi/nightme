//go:build windows

// Windows path resolution and verification for /cwd.
//
// Windows has several notions of "absolute" that POSIX does
// not. Go's filepath.IsAbs handles most of them after Clean
// normalises separator slashes:
//
//	filepath.IsAbs("C:\\foo")  → true   (drive-rooted)
//	filepath.IsAbs("C:/foo")   → true   (forward-slash variant)
//	filepath.IsAbs("C:foo")    → false  (drive-relative — ambiguous)
//	filepath.IsAbs("\\foo")    → true   (root-relative on current drive)
//	filepath.IsAbs("/foo")     → true   after Clean (Clean → "\\foo")
//
// "C:foo" (drive letter without separator) is genuinely
// ambiguous on Windows — it means "relative to the current
// directory on drive C:" — and almost always indicates a
// user typo. We detect it via isWindowsDriveRel before
// Clean so we don't depend on Clean's exact output for
// the bare-drive case.
//
// verifyDirectory is currently identical to the Unix
// version but lives in this file so future Windows-specific
// quirks (long-path "\\?\" prefix, reparse-point handling,
// reserved device names) can land here without disturbing
// the Unix implementation.
package cwd

import (
	"fmt"
	"os"
	"path/filepath"
)

// resolvePath turns an already-tilde-expanded path into an
// absolute path. See the file header for the classification
// rules; the summary is:
//
//	C:\foo, C:/foo, \foo, \\server\share  → kept as absolute
//	/foo, /foo                             → drive-letter prepended (current
//	                                         drive), then kept as absolute
//	C:foo, C:, c:foo                      → rejected (drive-relative ambiguity)
//	foo, ./foo, ../foo                    → joined with $HOME
func resolvePath(expanded string) (string, error) {
	// Drive-relative check runs on the RAW input. We can't
	// rely on filepath.Clean to flag these — Go 1.26's Clean
	// preserves the volume name verbatim (per the stdlib docs:
	// "On Windows, Clean does not modify the volume name"),
	// so Clean("C:") returns "C:" (NOT "C:." as one might
	// expect). Without the upfront check we'd fall through to
	// IsAbs("C:") which is false, and the user would get a
	// confusing "$HOME/C:" result instead of an actionable
	// error.
	if isWindowsDriveRel(expanded) {
		return "", fmt.Errorf(
			"drive-relative path %q is ambiguous on Windows; "+
				"use %q\\foo (or %q/foo) for an absolute path",
			expanded, expanded, expanded)
	}

	// Root-relative paths (leading / or \) need their drive
	// letter prepended. Go's filepath.IsAbs follows strict Win32
	// semantics — a leading `\` alone is "root-relative on the
	// current drive" but NOT a fully absolute path. The shell
	// convention ("/foo" means "root of current drive") is a
	// layer on top; we need to bridge the two.
	//
	// Strategy: if the path starts with / or \ AND has no volume
	// prefix (no drive letter, no UNC), prepend the current
	// drive letter from os.Getwd(). Then filepath.Abs on the
	// drive-prefixed path returns the expected "C:\foo" form.
	//
	// Why not just check `IsAbs("\\foo") == true`? Because Go's
	// IsAbs on Windows REQUIRES a volume prefix (volumeNameLen
	// > 0) — see internal/filepathlite/path_windows.go's
	// IsAbs implementation. A bare backslash path has
	// volumeNameLen == 0, so IsAbs returns false. The shell
	// handles the "root of current drive" translation; we have
	// to do it ourselves.
	normalised := expanded
	if len(normalised) > 0 && (normalised[0] == '/' || normalised[0] == '\\') {
		// Extract current drive letter from cwd (which is always
		// absolute on Windows, so cwd[0:2] = "C:").
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cwd: %w", err)
		}
		if len(wd) < 2 || wd[1] != ':' {
			// Shouldn't happen on real Windows, but bail safely
			// if cwd is somehow not in drive-letter form.
			return "", fmt.Errorf("cwd %q is not drive-rooted", wd)
		}
		// wd = "C:\foo\bar" → drive = "C:"
		// Replace the leading / or \ with drive + \ so the
		// resulting path is C:\foo (drive-rooted absolute).
		normalised = wd[:2] + "\\" + normalised[1:]
	}

	cleaned := filepath.Clean(normalised)

	if filepath.IsAbs(cleaned) {
		return filepath.Abs(cleaned)
	}

	// Truly relative — resolve against $HOME.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errHomeUnset(err)
	}
	return filepath.Abs(filepath.Join(home, expanded))
}

// isWindowsDriveRel reports whether path is a Windows
// drive-relative path — i.e. a drive letter followed by a
// colon with no separator immediately after. Such paths are
// ambiguous (they resolve against the current directory on
// the named drive, which we don't have a way to determine
// from /cwd input) and almost always indicate a user typo.
//
//	"C:"        → true   (just the drive)
//	"C:foo"     → true   (drive + path, no separator)
//	"C:."       → true
//	"c:foo"     → true   (case-insensitive)
//	"C:\\foo"   → false  (drive-rooted — absolute)
//	"C:/foo"    → false
//	"foo"       → false
//	""          → false
func isWindowsDriveRel(path string) bool {
	if len(path) < 2 {
		return false
	}
	c := path[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	if path[1] != ':' {
		return false
	}
	// "C:" alone is drive-relative (current dir on C: drive).
	if len(path) == 2 {
		return true
	}
	// "C:foo" — no separator after the colon → drive-relative.
	// "C:\\foo" / "C:/foo" have a separator → drive-rooted.
	return path[2] != '/' && path[2] != '\\'
}

// verifyDirectory checks that abs is a directory the user can
// /cwd into. raw is the original user input — included in
// error messages so the user can correlate the failure with
// what they typed.
//
// The implementation is identical to the Unix one (os.Stat
// + info.IsDir) but lives in a separate file so we can grow
// Windows-specific quirks without touching Unix. Likely
// extensions when needed:
//
//   - Long paths (> 260 chars): prepending "\\?\" before
//     os.Stat so Win32 MAX_PATH doesn't trip.
//   - Reparse points (junctions, symlinks): optionally
//     unwrapping via os.Lstat before deciding IsDir.
//   - Reserved device names (CON, PRN, AUX, NUL, COM1–9,
//     LPT1–9): rejecting early with a clearer message.
//
// All of those would go here, leaving path_unix.go and
// cmd.go untouched.
func verifyDirectory(abs, raw string) error {
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("Path does not exist: %s (resolved from %q)", abs, raw)
		}
		return fmt.Errorf("Cannot stat %s: %v", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Not a directory: %s", abs)
	}
	return nil
}
