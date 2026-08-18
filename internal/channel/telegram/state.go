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
