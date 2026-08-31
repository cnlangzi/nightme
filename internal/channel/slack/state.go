package slack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// openStreamTTL bounds how long an orphaned open-stream record
// survives. A stream older than this is assumed already reaped by
// Slack (or so stale that closing it would confuse more than help),
// so recovery drops it rather than firing a doomed stopStream.
const openStreamTTL = 24 * time.Hour

// OpenStream records a stream that has been started but not yet
// stopped.
//
// This has no Feishu counterpart and is the one genuinely new
// bookkeeping burden Slack imposes. A Feishu card is inert: if the
// daemon dies mid-turn the card simply stops updating. A Slack
// stream is stateful — startStream without a matching stopStream
// can leave the message rendered as perpetually in-progress. So the
// adapter persists every open stream and closes the leftovers on the
// next start (docs/channel/slack.md §3.5).
type OpenStream struct {
	ChatID    string    `json:"chat_id"`
	ChannelID string    `json:"channel_id"`
	ThreadTS  string    `json:"thread_ts,omitempty"`
	TS        string    `json:"ts"`
	UserMsgID string    `json:"user_msg_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// ChoiceState remembers an interactive prompt so a later
// block_actions callback can be correlated back to the originating
// request without the caller ever seeing a Slack message id (the
// Channel contract in internal/channel/channel.go keeps
// choice correlation Channel-private via Choice.RequestID).
type ChoiceState struct {
	RequestID  string    `json:"request_id"`
	ChatID     string    `json:"chat_id"`
	ChannelID  string    `json:"channel_id"`
	TS         string    `json:"ts"`
	ThreadTS   string    `json:"thread_ts,omitempty"`
	Settled    bool      `json:"settled"`
	SelectedID string    `json:"selected_id,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type persistedState struct {
	OpenStreams map[string]*OpenStream  `json:"open_streams"`
	Choices     map[string]*ChoiceState `json:"choices"`
}

// stateStore persists the adapter's cross-restart state to
// <dataDir>/slack_state.json.
type stateStore struct {
	mu      sync.RWMutex
	path    string
	streams map[string]*OpenStream
	choices map[string]*ChoiceState
}

func newStateStore(path string) (*stateStore, error) {
	store := &stateStore{
		path:    path,
		streams: make(map[string]*OpenStream),
		choices: make(map[string]*ChoiceState),
	}
	if path == "" {
		return store, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	var persisted persistedState
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, err
	}
	if persisted.OpenStreams != nil {
		store.streams = persisted.OpenStreams
	}
	if persisted.Choices != nil {
		store.choices = persisted.Choices
	}
	return store, nil
}

func streamKey(channelID, ts string) string { return channelID + "|" + ts }

// putStream records an open stream and persists immediately. The
// write happens before the first append so a crash between
// startStream and the first content still leaves a recoverable
// record.
func (s *stateStore) putStream(rec *OpenStream) {
	if rec == nil {
		return
	}
	s.mu.Lock()
	s.streams[streamKey(rec.ChannelID, rec.TS)] = rec
	s.mu.Unlock()
	_ = s.save()
}

// dropStream forgets a stream after a successful stopStream.
func (s *stateStore) dropStream(channelID, ts string) {
	s.mu.Lock()
	delete(s.streams, streamKey(channelID, ts))
	s.mu.Unlock()
	_ = s.save()
}

// orphanStreams returns the open streams that are still worth
// closing, and forgets the ones past openStreamTTL. TTL-expired
// records are evicted from memory AND persisted — without the save
// they would resurrect on the next load (slack_state.json keeps
// accumulating dead entries until some other write happens).
func (s *stateStore) orphanStreams(now time.Time) []OpenStream {
	type result struct {
		active  []OpenStream
		changed bool
	}
	var res result
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for key, rec := range s.streams {
			if rec == nil {
				delete(s.streams, key)
				res.changed = true
				continue
			}
			if now.Sub(rec.StartedAt) > openStreamTTL {
				delete(s.streams, key)
				res.changed = true
				continue
			}
			res.active = append(res.active, *rec)
		}
	}()
	if res.changed {
		_ = s.save()
	}
	return res.active
}

func (s *stateStore) putChoice(state *ChoiceState) {
	if state == nil || state.RequestID == "" {
		return
	}
	state.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	s.choices[state.RequestID] = state
	s.mu.Unlock()
	_ = s.save()
}

func (s *stateStore) choice(requestID string) (*ChoiceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.choices[requestID]
	if !ok || value == nil {
		return nil, false
	}
	clone := *value
	return &clone, true
}

// save writes the state atomically: a temp file in the same
// directory, then rename. A torn write here would make the next
// start fail to parse and lose every open-stream record.
func (s *stateStore) save() error {
	s.mu.RLock()
	payload := persistedState{OpenStreams: s.streams, Choices: s.choices}
	path := s.path
	data, err := json.MarshalIndent(payload, "", "  ")
	s.mu.RUnlock()
	if err != nil || path == "" {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".slack_state-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
