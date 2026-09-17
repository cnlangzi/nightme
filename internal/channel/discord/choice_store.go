package discord

import (
	"strconv"
	"sync"

	"github.com/cnlangzi/nightme/internal/messages"
)

// choiceState is the per-prompt record the adapter needs to
// correlate an incoming button click back to the choice it was
// rendered on. Discord Message Components V1 ships the
// `custom_id` string in the click payload — we encode
// "c:<shortRequestID>:<optionIndex>" / "i:<shortRequestID>"
// at send time and look the state back up on the read path.
//
// Choice is a clone of messages.Choice taken at send time so
// subsequent OutChoice / OutChoicePatch mutations to the upstream
// Choice pointer don't leak into the settled card. Settled and
// SelectedID track the local view of the click outcome (set by
// publishActionChoice / publishActionInput via markSettled).
//
// mu guards every field on this struct. The choiceStore's own
// mutex only protects the map itself (insertion / lookup); per-
// state mutation runs on the runtime / callback goroutines, so
// each entry carries its own lock to keep the map mutex free
// during EditMessage / AcknowledgeInteraction network calls.
type choiceState struct {
	mu         sync.Mutex
	RequestID  string
	ChannelID  string
	MessageID  string
	Choice     *messages.Choice
	Step       int
	Picks      []string
	Settled    bool
	SelectedID string
}

// choiceStore is an in-memory map of in-flight choice prompts.
// Choice state is intentionally NOT persisted across daemon
// restarts — Discord Message Components V1 tokens expire 3 s
// after the click event anyway, so a stale state entry would
// only cause nuisance "this prompt has expired" replies on the
// user's next session. Mirrors feishu's in-memory choice map.
//
// Three lookups:
//   - Get / Put on the full RequestID (the canonical key).
//   - GetByShortID on the truncated shortRequestID embedded in
//     custom_id ("c:<short>:<idx>" / "i:<short>"). The lookup is
//     linear over the map; expected card population is small
//     (one card per active prompt per chat) so a map scan is
//     cheaper than maintaining a parallel index.
//   - GetByMessageID on the Discord message id for resolver
//     disambiguation (mirrors telegram/callback.go:resolveRequestID).
type choiceStore struct {
	mu      sync.RWMutex
	entries map[string]*choiceState
}

// newChoiceStore returns an empty in-memory store.
func newChoiceStore() *choiceStore {
	return &choiceStore{entries: make(map[string]*choiceState)}
}

// Put inserts or replaces the entry for the given RequestID.
// Caller is expected to populate the ChannelID / MessageID fields
// after the CreateMessage response lands (the store does NOT
// reach into the API).
func (s *choiceStore) Put(state *choiceState) error {
	if s == nil || state == nil || state.RequestID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[state.RequestID] = state
	return nil
}

// Get returns the entry for the given RequestID.
func (s *choiceStore) Get(requestID string) (*choiceState, bool) {
	if s == nil || requestID == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.entries[requestID]
	return state, ok
}

// GetByShortID walks the map looking for an entry whose
// shortID(state.RequestID) equals short. Used by the interaction
// handler to resolve "c:<short>:<idx>" / "i:<short>" clicks.
func (s *choiceStore) GetByShortID(short string) (*choiceState, bool) {
	if s == nil || short == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, state := range s.entries {
		if state == nil {
			continue
		}
		if shortID(state.RequestID) == short {
			return state, true
		}
	}
	return nil, false
}

// GetByMessageID returns the entry whose MessageID equals messageID.
// Used as a disambiguator when a short id maps to multiple candidates
// (mirrors telegram/callback.go:301-304).
func (s *choiceStore) GetByMessageID(messageID string) (*choiceState, bool) {
	if s == nil || messageID == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, state := range s.entries {
		if state == nil {
			continue
		}
		if state.MessageID == messageID {
			return state, true
		}
	}
	return nil, false
}

// shortID truncates a long RequestID to the form embedded in
// custom_id: 8-char prefix + "-" + 8-char suffix when length > 16,
// otherwise the original. Total length is at most 17 (the "-" + 8 + 8),
// well under Discord's 100-char custom_id ceiling even with the
// "c:" / "i:" prefix.
//
// Mirrors internal/channel/telegram/topic.go:243.
func shortID(value string) string {
	if len(value) <= 16 {
		return value
	}
	return value[:8] + "-" + value[len(value)-8:]
}

// choiceCustomID encodes a choice button's interaction tag.
// Format: "c:<short>:<idx>". At most 17 + len(strconv.Itoa(idx))
// bytes — comfortably under Discord's 100-char limit.
func choiceCustomID(state *choiceState, optionIndex int) string {
	return "c:" + shortID(state.RequestID) + ":" + strconv.Itoa(optionIndex)
}

// inputCustomID encodes the "Type your answer" button's tag.
// Format: "i:<short>". Reused as the modal-level custom_id on
// the modal envelope (Discord echoes it back on MODAL_SUBMIT).
func inputCustomID(state *choiceState) string {
	return "i:" + shortID(state.RequestID)
}

// maxOptionButtons caps the number of choice buttons rendered on
// a single card. Discord allows at most 5 ActionRows per message;
// one row is reserved for the "Type your answer" button when
// the choice has at least one Question. 4 rows × 5 = 20.
const maxOptionButtons = 20

// cloneChoiceValue deep-copies a *messages.Choice so the store's
// snapshot is independent of upstream mutations. Mirrors
// internal/channel/telegram/adapter.go:2087+.
func cloneChoiceValue(choice *messages.Choice) *messages.Choice {
	if choice == nil {
		return nil
	}
	copy := *choice
	copy.Options = append([]messages.ChoiceOption(nil), choice.Options...)
	copy.Questions = append([]messages.ChoiceQuestion(nil), choice.Questions...)
	for questionIndex := range copy.Questions {
		copy.Questions[questionIndex].Options = append(
			[]messages.ChoiceOption(nil), choice.Questions[questionIndex].Options...,
		)
	}
	return &copy
}

// currentOptions returns the option list the user is choosing
// from at the current step — AskUserQuestion's per-step slice for
// multi-step choices, or the top-level Options for permissions /
// gtw decisions. Locks the state's mutex while reading Choice /
// Step so concurrent OutChoicePatch / handleChoiceClick goroutines
// can't tear the read.
func (s *choiceState) currentOptions() []messages.ChoiceOption {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Choice == nil {
		return nil
	}
	if len(s.Choice.Questions) > 0 {
		if s.Step >= 0 && s.Step < len(s.Choice.Questions) {
			return s.Choice.Questions[s.Step].Options
		}
		return nil
	}
	return s.Choice.Options
}

// choiceAndStep returns the Choice pointer and the current Step
// under the state mutex. Used by renderChoiceContent, which needs
// both fields atomically.
func (s *choiceState) choiceAndStep() (*messages.Choice, int) {
	if s == nil {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Choice, s.Step
}

// hasQuestions reports whether the current Choice has any
// AskUserQuestion items (used to gate the "Type your answer"
// button row in buildChoiceComponents).
func (s *choiceState) hasQuestions() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Choice != nil && len(s.Choice.Questions) > 0
}

// applyPatch replaces the mutable fields with the supplied
// choice snapshot + settled/selected-id. Called by patchChoice
// (OutChoicePatch path) under the state mutex so the callback
// goroutine's markSettled / publishActionChoice cannot observe a
// half-updated state.
func (s *choiceState) applyPatch(choice *messages.Choice, settled bool, selectedID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Choice = choice
	s.Settled = settled
	if selectedID != "" || settled {
		s.SelectedID = selectedID
	}
}

// markSettledLocal flips the local settled flag and persists the
// selected option id (when non-empty). Mutates the state under
// the state mutex.
func (s *choiceState) markSettledLocal(selectedID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Settled = true
	if selectedID != "" {
		s.SelectedID = selectedID
	}
}
