package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

type TopicState struct {
	ChatID               string    `json:"chat_id"`
	TopicID              int       `json:"topic_id"`
	PlaceholderMessageID int       `json:"placeholder_message_id"`
	UserMessageID        string    `json:"user_message_id"`
	LastMessageID        int       `json:"last_message_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type InputState struct {
	PromptMessageID int    `json:"prompt_message_id"`
	QuestionID      string `json:"question_id"`
	Step            int    `json:"step"`
	Kind            string `json:"kind"`
	OwnerID         int64  `json:"owner_id"`
}

type ChoiceState struct {
	RequestID  string           `json:"request_id"`
	MessageID  int              `json:"message_id"`
	ChatID     string           `json:"chat_id"`
	TopicID    int              `json:"topic_id"`
	Choice     *messages.Choice `json:"choice"`
	Step       int              `json:"step"`
	Picks      []string         `json:"picks"`
	Input      *InputState      `json:"input,omitempty"`
	Settled    bool             `json:"settled"`
	SelectedID string           `json:"selected_id"`
}

type persistedState struct {
	Topics  map[string]*TopicState  `json:"topics"`
	Choices map[string]*ChoiceState `json:"choices"`
}

type stateStore struct {
	mu      sync.RWMutex
	path    string
	topics  map[string]*TopicState
	choices map[string]*ChoiceState
}

// stateStoreTTL bounds how long inactive TopicState / ChoiceState
// entries survive a daemon restart. On newStateStore, entries
// older than (now - TTL) are pruned and the trimmed state is
// persisted. New inbound messages for the same chat recreate the
// state lazily — see handleMessage.ensurePlaceholder.
//
// 30 days is generous enough to cover:
//
//   - users who open Telegram rarely but still want continuity
//     (the placeholder they had on their last visit will be
//     recreated on their next message, but the in-Telegram
//     message itself is the visual anchor, not the adapter state)
//   - power users who switch devices / restart daemons
//
// Trade-off: a placeholder created >30 days ago gets a fresh
// message_id after a long idle period. The old Telegram message
// remains in the user's history (Telegram owns it, we don't
// delete); the user just sees a new "🤖 Working..." alongside
// the old one if they reopen that DM.
const stateStoreTTL = 30 * 24 * time.Hour

func newStateStore(path string) (*stateStore, error) {
	store := &stateStore{
		path:    path,
		topics:  make(map[string]*TopicState),
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
	if persisted.Topics != nil {
		store.topics = persisted.Topics
	}
	if persisted.Choices != nil {
		store.choices = persisted.Choices
	}
	// Prune stale entries older than stateStoreTTL on load.
	// Persist back if anything was removed so subsequent loads
	// start from the trimmed baseline.
	if store.pruneOlderThan(time.Now().UTC().Add(-stateStoreTTL)) > 0 {
		_ = store.save()
	}
	return store, nil
}

func (s *stateStore) topicKey(chatID string, topicID int) string {
	return chatID + "|" + stringInt(topicID)
}

func (s *stateStore) topic(chatID string, topicID int) (*TopicState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.topics[s.topicKey(chatID, topicID)]
	return cloneTopic(value), ok
}

func (s *stateStore) topicForChat(chatID string) (*TopicState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := chatID + "|"
	var match *TopicState
	for key, value := range s.topics {
		if len(key) < len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		if match == nil || value.TopicID < match.TopicID {
			match = value
		}
	}
	return cloneTopic(match), match != nil
}

func (s *stateStore) putTopic(value *TopicState) error {
	if value == nil || value.ChatID == "" {
		return nil
	}
	value.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	s.topics[s.topicKey(value.ChatID, value.TopicID)] = cloneTopic(value)
	s.mu.Unlock()
	return s.save()
}

// pruneOlderThan removes TopicState entries whose UpdatedAt is at
// or before cutoff, plus any ChoiceState bound to those topics
// (orphaned choices are unreachable and would leak). Returns the
// number of TopicState entries removed. Caller decides whether to
// save() — newStateStore does so automatically; mid-run callers
// may batch the save.
//
// Used by newStateStore to bound the on-disk state file when a
// user accumulates many inactive chats (block / mute / abandon).
// See stateStoreTTL for the rationale.
func (s *stateStore) pruneOlderThan(cutoff time.Time) int {
	if cutoff.IsZero() {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for key, value := range s.topics {
		if value == nil {
			delete(s.topics, key)
			continue
		}
		// UpdatedAt == zero means legacy state from before the
		// field was added. Treat as ancient — prune so a buggy
		// migration can't leak pre-existing entries indefinitely.
		if value.UpdatedAt.IsZero() || !value.UpdatedAt.After(cutoff) {
			delete(s.topics, key)
			removed++
		}
	}
	// Choices are pruned only if their topic is gone — a still-live
	// topic keeps its in-flight ChoiceState even if the choice has
	// been idle (e.g., user opened the prompt and walked away).
	for reqID, value := range s.choices {
		if value == nil {
			delete(s.choices, reqID)
			continue
		}
		if _, topicAlive := s.topics[s.topicKey(value.ChatID, value.TopicID)]; !topicAlive {
			delete(s.choices, reqID)
		}
	}
	return removed
}

func (s *stateStore) choiceByRequestID(requestID string) (*ChoiceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.choices[requestID]
	return cloneChoice(value), ok
}

func (s *stateStore) choiceByMessageID(messageID int) (*ChoiceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, value := range s.choices {
		if value != nil && value.MessageID == messageID {
			return cloneChoice(value), true
		}
	}
	return nil, false
}

func (s *stateStore) choiceByShortID(short string) (*ChoiceState, bool) {
	if short == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, value := range s.choices {
		if value == nil {
			continue
		}
		if shortID(value.RequestID) == short {
			return cloneChoice(value), true
		}
	}
	return nil, false
}

func (s *stateStore) pendingInput(chatID string, userID int64, replyToMessageID int) (*ChoiceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, value := range s.choices {
		if value != nil && value.Input != nil && value.ChatID == chatID && value.Input.PromptMessageID == replyToMessageID {
			if userID == 0 || value.Input.OwnerID == userID {
				return cloneChoice(value), true
			}
		}
	}
	return nil, false
}

func (s *stateStore) putChoice(value *ChoiceState) error {
	if value == nil || value.RequestID == "" {
		return nil
	}
	s.mu.Lock()
	s.choices[value.RequestID] = cloneChoice(value)
	s.mu.Unlock()
	return s.save()
}

func (s *stateStore) save() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	data, err := json.MarshalIndent(persistedState{Topics: s.topics, Choices: s.choices}, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".telegram-state-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, s.path)
}

func cloneTopic(value *TopicState) *TopicState {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneChoice(value *ChoiceState) *ChoiceState {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Picks = append([]string(nil), value.Picks...)
	if value.Choice != nil {
		choice := *value.Choice
		choice.Options = append([]messages.ChoiceOption(nil), value.Choice.Options...)
		choice.Questions = append([]messages.ChoiceQuestion(nil), value.Choice.Questions...)
		for questionIndex := range choice.Questions {
			choice.Questions[questionIndex].Options = append([]messages.ChoiceOption(nil), value.Choice.Questions[questionIndex].Options...)
		}
		copy.Choice = &choice
	}
	if value.Input != nil {
		input := *value.Input
		copy.Input = &input
	}
	return &copy
}

func stringInt(value int) string {
	if value == 0 {
		return "0"
	}
	result := ""
	for value > 0 {
		result = string(rune('0'+value%10)) + result
		value /= 10
	}
	return result
}
