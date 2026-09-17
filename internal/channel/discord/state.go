package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// intentsVersion is stamped into persisted state. Whenever the
// Intent bitfield that goes into IDENTIFY changes, bump this so
// the next Start knows the saved session_id is no longer
// resumable and should fall back to a fresh IDENTIFY.
const intentsVersion = 1

// stateStore persists the Gateway session state across daemon
// restarts. Phase 1 needs only session_id + last_seq +
// resume_gateway_url; Phase 3 may add jitter state / close-code
// counters without breaking the on-disk schema (struct fields are
// additive under encoding/json).
type stateStore struct {
	mu        sync.Mutex
	path      string
	persisted struct {
		SessionID        string    `json:"session_id"`
		LastSeq          int64     `json:"last_seq"`
		ResumeGatewayURL string    `json:"resume_gateway_url,omitempty"`
		IntentsVersion   int       `json:"intents_version"`
		SavedAt          time.Time `json:"saved_at,omitempty"`
	}
}

// newStateStore loads (or initializes) the state store from path.
// A missing file yields an empty store; a corrupt file is reported
// as an error so the operator notices (mirrors chatstore's policy).
func newStateStore(path string) (*stateStore, error) {
	s := &stateStore{path: path}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &s.persisted); err != nil {
		return nil, fmt.Errorf("discord: parse state %s: %w", path, err)
	}
	return s, nil
}

// snapshot returns a copy of the persisted fields for safe reading.
func (s *stateStore) snapshot() (sessionID string, lastSeq int64, resumeURL string, intentsVersion int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persisted.SessionID, s.persisted.LastSeq, s.persisted.ResumeGatewayURL, s.persisted.IntentsVersion
}

// setSession records the values Discord returned in op=0 t=READY.
// lastSeq is reset to 0 because READY is itself a dispatch event
// (Discord doesn't emit an `s` field for it; the FIRST post-READY
// dispatch carries s=1).
func (s *stateStore) setSession(sessionID, resumeURL string) error {
	s.mu.Lock()
	s.persisted.SessionID = sessionID
	s.persisted.ResumeGatewayURL = resumeURL
	s.persisted.IntentsVersion = intentsVersion
	s.persisted.SavedAt = time.Now().UTC()
	s.mu.Unlock()
	return s.save()
}

// setSeq records the latest dispatch sequence number. Called on
// every op=0 frame so a crash mid-stream loses at most the
// in-flight event.
func (s *stateStore) setSeq(seq int64) error {
	s.mu.Lock()
	s.persisted.LastSeq = seq
	s.persisted.SavedAt = time.Now().UTC()
	s.mu.Unlock()
	return s.save()
}

// clear wipes the persisted session. Called when Discord replies
// with op=9 Invalid Session (d=false) so the next Start falls
// back to IDENTIFY rather than thrashing.
func (s *stateStore) clear() error {
	s.mu.Lock()
	s.persisted.SessionID = ""
	s.persisted.LastSeq = 0
	s.persisted.ResumeGatewayURL = ""
	s.mu.Unlock()
	return s.save()
}

// save writes atomically: temp file + rename. Mirrors
// chatstore.SaveDefault.
func (s *stateStore) save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	data, err := json.MarshalIndent(&s.persisted, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".discord-state-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
