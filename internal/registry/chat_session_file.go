// Package registry — ChatSessionFile (v1.2 chat_sessions.json I/O wrapper).
//
// Mirrors the existing File pattern: path + mutex + map + atomic
// write (temp + fsync + rename + chmod 0600).
//
// On-disk format:
//   { "version": 1, "chatSessions": { "<id>": ChatSessionEntry, ... } }
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ChatSessionFile is the I/O wrapper for chat_sessions.json. It is
// safe for concurrent use.
type ChatSessionFile struct {
	path string

	mu      sync.RWMutex
	entries map[string]*ChatSessionEntry
}

// OpenChatSessionFile loads (or initializes) the chat session file
// at path. A missing file yields an empty store; a corrupt file is
// backed up to <path>.bak and the store is reset to empty.
func OpenChatSessionFile(path string) (*ChatSessionFile, error) {
	f := &ChatSessionFile{
		path:    path,
		entries: make(map[string]*ChatSessionEntry),
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var container struct {
			Version      int                          `json:"version"`
			ChatSessions map[string]*ChatSessionEntry `json:"chatSessions"`
		}
		if err := json.Unmarshal(data, &container); err != nil {
			if backupErr := backupCorrupt(path, data); backupErr != nil {
				return nil, fmt.Errorf("chat_sessions: corrupt %s and backup failed: %w", path, backupErr)
			}
			f.entries = make(map[string]*ChatSessionEntry)
		} else {
			if container.ChatSessions != nil {
				f.entries = container.ChatSessions
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// Nothing to do — empty store.
	default:
		return nil, fmt.Errorf("chat_sessions: read %s: %w", path, err)
	}

	return f, nil
}

// Path returns the file path the store was opened with.
func (f *ChatSessionFile) Path() string { return f.path }

// Upsert inserts or replaces the entry keyed by ID and writes the
// store to disk atomically. The on-disk file is chmod 0600.
func (f *ChatSessionFile) Upsert(e *ChatSessionEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if e == nil {
		return errors.New("chat_sessions: nil entry")
	}
	f.entries[e.ID] = e
	return f.writeLocked()
}

// Get returns the entry for id, or nil/false if absent.
func (f *ChatSessionFile) Get(id string) (*ChatSessionEntry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.entries[id]
	return e, ok
}

// GetByChat returns the entry whose ChatID matches, or nil/false.
// At most one match (ChatID is UNIQUE).
func (f *ChatSessionFile) GetByChat(chatID string) (*ChatSessionEntry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, e := range f.entries {
		if e.ChatID == chatID {
			return e, true
		}
	}
	return nil, false
}

// List returns a snapshot of all entries. Order is unspecified.
func (f *ChatSessionFile) List() []*ChatSessionEntry {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*ChatSessionEntry, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e)
	}
	return out
}

// Delete removes the entry for id and persists. Deleting a
// non-existent id is a no-op.
func (f *ChatSessionFile) Delete(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.entries[id]; !ok {
		return nil
	}
	delete(f.entries, id)
	return f.writeLocked()
}

// writeLocked serializes f.entries to disk. Caller must hold f.mu
// (write or read). Atomic write (temp + fsync + rename + chmod 0600).
func (f *ChatSessionFile) writeLocked() error {
	container := struct {
		Version      int                          `json:"version"`
		ChatSessions map[string]*ChatSessionEntry `json:"chatSessions"`
	}{
		Version:      ChatSessionFileVersion,
		ChatSessions: f.entries,
	}
	data, err := json.MarshalIndent(container, "", "  ")
	if err != nil {
		return fmt.Errorf("chat_sessions: marshal: %w", err)
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".chat_sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("chat_sessions: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chat_sessions: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chat_sessions: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("chat_sessions: close temp: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("chat_sessions: rename: %w", err)
	}
	if err := os.Chmod(f.path, 0o600); err != nil {
		return fmt.Errorf("chat_sessions: chmod: %w", err)
	}
	return nil
}