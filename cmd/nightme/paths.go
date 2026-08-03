// Package main — shared v1.2 store path helpers.
//
// cmd/nightme/run.go and cmd/nightme/list.go both need to resolve
// the same on-disk store paths (chat_sessions.json,
// agent_sessions.json). Centralizing them here keeps the two
// commands in agreement and avoids the trap of duplicating
// `cfg.Paths.DataDir` resolution semantics.
package main

import (
	"os"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/config"
)

// chatSessionsPath returns the absolute path to chat_sessions.json
// under cfg.Paths.DataDir. Mirrors the runtime (cmd/nightme/run.go)
// so the CLI list command sees the same file the daemon writes.
func chatSessionsPath(cfg *config.Config) (string, error) {
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "chat_sessions.json"), nil
}

// agentSessionsPath returns the absolute path to agent_sessions.json
// under cfg.Paths.DataDir. Mirrors the runtime (cmd/nightme/run.go).
func agentSessionsPath(cfg *config.Config) (string, error) {
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agent_sessions.json"), nil
}

// legacyRegistryPath returns the absolute path to the v0.1
// registry.json that the v1.2 daemon no longer writes. Kept here
// so the daemon and the `list` command can both call
// `removeLegacyRegistryFile` to clean up the obsolete file on
// first use (the v1.x Format is not understood by v1.2 stores).
func legacyRegistryPath(cfg *config.Config) (string, error) {
	base, err := filepath.Abs(cfg.Paths.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "registry.json"), nil
}

// removeLegacyRegistryFile archives the v0.1 registry.json to
// <path>.v1.bak and then deletes the original, so a human can
// recover the v1.x data after upgrading. Best-effort: missing
// files are silently ignored; other errors are returned so the
// caller can log and continue. The v1.2 daemon does not read or
// write this file, so the archive + delete is safe.
//
// Idempotent: if <path>.v1.bak already exists (a prior migration
// ran), the original is left alone so a later re-discovery of the
// same legacy file does not destroy the earlier snapshot.
func removeLegacyRegistryFile(cfg *config.Config) error {
	path, err := legacyRegistryPath(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	bak := path + ".v1.bak"
	if _, err := os.Stat(bak); err == nil {
		// Backup already exists — leave both files alone. The
		// archive is the source of truth for recovery; the
		// duplicate legacy file is harmless.
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(path, bak); err != nil {
		return err
	}
	return nil
}

