// Package registry is a JSON-backed process registry for nightme.
//
// The registry records every CLI process nightme spawns: its session
// ID, chat ID, workspace, agent name, PID, and lifecycle status. It
// is the source of truth for nightme's "what is still running"
// queries and the cleanup sweep on shutdown.
//
// On-disk format: a single JSON object — { "<session_id>": Entry } —
// stored at a caller-provided path. Writes are atomic (temp file +
// fsync + rename) and the file is chmod 0600 (NFR N-7:
// config / log / registry all 0600).
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status enumerates the lifecycle states recorded for an entry.
type Status string

const (
	// StatusRunning means the child process is alive and nightme is
	// attached to it.
	StatusRunning Status = "running"

	// StatusDetached means the child process is alive but nightme no
	// longer holds it (e.g. SIGTERM was sent to nightme). The next
	// cleanup sweep can still see it for re-attachment.
	StatusDetached Status = "detached"

	// StatusExited means the child process has terminated. ExitCode
	// carries the exit code (or nil if the process was killed).
	StatusExited Status = "exited"
)

// Entry is one row in the registry. The zero value is not meaningful;
// callers should populate every field.
type Entry struct {
	SessionID string    `json:"session_id"`
	ChatID    string    `json:"chat_id"`
	Workspace string    `json:"workspace"`
	Agent     string    `json:"agent"`
	Args      []string  `json:"args,omitempty"`
	PID       int       `json:"pid"`
	PPID      int       `json:"ppid"`
	StartedAt time.Time `json:"started_at"`
	LastRunAt time.Time `json:"last_run_at"`
	Status    Status    `json:"status"`
	ExitCode  *int      `json:"exit_code,omitempty"`
}

// File is the on-disk backed registry. It is safe for concurrent use.
//
// The zero value is not usable; create one with Open.
type File struct {
	path string

	mu      sync.RWMutex
	entries map[string]Entry
}

// Open loads (or initializes) the registry at path. A missing file
// yields an empty registry; a corrupt file is backed up to <path>.bak
// and the registry is reset to empty.
func Open(path string) (*File, error) {
	f := &File{
		path:    path,
		entries: make(map[string]Entry),
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &f.entries); err != nil {
			// Corrupt file: back up and reset.
			if backupErr := backupCorrupt(path, data); backupErr != nil {
				return nil, fmt.Errorf("registry: corrupt %s and backup failed: %w", path, backupErr)
			}
			f.entries = make(map[string]Entry)
		}
	case errors.Is(err, os.ErrNotExist):
		// Nothing to do — empty registry.
	default:
		return nil, fmt.Errorf("registry: read %s: %w", path, err)
	}

	return f, nil
}

// Path returns the file path the registry was opened with.
func (f *File) Path() string { return f.path }

// Upsert inserts or replaces the entry keyed by SessionID and writes
// the registry to disk atomically. The on-disk file is chmod 0600.
func (f *File) Upsert(e Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.entries[e.SessionID] = e
	return f.writeLocked()
}

// Get returns the entry for sessionID, or false if absent.
func (f *File) Get(sessionID string) (Entry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.entries[sessionID]
	return e, ok
}

// GetByChat returns the entry bound to chatID, or false if none.
// Only one session per chat is allowed (Q4 in SPEC.md), so there is
// at most one match.
func (f *File) GetByChat(chatID string) (Entry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, e := range f.entries {
		if e.ChatID == chatID {
			return e, true
		}
	}
	return Entry{}, false
}

// List returns a snapshot of all entries. Order is unspecified.
// The returned slice is freshly allocated; callers may mutate it.
func (f *File) List() []Entry {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Entry, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e)
	}
	return out
}

// Delete removes the entry for sessionID and persists. Deleting a
// non-existent sessionID is a no-op.
func (f *File) Delete(sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.entries[sessionID]; !ok {
		return nil
	}
	delete(f.entries, sessionID)
	return f.writeLocked()
}

// MarkDetached flips the entry's status to detached and bumps
// LastRunAt. The entry is persisted. If the sessionID is unknown,
// ErrNotFound is returned.
func (f *File) MarkDetached(sessionID string) error {
	return f.transition(sessionID, StatusDetached, nil)
}

// MarkExited flips the entry's status to exited and records the exit
// code. If the sessionID is unknown, ErrNotFound is returned.
func (f *File) MarkExited(sessionID string, exitCode int) error {
	return f.transition(sessionID, StatusExited, &exitCode)
}

// ErrNotFound is returned by MarkDetached / MarkExited when the
// sessionID is not present in the registry.
var ErrNotFound = errors.New("registry: session not found")

func (f *File) transition(sessionID string, status Status, exitCode *int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	e, ok := f.entries[sessionID]
	if !ok {
		return ErrNotFound
	}
	e.Status = status
	e.LastRunAt = time.Now()
	if exitCode != nil {
		e.ExitCode = exitCode
	}
	f.entries[sessionID] = e
	return f.writeLocked()
}

// writeLocked serializes f.entries to disk. Caller must hold f.mu
// (write or read — read is sufficient to obtain a consistent copy).
//
// The write is atomic: data is written to a sibling temp file, fsync'd
// to flush OS buffers, then renamed over the target path. The
// resulting file is chmod 0600 (N-7).
func (f *File) writeLocked() error {
	data, err := json.MarshalIndent(f.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: marshal: %w", err)
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("registry: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any failure.
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("registry: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("registry: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("registry: close temp: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("registry: rename: %w", err)
	}
	// Ensure the file is 0600 regardless of umask (N-7).
	if err := os.Chmod(f.path, 0o600); err != nil {
		return fmt.Errorf("registry: chmod: %w", err)
	}
	return nil
}

// backupCorrupt moves the offending bytes to <path>.bak so a human
// can inspect them later. Best-effort: any failure is returned to the
// caller.
func backupCorrupt(path string, data []byte) error {
	bak := path + ".bak"
	if err := os.WriteFile(bak, data, 0o600); err != nil {
		return err
	}
	// Remove the original — Open will re-create on next write.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
