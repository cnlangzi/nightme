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

	"github.com/cnlangzi/nightme/internal/pathutil"
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
//
// F-PATHUTIL-001 §13.3.3: this used to inline the drive-
// relative rejection (isWindowsDriveRel), the root-relative
// drive-letter prepending, the filepath.Clean / IsAbs checks,
// and the HOME-relative fallback. All of those concerns except
// the HOME-relative fallback (which pathutil deliberately does
// NOT do — see F-PATHUTIL-001 §4.3) now live in pathutil.
// pathutil.NormalizeForOS handles the drive-relative rejection,
// the drive-letter prepending, the Clean, and the
// long-path-prefix preservation; this function adds the
// HOME-relative fallback on top.
func resolvePath(expanded string) (string, error) {
	abs, err := pathutil.NormalizeForOS(expanded)
	if err != nil {
		// pathutil.NormalizeForOS already returns a
		// "drive-relative path %q is ambiguous..." error for
		// the "C:foo" / "C:" cases — re-emit verbatim, keeping
		// the user-facing message identical to the pre-migration
		// behaviour (cwd_test.go's TestResolvePath_DriveRelative
		// asserts on the substring "drive-relative").
		return "", err
	}
	if pathutil.IsAbs(abs) {
		return abs, nil
	}
	// Truly relative — resolve against $HOME.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errHomeUnset(err)
	}
	return pathutil.Join(home, expanded), nil
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
