// Package chatstore is the in-process store for chat_sessions.json.
//
// F-CHATSTORE-001 (https://github.com/cnlangzi/nightme/pull/227):
// replaces the previous two-layer design (chatstore.Store wrapping
// registry.ChatSessionFile) with a single Store that owns the
// entries map and the persistence path. The split caused a
// cross-chatID data race: the chatstore layer held r.mu (per-chat)
// while marshalling in writeLocked held f.mu (file-level), so
// setters and the marshal could touch the same entry struct under
// different locks.
//
// The single Store struct has a single mutex, which the marshal
// and every per-field setter acquire. No per-chat r.mu — there is
// no longer a separate "in-memory cache" map; the Store.entries
// field IS the authoritative state.
//
// Per-field setters (SetSelectedCwd, etc.) all go through Store's
// mutex: lock, no-op check, mutate, unlock. The setters are
// responsible for atomic mutation + persistence; the Store doesn't
// hide that from the caller.
package chatstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/registry"
)

// Store is the chat_sessions.json in-memory cache + persistence.
//
// Constructed once at startup via New; shared across all callers
// that read or mutate chat session state. Safe for concurrent use.
type Store struct {
	path string

	mu      sync.Mutex
	entries map[string]*registry.ChatSessionEntry
}

// New loads (or initializes) the chat_sessions.json store. A missing
// file yields an empty store; a corrupt file is backed up to
// <path>.bak and the store is reset to empty.
//
// Every entry's map key MUST equal its ChatID field. The ChatID
// MUST start with a channel-namespaced prefix registered by the
// channel that produced it (Telegram: "tg_", Feishu: "oc_",
// Slack: "sl_", …). See docs/CHANNEL.md §5.5 and
// docs/channel/telegram.md §5.1 for the channel-namespacing rule.
//
// The set of accepted prefixes comes from channel.ChatIDPrefixes()
// — every channel adapter registers its prefix at init() time via
// channel.Register. New channels (future Slack bridge, …) plug in
// automatically: declare the prefix in their init() and chatstore
// validation picks it up. No edits to this file are required when
// a new channel is added.
//
// On unknown prefixes the loader is lenient: it logs a warning
// identifying the offending entry and the recognised prefix set,
// then skips the entry and continues with the next one. This
// matches chatstore's general philosophy of never blocking daemon
// startup on a stale / hand-edited file — a single odd entry
// should not keep every other chat unreachable. Operators see the
// warning in stderr and can fix the file by hand. A key that
// disagrees with its entry's ChatID field is still rejected
// outright, since that is a structural corruption the loader
// cannot reason about.
//
// The loader never silently rewrites the on-disk file.
func New(path string) (*Store, error) {
	s := &Store{
		path:    path,
		entries: make(map[string]*registry.ChatSessionEntry),
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var container struct {
			Version      int                                   `json:"version"`
			ChatSessions map[string]*registry.ChatSessionEntry `json:"chatSessions"`
		}
		if err := json.Unmarshal(data, &container); err != nil {
			if backupErr := registry.BackupCorrupt(path, data); backupErr != nil {
				return nil, fmt.Errorf("chat_sessions: corrupt %s and backup failed: %w", path, backupErr)
			}
			s.entries = make(map[string]*registry.ChatSessionEntry)
		} else if container.ChatSessions != nil {
			prefixes := channel.ChatIDPrefixes()
			for k, e := range container.ChatSessions {
				if e == nil {
					continue
				}
				if e.ChatID != k {
					return nil, fmt.Errorf("chat_sessions: %s has key %q but entry.ChatID %q — every entry must be keyed by ChatID", path, k, e.ChatID)
				}
				if !hasRegisteredPrefix(k, prefixes) {
					log.Printf(
						"chatstore: %s entry %q has unknown chat-id prefix (registered: %s); skipping",
						path, k, formatPrefixList(prefixes),
					)
					continue
				}
				s.entries[k] = e
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// Nothing to do — empty store.
	default:
		return nil, fmt.Errorf("chat_sessions: read %s: %w", path, err)
	}

	return s, nil
}

// hasRegisteredPrefix reports whether key starts with any of the
// prefixes registered by a channel adapter (channel.ChatIDPrefixes).
// Extracted so the per-entry loop in New stays readable.
func hasRegisteredPrefix(key string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// formatPrefixList renders the registered prefix set as a
// human-readable, comma-joined list — quoted, so an operator
// scanning stderr sees exactly which prefixes their build
// recognises.
func formatPrefixList(prefixes []string) string {
	if len(prefixes) == 0 {
		return "(none registered)"
	}
	quoted := make([]string, len(prefixes))
	for i, p := range prefixes {
		quoted[i] = `"` + p + `...`
	}
	return strings.Join(quoted, ", ")
}

// Path returns the file path the store was opened with.
func (s *Store) Path() string { return s.path }

// save serializes the current entries map to disk. Caller must hold
// s.mu. Atomic write (temp + fsync + rename + chmod 0600).
func (s *Store) save() error {
	container := struct {
		Version      int                                   `json:"version"`
		ChatSessions map[string]*registry.ChatSessionEntry `json:"chatSessions"`
	}{
		Version:      registry.ChatSessionFileVersion,
		ChatSessions: s.entries,
	}
	data, err := json.MarshalIndent(container, "", "  ")
	if err != nil {
		return fmt.Errorf("chat_sessions: marshal: %w", err)
	}

	dir := filepath.Dir(s.path)
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
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("chat_sessions: rename: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("chat_sessions: chmod: %w", err)
	}
	return nil
}

// Get returns a copy of the entry for chatID, or (nil, false) if the
// chatID is unknown. The returned entry is a fresh copy (including
// pointer fields, so mutating any field — including
// SelectedAgentSessionID — has no effect on the in-memory record).
// Safe for concurrent use.
func (s *Store) Get(chatID string) (*registry.ChatSessionEntry, bool) {
	s.mu.Lock()
	e, ok := s.entries[chatID]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	return deepCopyEntry(e), true
}

// List returns a snapshot of all in-memory entries. Each returned
// entry is a fresh copy (incl. pointer fields). Order is unspecified.
func (s *Store) List() []*registry.ChatSessionEntry {
	s.mu.Lock()
	out := make([]*registry.ChatSessionEntry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, deepCopyEntry(e))
	}
	s.mu.Unlock()
	return out
}

// deepCopyEntry returns a copy of e whose pointer fields do not
// alias the original. ChatSessionEntry has only one pointer field
// (SelectedAgentSessionID); the rest are value types and the
// struct shallow copy covers them.
func deepCopyEntry(e *registry.ChatSessionEntry) *registry.ChatSessionEntry {
	cp := *e
	if e.SelectedAgentSessionID != nil {
		v := *e.SelectedAgentSessionID
		cp.SelectedAgentSessionID = &v
	}
	return &cp
}

// Save writes entry to the in-memory map and atomically persists
// to disk. Used by callers (e.g. ChatSession.persistChatEntry) that
// have a full ChatSessionEntry struct to write. The map write and
// the disk write are under the same Store mutex, so concurrent
// Get/List see a consistent state.
//
// Concurrency: this replaces the previous chat_sessions.json
// write paths that suffered a cross-chatID data race (the marshal
// in writeLocked held f.mu while the writer's chatstore wrapper
// held r.mu for the same entry struct). The single-mutex design
// eliminates that race.
func (s *Store) Save(entry *registry.ChatSessionEntry) error {
	if entry == nil {
		return errors.New("chatstore: nil entry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[entry.ChatID] = entry
	return s.save()
}

// Bootstrap creates a fresh entry for chatID with primaryAgent. If an
// entry already exists, the call is a no-op for that field but
// LastInteractionAt is bumped and the file is rewritten.
//
// Returns a deep copy (same as Get) so concurrent Bootstrap callers
// cannot race on the internal map entry pointer.
//
// Concurrency: holds s.mu across the mutation AND save().
func (s *Store) Bootstrap(chatID, primaryAgent string) (*registry.ChatSessionEntry, error) {
	if chatID == "" {
		return nil, errors.New("chatstore: empty chatID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[chatID]; !ok {
		if primaryAgent == "" {
			return nil, errors.New("chatstore: chatID not on disk; need primaryAgent to create")
		}
		s.entries[chatID] = &registry.ChatSessionEntry{
			ID:                "cs_" + chatID,
			ChatID:            chatID,
			PrimaryAgent:      primaryAgent,
			SelectedAgent:     primaryAgent, // seed active agent (docs/CHATSTORE.md)
			CreatedAt:         time.Now(),
			LastInteractionAt: time.Now(),
		}
	} else {
		// Bump the timestamp so callers can observe the access.
		e := s.entries[chatID]
		e.LastInteractionAt = time.Now()
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	return deepCopyEntry(s.entries[chatID]), nil
}

// SetSelectedCwd changes the active workspace for chatID. An empty
// cwd is permitted (cleared state). If chatID has no entry yet, a
// fresh one is created with primaryAgent (or, if empty, an error).
// No-op if the value is unchanged.
//
// Concurrency: holds s.mu across the mutation AND save() so the entry
// fields and the on-disk snapshot move together. A concurrent Get /
// List sees either the pre-mutation or post-mutation state, never a
// half-applied one. The single-mutex design eliminates the
// cross-chatID race that the previous chatstore wrapper had
// (where r.mu and f.mu protected the same entry struct under
// different locks).
func (s *Store) SetSelectedCwd(chatID, cwd string) error {
	if chatID == "" {
		return errors.New("chatstore: empty chatID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[chatID]
	if !ok {
		return errors.New("chatstore: entry not initialized; call Bootstrap first")
	}
	if e.SelectedCwd == cwd {
		return nil
	}
	e.SelectedCwd = cwd
	e.LastInteractionAt = time.Now()
	return s.save()
}

// SetSelectedAgent switches the active agent family for chatID.
// See SetSelectedCwd for the concurrency contract.
func (s *Store) SetSelectedAgent(chatID, agent string) error {
	if chatID == "" || agent == "" {
		return errors.New("chatstore: empty agent")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[chatID]
	if !ok {
		return errors.New("chatstore: entry not initialized; call Bootstrap first")
	}
	if e.SelectedAgent == agent {
		return nil
	}
	e.SelectedAgent = agent
	e.LastInteractionAt = time.Now()
	return s.save()
}

// SetWatchMode changes the per-chat message-watch mode.
// See SetSelectedCwd for the concurrency contract.
func (s *Store) SetWatchMode(chatID string, mode int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[chatID]
	if !ok {
		return errors.New("chatstore: entry not initialized; call Bootstrap first")
	}
	if e.WatchMode == mode {
		return nil
	}
	e.WatchMode = mode
	e.LastInteractionAt = time.Now()
	return s.save()
}

// SetThinkMode changes the per-chat thinking-content visibility.
// See SetSelectedCwd for the concurrency contract.
func (s *Store) SetThinkMode(chatID string, mode int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[chatID]
	if !ok {
		return errors.New("chatstore: entry not initialized; call Bootstrap first")
	}
	if e.ThinkMode == mode {
		return nil
	}
	e.ThinkMode = mode
	e.LastInteractionAt = time.Now()
	return s.save()
}

// SetToolsMode changes the per-chat tool-event visibility.
// See SetSelectedCwd for the concurrency contract.
func (s *Store) SetToolsMode(chatID string, mode int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[chatID]
	if !ok {
		return errors.New("chatstore: entry not initialized; call Bootstrap first")
	}
	if e.ToolsMode == mode {
		return nil
	}
	e.ToolsMode = mode
	e.LastInteractionAt = time.Now()
	return s.save()
}

// SetWatcherHintEmitted stamps the one-time /watch hint tombstone.
// Does NOT bump LastInteractionAt — the hint is a system event, not
// a user interaction (docs/CHATSTORE.md / F-watch).
func (s *Store) SetWatcherHintEmitted(chatID string, v bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[chatID]
	if !ok {
		return errors.New("chatstore: entry not initialized; call Bootstrap first")
	}
	if e.WatcherHintEmitted == v {
		return nil
	}
	e.WatcherHintEmitted = v
	return s.save()
}
