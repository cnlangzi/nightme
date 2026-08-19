// Package registry — AgentSessionFile (v1.2 agent_sessions.json I/O wrapper).
//
// On-disk format:
//   { "version": 1, "agentSessions": { "<id>": AgentSessionEntry, ... } }
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// AgentSessionFile is the I/O wrapper for agent_sessions.json. It
// is safe for concurrent use.
type AgentSessionFile struct {
	path string

	mu      sync.RWMutex
	entries map[string]*AgentSessionEntry
}

// OpenAgentSessionFile loads (or initializes) the agent session
// file at path. A missing file yields an empty store; a corrupt
// file is backed up to <path>.bak and the store is reset to empty.
func OpenAgentSessionFile(path string) (*AgentSessionFile, error) {
	f := &AgentSessionFile{
		path:    path,
		entries: make(map[string]*AgentSessionEntry),
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var container struct {
			Version       int                           `json:"version"`
			AgentSessions map[string]*AgentSessionEntry `json:"agentSessions"`
		}
		if err := json.Unmarshal(data, &container); err != nil {
			if backupErr := BackupCorrupt(path, data); backupErr != nil {
				return nil, fmt.Errorf("agent_sessions: corrupt %s and backup failed: %w", path, backupErr)
			}
			f.entries = make(map[string]*AgentSessionEntry)
		} else {
			if container.AgentSessions != nil {
				f.entries = container.AgentSessions
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// Nothing to do.
	default:
		return nil, fmt.Errorf("agent_sessions: read %s: %w", path, err)
	}

	return f, nil
}

// Path returns the file path the store was opened with.
func (f *AgentSessionFile) Path() string { return f.path }

// Upsert inserts or replaces the entry keyed by ID and writes the
// store to disk atomically. The on-disk file is chmod 0600.
func (f *AgentSessionFile) Upsert(e *AgentSessionEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if e == nil {
		return errors.New("agent_sessions: nil entry")
	}
	f.entries[e.ID] = e
	return f.writeLocked()
}

// Get returns the entry for id, or nil/false if absent.
func (f *AgentSessionFile) Get(id string) (*AgentSessionEntry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.entries[id]
	return e, ok
}

// GetByChatPool returns all entries whose ChatSessionID matches.
// Multiple matches expected (per-chat pool of AgentSessions).
func (f *AgentSessionFile) GetByChatPool(chatSessionID string) []*AgentSessionEntry {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var out []*AgentSessionEntry
	for _, e := range f.entries {
		if e.ChatSessionID == chatSessionID {
			out = append(out, e)
		}
	}
	return out
}

// List returns a snapshot of all entries.
func (f *AgentSessionFile) List() []*AgentSessionEntry {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*AgentSessionEntry, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e)
	}
	return out
}

// Delete removes the entry for id and persists. No-op for unknown id.
func (f *AgentSessionFile) Delete(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.entries[id]; !ok {
		return nil
	}
	delete(f.entries, id)
	return f.writeLocked()
}

// DeleteMany removes every id present in the store and persists the
// result in a single write. Ids absent from the store are silently
// skipped. Useful for batch GC (e.g. `nightme list` cleaning up
// exited sessions) where calling Delete in a loop would rewrite
// the file N times.
func (f *AgentSessionFile) DeleteMany(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	changed := false
	for _, id := range ids {
		if _, ok := f.entries[id]; ok {
			delete(f.entries, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return f.writeLocked()
}

// writeLocked serializes f.entries to disk. Caller must hold f.mu.
// Atomic write (temp + fsync + rename + chmod 0600).
func (f *AgentSessionFile) writeLocked() error {
	container := struct {
		Version       int                           `json:"version"`
		AgentSessions map[string]*AgentSessionEntry `json:"agentSessions"`
	}{
		Version:       AgentSessionFileVersion,
		AgentSessions: f.entries,
	}
	data, err := json.MarshalIndent(container, "", "  ")
	if err != nil {
		return fmt.Errorf("agent_sessions: marshal: %w", err)
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".agent_sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("agent_sessions: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("agent_sessions: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("agent_sessions: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agent_sessions: close temp: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("agent_sessions: rename: %w", err)
	}
	if err := os.Chmod(f.path, 0o600); err != nil {
		return fmt.Errorf("agent_sessions: chmod: %w", err)
	}
	return nil
}