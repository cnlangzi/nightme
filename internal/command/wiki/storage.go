package wiki

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// atomicWrite writes content to a temp file adjacent to path,
// then renames into place. Crash-safe: a partial write leaves
// the original file untouched (the temp file is cleaned up by
// the deferred Remove).
//
// Parent directories are created on demand. The mode of the
// final file matches the convention:
//
//	wiki.yml       0o600 (metadata — matches ~/.nightme/gtw.yml)
//	anything else  0o644
//
// §11.5 + §12 boundary: parent directories must not be
// symbolic links. A symlink at any ancestor of path rejects the
// write — the wiki/ tree is owned by /wiki, not by user
// redirection.
func atomicWrite(path, content string) error {
	dir := filepath.Dir(path)
	if err := checkNoSymlinkAncestors(dir); err != nil {
		return fmt.Errorf("refuse write into %s: %w", path, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent dir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".wiki-write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	mode := os.FileMode(0o644)
	if filepath.Base(path) == "wiki.yml" {
		mode = 0o600
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// checkNoSymlinkAncestors walks dir upward and rejects if any
// component is a symbolic link. Uses os.Lstat so the check is
// about the link itself, not its target.
//
// Bounded by repoRoot: when set, the walk stops at repoRoot and
// the final repoRoot is also checked. This prevents a symlinked
// repo root from masquerading as a normal directory while still
// catching wiki/-internal symlinks (which is the §11.5 case).
func checkNoSymlinkAncestors(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for {
		info, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Path does not exist yet — MkdirAll will create
				// the leaf dir, but every existing ancestor must
				// be a real dir. Continue the walk; MkdirAll
				// fails naturally if a non-dir exists.
				parent := filepath.Dir(abs)
				if parent == abs {
					return nil
				}
				abs = parent
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent directory %s is a symbolic link", abs)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent path %s is not a directory", abs)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return nil
		}
		abs = parent
	}
}

// hashFile returns the SHA-256 of a file's contents in the
// canonical "sha256:<hex>" form used by Wiki.md §11.4's framing
// contract. Returns the empty string when path is missing.
func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(data)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// resolvedUnder reports whether path resolves to a file beneath
// root (the physical wiki directory), using cleaned lexically-
// resolved paths. Used by §12 containment checks.
//
// The lexical-cleaned comparison is the cheap first filter; the
// symlink ancestor check above is the hard boundary. The two
// together satisfy Wiki.md §11.5's "containment checks use
// cleaned, resolved paths under the repository's physical
// wiki/ directory rather than lexical prefix checks alone."
func resolvedUnder(root, path string) bool {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}
