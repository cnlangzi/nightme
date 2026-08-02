// Package registry is a JSON-backed process registry for nightme.
//
// v1.1 (commit 5): the registry holds TWO tables — sessions
// (per-process state) and bindings (chat → session). The split
// matches the v1.1 responsibility isolation: Session is pure
// process state (no chat_id); the binding that connects a chat to
// a session lives in its own table owned by the Gateway.
//
// On-disk format (version 3):
//
//	{
//	  "version": 3,
//	  "sessions": { "<session_id>": Entry },
//	  "bindings": { "<chat_id>": BindingEntry }
//	}
//
// Sessions and bindings are keyed by their natural IDs
// (SessionID / ChatID respectively). v0.2.x files (version 2 or
// no version) are migrated on first read: every SessionEntry's
// ChatID field is extracted into a synthetic BindingEntry, the
// SessionEntry's ChatID is blanked, and the file is rewritten at
// v3.
//
// Writes are atomic (temp file + fsync + rename) and the file is
// chmod 0600 (NFR N-7: config / log / registry all 0600).
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

// Version is the on-disk schema version. Bump when the JSON shape
// changes incompatibly.
const Version = 3

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

// Entry is one row in the sessions table. v1.1: ChatID is always
// empty (the chat → session binding lives in BindingEntry). The
// field is kept on the struct for v0.2.x backward compatibility
// (the migration reads it but never writes it post-migration).
type Entry struct {
	SessionID string    `json:"session_id"`
	ChatID    string    `json:"chat_id,omitempty"` // always "" in v1.1 writes
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

// BindingEntry is one row in the bindings table. The chat → session
// binding belongs to the Gateway in v1.1; this struct is the
// on-disk representation.
//
// ChatID is the natural key. ChatType is metadata (p2p / group /
// thread) carried for /status replies. SessionID is the FK into
// the sessions table. Workspace and Agent are denormalized for
// /cwd reply ("Workspace set to <ws>") and /run reply
// ("Started: <agent>") without re-querying the session.
//
// v1.1 default ChatType for migrated entries: when a v0.2.x file
// is upgraded, the ChatType is unknown, so we default to "group"
// (the safe side per F-26 §3.1).
type BindingEntry struct {
	ChatID    string `json:"chat_id"`
	ChatType  string `json:"chat_type"`
	SessionID string `json:"session_id"`
	Workspace string `json:"workspace"`
	Agent     string `json:"agent"`
}

// File is the on-disk backed registry. It is safe for concurrent
// use. The zero value is not usable; create one with Open.
type File struct {
	path string

	mu       sync.RWMutex
	entries  map[string]Entry
	bindings map[string]BindingEntry
	dirty    bool // true when an in-memory v0.2.x → v1.1 migration is pending a rewrite
}

// Open loads (or initializes) the registry at path. A missing file
// yields an empty registry; a corrupt file is backed up to <path>.bak
// and the registry is reset to empty.
//
// On Open, files in the legacy v0.2.x shape (no "version" key) are
// migrated in-memory: every SessionEntry's ChatID is extracted into
// a synthetic BindingEntry and the entry's ChatID is blanked. The
// migration is persisted on the next Upsert / write — Open does
// not eagerly rewrite the file so the daemon's read path stays
// idempotent.
func Open(path string) (*File, error) {
	f := &File{
		path:     path,
		entries:  make(map[string]Entry),
		bindings: make(map[string]BindingEntry),
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := f.unmarshal(data); err != nil {
			if backupErr := backupCorrupt(path, data); backupErr != nil {
				return nil, fmt.Errorf("registry: corrupt %s and backup failed: %w", path, backupErr)
			}
			f.entries = make(map[string]Entry)
			f.bindings = make(map[string]BindingEntry)
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

// Upsert inserts or replaces the session entry keyed by SessionID
// and writes the registry to disk atomically. The on-disk file is
// chmod 0600.
func (f *File) Upsert(e Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.entries[e.SessionID] = e
	return f.writeLocked()
}

// Get returns the session entry for sessionID, or false if absent.
func (f *File) Get(sessionID string) (Entry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.entries[sessionID]
	return e, ok
}

// GetByChat returns the SESSION entry whose binding points at
// chatID. v1.1: this walks the bindings table (via in-memory map)
// then looks up the session. v0.2.x files migrated on Open have
// the same data shape so this works either way.
//
// Returns false when no binding points at chatID.
func (f *File) GetByChat(chatID string) (Entry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	be, ok := f.bindings[chatID]
	if !ok {
		// Legacy v0.2.x fallback: scan entries for ChatID match.
		// Empty after migration, but defensive against mixed state.
		for _, e := range f.entries {
			if e.ChatID == chatID {
				return e, true
			}
		}
		return Entry{}, false
	}
	e, ok := f.entries[be.SessionID]
	return e, ok
}

// List returns a snapshot of all session entries. Order is
// unspecified.
func (f *File) List() []Entry {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Entry, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e)
	}
	return out
}

// Delete removes the session entry for sessionID and persists.
// Deleting a non-existent sessionID is a no-op. Any binding that
// pointed at the session is also removed (orphaned bindings are
// useless).
func (f *File) Delete(sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.entries[sessionID]; !ok {
		return nil
	}
	delete(f.entries, sessionID)
	// Drop any binding that pointed at this session.
	for chatID, be := range f.bindings {
		if be.SessionID == sessionID {
			delete(f.bindings, chatID)
		}
	}
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

// --- Binding table (v1.1) ----------------------------------------------

// UpsertBinding inserts or replaces the binding keyed by ChatID
// and persists.
func (f *File) UpsertBinding(b BindingEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings[b.ChatID] = b
	return f.writeLocked()
}

// GetBinding returns the binding for chatID.
func (f *File) GetBinding(chatID string) (BindingEntry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	b, ok := f.bindings[chatID]
	return b, ok
}

// ListBindings returns a snapshot of all bindings.
func (f *File) ListBindings() []BindingEntry {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]BindingEntry, 0, len(f.bindings))
	for _, b := range f.bindings {
		out = append(out, b)
	}
	return out
}

// DeleteBinding removes the binding for chatID and persists.
func (f *File) DeleteBinding(chatID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.bindings[chatID]; !ok {
		return nil
	}
	delete(f.bindings, chatID)
	return f.writeLocked()
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

// --- Serialization -----------------------------------------------------

// onDisk is the JSON shape (v3). Two maps + a version stamp.
type onDisk struct {
	Version  int                      `json:"version"`
	Sessions map[string]Entry         `json:"sessions"`
	Bindings map[string]BindingEntry  `json:"bindings"`
}

// unmarshal reads bytes, detects v0.2.x vs v1.1, and populates
// f.entries / f.bindings. v0.2.x inputs are migrated in-memory and
// marked dirty (next write rewrites the file at v3).
func (f *File) unmarshal(data []byte) error {
	var v3 onDisk
	if err := json.Unmarshal(data, &v3); err == nil && v3.Version == Version {
		// v1.1 shape — populate directly.
		if v3.Sessions == nil {
			v3.Sessions = map[string]Entry{}
		}
		if v3.Bindings == nil {
			v3.Bindings = map[string]BindingEntry{}
		}
		f.entries = v3.Sessions
		f.bindings = v3.Bindings
		return nil
	}

	// Legacy v0.2.x shape: a flat map[string]Entry keyed by
	// session_id, with ChatID on each entry. Parse, then migrate.
	var legacy map[string]Entry
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("registry: parse: %w", err)
	}

	migrated, bindings := migrateLegacy(legacy)
	f.entries = migrated
	f.bindings = bindings
	f.dirty = true // next write rewrites in v3
	return nil
}

// migrateLegacy constructs the v1.1 state from a v0.2.x entry map:
// every entry's ChatID is extracted into a synthetic BindingEntry
// (ChatType defaults to "group" per F-26 §3.1 since the v0.2.x
// format didn't carry ChatType) and the entry's ChatID is blanked
// (v1.1 writes always have ChatID="" because the binding lives
// in its own table).
func migrateLegacy(legacy map[string]Entry) (map[string]Entry, map[string]BindingEntry) {
	entries := make(map[string]Entry, len(legacy))
	bindings := make(map[string]BindingEntry)
	for sid, e := range legacy {
		if e.ChatID != "" {
			bindings[e.ChatID] = BindingEntry{
				ChatID:    e.ChatID,
				ChatType:  ChatTypeDefault,
				SessionID: sid,
				Workspace: e.Workspace,
				Agent:     e.Agent,
			}
			e.ChatID = ""
		}
		entries[sid] = e
	}
	return entries, bindings
}

// ChatTypeDefault is the ChatType assigned to v0.2.x → v1.1
// migrated bindings whose original ChatType is unknown. Per
// F-26 §3.1, the safe default is "group".
const ChatTypeDefault = "group"

// writeLocked serializes f.entries + f.bindings to disk. Caller
// must hold f.mu.
//
// The write is atomic: data is written to a sibling temp file,
// fsync'd to flush OS buffers, then renamed over the target path.
// The resulting file is chmod 0600 (N-7).
func (f *File) writeLocked() error {
	out := onDisk{
		Version:  Version,
		Sessions: f.entries,
		Bindings: f.bindings,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: marshal: %w", err)
	}

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("registry: create temp: %w", err)
	}
	tmpName := tmp.Name()
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
	f.dirty = false
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
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}