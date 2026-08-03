// Package registry — v1.x → v1.2 migration.
//
// v1.1 stored a single Entry per session in registry.json. v1.2
// splits that into ChatSessionEntry + AgentSessionEntry in two
// separate files (chat_sessions.json, agent_sessions.json).
//
// Design decision (per docs/PLAN.md §4.6.7):
//
//	v1.x → v1.2 is NOT a transparent migration. v1.1 did not
//	persist chat_id (the binding was in-memory only; see
//	gateway.BindingEntry doc). We therefore cannot reconstruct
//	the chat_id → session mapping from disk alone.
//
// Consequence:
//
//	- v1.x registry.json is preserved as registry.json.v1.bak for
//	  manual recovery.
//	- v1.2 starts with empty chat_sessions.json and
//	  agent_sessions.json.
//	- Existing CLI processes are NOT auto-restored. Users re-do
//	  /cwd for each chat to bind fresh.
//
// This file provides MigrateV1ToV2: detect v1.x data, archive it,
// and emit (possibly empty) v1.2 files. The caller decides whether
// to actually create the v1.2 files (typically only if v1.x data
// existed).
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// MigrateV1ToV2 inspects v1RegistryPath. If a v1.x file exists:
//
//  1. Loads it (re-using the existing Open logic for the v1.x
//     format; we only use the on-disk bytes via direct read so the
//     runtime does not have to open the legacy file).
//  2. Writes a backup to <v1RegistryPath>.v1.bak (chmod 0600).
//  3. Does NOT write the v1.2 files; the caller starts fresh.
//
// Returns (v1EntryCount, error). v1EntryCount is the number of
// v1.x Entries found (0 if no v1.x file existed).
//
// The function is idempotent: if <v1RegistryPath>.v1.bak already
// exists, it is left alone (the previous migration already ran).
// If v1RegistryPath itself is missing, returns (0, nil).
func MigrateV1ToV2(v1RegistryPath string) (int, error) {
	// 1. Detect v1.x file.
	data, err := os.ReadFile(v1RegistryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("migrate: read v1 registry: %w", err)
	}

	// 2. Parse to count entries (best-effort: if corrupt, we still
	//    want to back up the bytes).
	entries := make(map[string]Entry)
	if err := json.Unmarshal(data, &entries); err != nil {
		// Corrupt file: back up and return as if no entries.
		if backupErr := writeV1Backup(v1RegistryPath, data); backupErr != nil {
			return 0, fmt.Errorf("migrate: corrupt v1 file and backup failed: %w", backupErr)
		}
		return 0, nil
	}

	// 3. Write backup.
	if err := writeV1Backup(v1RegistryPath, data); err != nil {
		return 0, fmt.Errorf("migrate: write v1 backup: %w", err)
	}

	return len(entries), nil
}

// writeV1Backup writes the raw v1.x bytes to <path>.v1.bak.
// Best-effort: returns the underlying error so the caller can
// decide whether to abort startup.
func writeV1Backup(v1RegistryPath string, data []byte) error {
	bak := v1RegistryPath + ".v1.bak"

	// If backup already exists, leave it alone (idempotent).
	if _, err := os.Stat(bak); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat v1 backup: %w", err)
	}

	dir := filepath.Dir(bak)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir v1 backup dir: %w", err)
	}

	if err := os.WriteFile(bak, data, 0o600); err != nil {
		return fmt.Errorf("write v1 backup: %w", err)
	}
	return nil
}

// V1RegistryHasData reports whether a v1.x registry file exists at
// path. Useful for callers that want to log "found N legacy entries,
// preserved to .v1.bak" before deciding whether to start fresh.
func V1RegistryHasData(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}