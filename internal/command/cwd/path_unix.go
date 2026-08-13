//go:build !windows

// Unix path resolution for /cwd.
//
// POSIX has a single notion of absolute: a leading '/'. There
// are no drive letters, UNC roots, or root-relative variants
// to worry about, so the platform-specific logic reduces to
// "IsAbs → keep, otherwise join with $HOME".
//
// verifyDirectory is currently identical across Unix and
// Windows (os.Stat + info.IsDir), but the two platforms live
// in separate files so future Windows-specific concerns —
// \\?\ long-path prefixing, reparse-point unwrapping, reserved
// device names (CON, PRN, AUX, …) — can land without touching
// the Unix path.
package cwd

import (
	"fmt"
	"os"
	"path/filepath"
)

// resolvePath turns an already-tilde-expanded path into an
// absolute path. Absolute paths are kept as-is (modulo
// filepath.Abs cleanup); relative paths are resolved against
// the user's home directory, matching the documented /cwd
// semantics:
//
//	/cwd /abs/path  → /abs/path
//	/cwd foo        → $HOME/foo
func resolvePath(expanded string) (string, error) {
	if filepath.IsAbs(expanded) {
		return filepath.Abs(expanded)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errHomeUnset(err)
	}
	return filepath.Abs(filepath.Join(home, expanded))
}

// verifyDirectory checks that abs is a directory the user can
// /cwd into. raw is the original user input — included in
// error messages so the user can correlate the failure with
// what they typed.
//
// On Unix this is a plain os.Stat + info.IsDir check. The
// file lives next to resolvePath so the platform split stays
// in one place; the Windows variant can grow quirks (long
// path support, reparse-point handling) without disturbing
// this implementation.
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
