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
	"log/slog"
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
//
// entries holds known-prefix chatIDs the loader could map onto a
// registered channel; dropped holds unknown-prefix entries kept as
// raw JSON bytes so save() can preserve them verbatim. The two
// maps together equal the on-disk chatSessions object.
type Store struct {
	path string

	mu      sync.Mutex
	entries map[string]*registry.ChatSessionEntry
	dropped map[string]json.RawMessage
}

// New loads (or initializes) the chat_sessions.json store. A missing
// file yields an empty store; a corrupt file is backed up to
// <path>.bak and the store is reset to empty.
//
// Every entry's map key MUST equal its ChatID field. The ChatID
// MUST start with a channel-namespaced prefix registered by the
// channel that produced it (telegram "tg_", feishu "oc_", slack
// "sl_", bot workflows "bt_", …). See docs/CHANNEL.md §5.5 and
// docs/channel/telegram.md §5.1 for the channel-namespacing rule.
//
// The set of accepted prefixes comes from channel.ChatIDPrefixes()
// — every channel adapter registers its prefix at init() time via
// channel.Register. New channels plug in automatically: declare
// the prefix in their init() and chatstore validation picks it up.
// No edits to this file are required when a new channel is added.
//
// On unknown prefixes the loader is lenient: it stashes the entry
// in Store.dropped as raw JSON, logs a warning identifying the
// offending key(s), and continues. Dropped entries are preserved
// verbatim on every subsequent save() — the daemon never silently
// overwrites user data, and the operator can fix the file by hand
// (re-key by ChatID, add the channel adapter to the build, etc.).
// A key that disagrees with its entry's ChatID field is still
// rejected outright, since that is a structural corruption the
// loader cannot reason about.
//
// If no channels are registered at all AND the file has entries,
// New returns an error: every entry would be dropped and the
// operator would see no useful feedback, which is the data-loss
// scenario this package refuses to enter silently. Build the
// daemon with at least one channel adapter to load.
func New(path string) (*Store, error) {
	s := &Store{
		path:    path,
		entries: make(map[string]*registry.ChatSessionEntry),
		dropped: make(map[string]json.RawMessage),
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// First unmarshal pass — pull each chatSessions entry as
		// raw JSON so we can keep unknown-prefix bytes in
		// Store.dropped verbatim without re-marshalling them.
		var container struct {
			Version      int                        `json:"version"`
			ChatSessions map[string]json.RawMessage `json:"chatSessions"`
		}
		if err := json.Unmarshal(data, &container); err != nil {
			if backupErr := registry.BackupCorrupt(path, data); backupErr != nil {
				return nil, fmt.Errorf("chat_sessions: corrupt %s and backup failed: %w", path, backupErr)
			}
			s.entries = make(map[string]*registry.ChatSessionEntry)
			s.dropped = make(map[string]json.RawMessage)
		} else if len(container.ChatSessions) > 0 {
			prefixes := channel.ChatIDPrefixes()
			if len(prefixes) == 0 {
				// Build misconfiguration: no channels registered
				// means every entry would be dropped with no
				// meaningful feedback. Fail loudly so the
				// operator can fix the build, not the data.
				return nil, fmt.Errorf(
					"chat_sessions: %s has %d entries but no channels are registered; chatstore cannot accept any prefix. Build with at least one channel adapter (channel/feishu, channel/telegram, channel/bot, …)",
					path, len(container.ChatSessions),
				)
			}
			prefixSet := make(map[string]bool, len(prefixes))
			for _, p := range prefixes {
				prefixSet[p] = true
			}
			const maxLogged = 5
			dropped := 0
			for k, raw := range container.ChatSessions {
				if len(raw) == 0 {
					continue
				}
				// Structural check first: key must equal
				// entry.ChatID. This is a hard error — a
				// mismatch is corruption the loader cannot
				// reason about, so we cannot preserve it
				// verbatim (we don't know which field the
				// operator wanted to keep).
				var probe registry.ChatSessionEntry
				if err := json.Unmarshal(raw, &probe); err != nil {
					return nil, fmt.Errorf(
						"chat_sessions: %s entry %q failed to decode: %w",
						path, k, err,
					)
				}
				if probe.ChatID != k {
					return nil, fmt.Errorf(
						"chat_sessions: %s has key %q but entry.ChatID %q — every entry must be keyed by ChatID",
						path, k, probe.ChatID,
					)
				}
				if !hasRegisteredPrefix(k, prefixSet) {
					// Stash raw bytes — save() will rewrite them
					// to disk verbatim so the on-disk file is
					// never silently modified by the loader.
					s.dropped[k] = append(json.RawMessage(nil), raw...)
					dropped++
					if dropped <= maxLogged {
						slog.Warn(
							"chatstore: chat_sessions.json entry has unknown chat-id prefix; preserving verbatim in on-disk file",
							"path", path,
							"key", k,
							"registered_prefixes", prefixes,
						)
					}
					continue
				}
				s.entries[k] = &probe
			}
			if dropped > maxLogged {
				slog.Warn(
					"chatstore: chat_sessions.json has additional entries with unknown chat-id prefixes; all are preserved verbatim",
					"path", path,
					"first_logged", maxLogged,
					"total_dropped", dropped,
				)
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
// registered prefixes. Extracted so the per-entry loop in New
// stays readable; takes a precomputed set for O(1) lookup.
func hasRegisteredPrefix(key string, prefixSet map[string]bool) bool {
	for p := range prefixSet {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// Dropped returns the chat-id keys that were preserved verbatim
// during load (unknown prefix) and the count. Tests / diagnostics
// can use this to confirm the loader preserved on-disk data
// without inspecting the raw file.
func (s *Store) Dropped() (keys []string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys = make([]string, 0, len(s.dropped))
	for k := range s.dropped {
		keys = append(keys, k)
	}
	return keys, len(s.dropped)
}

// Path returns the file path the store was opened with.
func (s *Store) Path() string { return s.path }

// save serializes the current entries + dropped map to disk.
// Caller must hold s.mu. Atomic write (temp + fsync + rename +
// chmod 0600).
//
// Known entries are marshaled through their typed structs;
// dropped entries are written verbatim as raw JSON. The merged
// object is what the next load sees, so the on-disk file round-
// trips through this loader without modification.
func (s *Store) save() error {
	sessions := make(map[string]json.RawMessage, len(s.entries)+len(s.dropped))
	for k, e := range s.entries {
		b, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("chat_sessions: marshal entry %q: %w", k, err)
		}
		sessions[k] = b
	}
	for k, raw := range s.dropped {
		sessions[k] = raw
	}
	container := struct {
		Version      int                        `json:"version"`
		ChatSessions map[string]json.RawMessage `json:"chatSessions"`
	}{
		Version:      registry.ChatSessionFileVersion,
		ChatSessions: sessions,
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