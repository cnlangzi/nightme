// Package chatstore encapsulates all read/write operations against
// chat_sessions.json. It is the single point of truth for the on-disk
// ChatSessionEntry shape: every persisted field mutation goes through
// Store, and ChatSession reads it back via Get.
//
// The package exists to eliminate the lost-update race that lived in
// ChatSession.persistChatEntry's release-then-persist pattern
// (cs.mu.Unlock before csFile.Upsert). Each setter here holds
// record.mu.Lock through the Upsert so a concurrent setter cannot
// overwrite in-memory state while a stale snapshot is still queued
// for disk. See docs/feat/F-CHATSTORE-001-chat-session-persistence.md
// §1.3 for the interleaving diagram.
package chatstore

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"
)

// Store wraps a ChatSessionFile and serves as the only entry point for
// mutating or reading persisted chat-session state.
//
// Constructed once at startup via New; shared across all
// per-ChatSession reads/writes. Safe for concurrent use.
type Store struct {
	file *registry.ChatSessionFile
	mu   sync.Mutex // protects recs map
	recs map[string]*record
}

// record is the per-chatID bookkeeping. mu serializes all mutations on
// that chatID (within a chat). entry is the canonical in-memory view;
// when nil, the entry must be loaded from disk (or freshly created via
// Bootstrap) before any setter is invoked.
type record struct {
	mu    sync.Mutex
	entry *registry.ChatSessionEntry
}

// New creates a Store that persists through file.
func New(file *registry.ChatSessionFile) *Store {
	return &Store{
		file: file,
		recs: make(map[string]*record),
	}
}

// Bootstrap ensures chatID has a record installed in memory and an
// entry on disk. Used by Manager.GetOrCreate on every chat access:
//
//   - memory has entry → return it
//   - memory empty, disk has entry → install from disk, return
//   - memory empty, disk empty → create fresh, sync Upsert, return
//
// primaryAgent is required only when creating a fresh entry (otherwise
// the entry has no agent identity). Safe to call multiple times; the
// second call returns the existing record without re-writing.
func (s *Store) Bootstrap(chatID, primaryAgent string) (*registry.ChatSessionEntry, error) {
	if chatID == "" {
		return nil, errors.New("chatstore: empty chatID")
	}
	r := s.getOrCreateRecord(chatID)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry != nil {
		return r.entry, nil
	}

	if e, ok := s.file.GetByChat(chatID); ok {
		r.entry = e
		return r.entry, nil
	}

	if primaryAgent == "" {
		return nil, fmt.Errorf("chatstore: %s not on disk; need primaryAgent to create", chatID)
	}

	r.entry = &registry.ChatSessionEntry{
		ID:                "cs_" + chatID,
		ChatID:            chatID,
		PrimaryAgent:      primaryAgent,
		CreatedAt:         time.Now(),
		LastInteractionAt: time.Now(),
	}
	if err := s.file.Upsert(r.entry); err != nil {
		return nil, err
	}
	return r.entry, nil
}

// load returns the record for chatID. Loads from disk if not in memory.
// Returns an error if neither memory nor disk has it — callers must
// invoke Bootstrap first.
//
// Used by every setter; setters assume the chatID has been bootstrapped.
func (s *Store) load(chatID string) (*record, error) {
	r := s.getOrCreateRecord(chatID)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry != nil {
		return r, nil
	}

	if e, ok := s.file.GetByChat(chatID); ok {
		r.entry = e
		return r, nil
	}
	return nil, fmt.Errorf("chatstore: %s not initialized; call Bootstrap first", chatID)
}

// getOrCreateRecord returns the record for chatID, creating an empty
// one in the map if absent. Cheap: the heavy initialization (loading
// from disk) happens later in load / Bootstrap.
func (s *Store) getOrCreateRecord(chatID string) *record {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[chatID]; ok {
		return r
	}
	r := &record{}
	s.recs[chatID] = r
	return r
}

// SetSelectedCwd changes the active workspace for chatID. An empty
// cwd is permitted (cleared state). Holds record.mu.Lock through the
// Upsert to prevent the lost-update race documented in
// F-CHATSTORE-001 §1.3 — release-then-persist is forbidden.
func (s *Store) SetSelectedCwd(chatID, cwd string) error {
	if chatID == "" {
		return errors.New("chatstore: empty chatID")
	}
	if s == nil || s.file == nil { return nil }
	r, err := s.load(chatID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry.SelectedCwd == cwd {
		return nil // no-op short-circuit
	}
	r.entry.SelectedCwd = cwd
	r.entry.LastInteractionAt = time.Now()
	return s.file.Upsert(r.entry)
}

// SetSelectedAgent switches the active agent family for chatID. Holds
// record.mu.Lock through the Upsert.
func (s *Store) SetSelectedAgent(chatID, agent string) error {
	if chatID == "" || agent == "" {
		return errors.New("chatstore: empty agent")
	}
	r, err := s.load(chatID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry.SelectedAgent == agent {
		return nil
	}
	r.entry.SelectedAgent = agent
	r.entry.LastInteractionAt = time.Now()
	return s.file.Upsert(r.entry)
}

// SetWatchMode changes the per-chat message-watch mode. Holds
// record.mu.Lock through the Upsert.
func (s *Store) SetWatchMode(chatID string, mode int) error {
	r, err := s.load(chatID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry.WatchMode == mode {
		return nil
	}
	r.entry.WatchMode = mode
	r.entry.LastInteractionAt = time.Now()
	return s.file.Upsert(r.entry)
}

// SetThinkMode changes the per-chat thinking-content visibility.
// Holds record.mu.Lock through the Upsert.
func (s *Store) SetThinkMode(chatID string, mode int) error {
	r, err := s.load(chatID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry.ThinkMode == mode {
		return nil
	}
	r.entry.ThinkMode = mode
	r.entry.LastInteractionAt = time.Now()
	return s.file.Upsert(r.entry)
}

// SetToolsMode changes the per-chat tool-event visibility. Holds
// record.mu.Lock through the Upsert.
func (s *Store) SetToolsMode(chatID string, mode int) error {
	r, err := s.load(chatID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry.ToolsMode == mode {
		return nil
	}
	r.entry.ToolsMode = mode
	r.entry.LastInteractionAt = time.Now()
	return s.file.Upsert(r.entry)
}

// SetWatcherHintEmitted stamps the one-time /watch hint tombstone.
// Holds record.mu.Lock through the Upsert.
func (s *Store) SetWatcherHintEmitted(chatID string, v bool) error {
	r, err := s.load(chatID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry.WatcherHintEmitted == v {
		return nil
	}
	r.entry.WatcherHintEmitted = v
	r.entry.LastInteractionAt = time.Now()
	return s.file.Upsert(r.entry)
}

// Get returns a copy of the entry for chatID, or (nil, false) if the
// chatID is unknown. The returned entry is a fresh copy; mutating it
// has no effect on the in-memory record. Safe for concurrent use.
func (s *Store) Get(chatID string) (*registry.ChatSessionEntry, bool) {
	s.mu.Lock()
	r, ok := s.recs[chatID]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entry == nil {
		return nil, false
	}
	cp := *r.entry
	return &cp, true
}

// List returns a snapshot of all in-memory entries. Each returned
// entry is a fresh copy. Order is unspecified.
func (s *Store) List() []*registry.ChatSessionEntry {
	s.mu.Lock()
	recs := make([]*record, 0, len(s.recs))
	for _, r := range s.recs {
		recs = append(recs, r)
	}
	s.mu.Unlock()

	out := make([]*registry.ChatSessionEntry, 0, len(recs))
	for _, r := range recs {
		r.mu.Lock()
		if r.entry != nil {
			cp := *r.entry
			out = append(out, &cp)
		}
		r.mu.Unlock()
	}
	return out
}
// NilStore is a sentinel used by ChatSession.Store() when no real
// store has been wired (e.g. unit tests that construct a ChatSession
// via New without WithStore). Every setter on NilStore is a no-op
// and returns nil. The in-memory ChatSession state stays authoritative
// for these cases; the production runtime always wires a real store
// via Manager.WithPersistence.
var NilStore = &Store{}

// Override the constructor to support NilStore: every setter
// must be safe when s.file is nil.
