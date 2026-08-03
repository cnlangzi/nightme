// Package registry — backupCorrupt helper shared by the v1.2
// stores. When a JSON file fails to parse, the original bytes are
// moved to <path>.bak so a human can inspect them later. Used by
// both ChatSessionFile and AgentSessionFile.
package registry

import (
	"errors"
	"os"
)

// backupCorrupt moves the offending bytes to <path>.bak so a
// human can inspect them later. Best-effort: any failure is
// returned to the caller.
func backupCorrupt(path string, data []byte) error {
	bak := path + ".bak"
	if err := os.WriteFile(bak, data, 0o600); err != nil {
		return err
	}
	// Remove the original — the open path will re-create on next write.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
