package telegram

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

func TestStateStore_TopicLifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := newStateStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	state := &TopicState{ChatID: "100", TopicID: 1}
	if err := store.putTopic(state); err != nil {
		t.Fatalf("putTopic: %v", err)
	}
	got, ok := store.topic("100", 1)
	if !ok || got.ChatID != "100" {
		t.Fatalf("topic lookup failed: %+v", got)
	}
	state.PlaceholderMessageID = 42
	if err := store.putTopic(state); err != nil {
		t.Fatalf("putTopic update: %v", err)
	}
	got, _ = store.topic("100", 1)
	if got.PlaceholderMessageID != 42 {
		t.Fatalf("placeholder not updated: %+v", got)
	}
}

func TestStateStore_TopicForChat(t *testing.T) {
	dir := t.TempDir()
	store, err := newStateStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	_ = store.putTopic(&TopicState{ChatID: "100", TopicID: 1})
	_ = store.putTopic(&TopicState{ChatID: "100", TopicID: 2})
	state, ok := store.topicForChat("100")
	if !ok || state.TopicID != 1 {
		t.Fatalf("topicForChat should return first topic, got %+v ok=%v", state, ok)
	}
}

func TestStateStore_ChoiceLifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := newStateStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	choice := &messages.Choice{
		RequestID: "req-1",
		Kind:      messages.ChoiceKindPermission,
		Title:     "Perm",
		Options: []messages.ChoiceOption{
			{ID: "yes", Label: "Yes"},
			{ID: "no", Label: "No"},
		},
	}
	state := &ChoiceState{
		RequestID: "req-1",
		ChatID:    "100",
		TopicID:   1,
		Choice:    choice,
		Step:      0,
		Picks:     []string{},
	}
	if err := store.putChoice(state); err != nil {
		t.Fatalf("putChoice: %v", err)
	}
	got, ok := store.choiceByRequestID("req-1")
	if !ok || got.RequestID != "req-1" {
		t.Fatalf("choiceByRequestID failed: %+v", got)
	}
}

func TestStateStore_ChoiceByMessageID(t *testing.T) {
	dir := t.TempDir()
	store, err := newStateStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	state := &ChoiceState{
		RequestID: "req-1",
		MessageID: 99,
		ChatID:    "100",
		TopicID:   1,
		Choice:    &messages.Choice{RequestID: "req-1", Kind: messages.ChoiceKindPermission},
	}
	_ = store.putChoice(state)
	got, ok := store.choiceByMessageID(99)
	if !ok || got.RequestID != "req-1" {
		t.Fatalf("choiceByMessageID failed: %+v", got)
	}
}

func TestStateStore_ChoiceByShortID(t *testing.T) {
	dir := t.TempDir()
	store, err := newStateStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	state := &ChoiceState{
		RequestID: "req-very-long-id-12345678",
		MessageID: 99,
		ChatID:    "100",
		TopicID:   1,
		Choice:    &messages.Choice{RequestID: "req-very-long-id-12345678", Kind: messages.ChoiceKindPermission},
	}
	_ = store.putChoice(state)
	got, ok := store.choiceByShortID(shortID(state.RequestID))
	if !ok || got.RequestID != state.RequestID {
		t.Fatalf("choiceByShortID failed: %+v ok=%v", got, ok)
	}
}

func TestStateStore_PendingInput(t *testing.T) {
	dir := t.TempDir()
	store, err := newStateStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	state := &ChoiceState{
		RequestID: "req-1",
		ChatID:    "100",
		TopicID:   1,
		Choice:    &messages.Choice{RequestID: "req-1", Kind: messages.ChoiceKindQuestion},
		Input: &InputState{
			PromptMessageID: 200,
			QuestionID:      "q1",
			Step:            0,
			Kind:            "question",
			OwnerID:         42,
		},
	}
	_ = store.putChoice(state)
	got, ok := store.pendingInput("100", 42, 200)
	if !ok || got.RequestID != "req-1" {
		t.Fatalf("pendingInput failed: %+v", got)
	}
	_, ok = store.pendingInput("100", 999, 200)
	if ok {
		t.Fatal("pendingInput should reject wrong owner")
	}
	_, ok = store.pendingInput("100", 42, 999)
	if ok {
		t.Fatal("pendingInput should reject wrong prompt id")
	}
}

func TestStateStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := newStateStore(path)
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	_ = store.putTopic(&TopicState{ChatID: "100", TopicID: 1, PlaceholderMessageID: 5})
	_ = store.putChoice(&ChoiceState{
		RequestID: "req-persist",
		ChatID:    "100",
		TopicID:   1,
		MessageID: 50,
		Choice:    &messages.Choice{RequestID: "req-persist", Kind: messages.ChoiceKindPermission},
	})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	store2, err := newStateStore(path)
	if err != nil {
		t.Fatalf("newStateStore reload: %v", err)
	}
	if _, ok := store2.topic("100", 1); !ok {
		t.Fatal("topic not reloaded")
	}
	if _, ok := store2.choiceByRequestID("req-persist"); !ok {
		t.Fatal("choice not reloaded")
	}
}

func TestStateStore_MissingFile(t *testing.T) {
	dir := t.TempDir()
	store, err := newStateStore(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("newStateStore missing: %v", err)
	}
	if store == nil || len(store.topics) != 0 || len(store.choices) != 0 {
		t.Fatalf("missing file should yield empty store")
	}
}

func TestStateStore_EmptyPath(t *testing.T) {
	store, err := newStateStore("")
	if err != nil {
		t.Fatalf("newStateStore empty: %v", err)
	}
	if store == nil {
		t.Fatal("empty path should yield store")
	}
	if err := store.putTopic(&TopicState{ChatID: "100", TopicID: 1}); err != nil {
		t.Fatalf("putTopic empty path: %v", err)
	}
}

// TestStateStore_PruneOlderThan locks the on-load GC contract:
// topics updated before the cutoff are removed; topics updated
// after the cutoff are kept. Choices bound to a pruned topic are
// also dropped (orphaned choices are unreachable). See
// stateStoreTTL in state.go.
//
// putTopic always bumps UpdatedAt to now (active-write semantic),
// so we seed the test fixtures by reaching into s.topics directly
// to simulate entries that have been idle for days / months.
func TestStateStore_PruneOlderThan(t *testing.T) {
	store, err := newStateStore("")
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	now := time.Now().UTC()
	store.mu.Lock()
	store.topics[store.topicKey("100", 1)] = &TopicState{
		ChatID: "100", TopicID: 1,
		UpdatedAt: now.Add(-1 * time.Hour), // fresh
	}
	store.topics[store.topicKey("100", 2)] = &TopicState{
		ChatID: "100", TopicID: 2,
		UpdatedAt: now.Add(-72 * time.Hour), // stale
	}
	store.topics[store.topicKey("200", 0)] = &TopicState{
		ChatID: "200", TopicID: 0,
		UpdatedAt: now.Add(-365 * 24 * time.Hour), // ancient
	}
	store.topics[store.topicKey("300", 0)] = &TopicState{
		ChatID: "300", TopicID: 0,
		UpdatedAt: time.Time{}, // legacy (zero)
	}
	store.choices["stale-choice"] = &ChoiceState{
		RequestID: "stale-choice",
		ChatID:    "100", TopicID: 2,
		Choice: &messages.Choice{RequestID: "stale-choice", Kind: messages.ChoiceKindPermission},
	}
	store.choices["fresh-choice"] = &ChoiceState{
		RequestID: "fresh-choice",
		ChatID:    "100", TopicID: 1,
		Choice: &messages.Choice{RequestID: "fresh-choice", Kind: messages.ChoiceKindPermission},
	}
	store.mu.Unlock()
	cutoff := now.Add(-24 * time.Hour)
	removed := store.pruneOlderThan(cutoff)
	if removed != 3 {
		t.Fatalf("pruneOlderThan removed = %d, want 3 (stale + ancient + legacy)", removed)
	}
	if _, ok := store.topic("100", 1); !ok {
		t.Fatal("fresh topic must survive prune")
	}
	if _, ok := store.topic("100", 2); ok {
		t.Fatal("stale topic must be pruned")
	}
	if _, ok := store.topic("200", 0); ok {
		t.Fatal("ancient topic must be pruned")
	}
	if _, ok := store.topic("300", 0); ok {
		t.Fatal("legacy (zero UpdatedAt) topic must be pruned")
	}
	if _, ok := store.choiceByRequestID("stale-choice"); ok {
		t.Fatal("choice bound to pruned topic must be dropped")
	}
	if _, ok := store.choiceByRequestID("fresh-choice"); !ok {
		t.Fatal("choice bound to live topic must survive prune")
	}
}

// TestStateStore_NewStateStoreTrimsOnLoad locks that loading a
// stale state file automatically trims entries older than
// stateStoreTTL and persists the trimmed baseline back to disk.
func TestStateStore_NewStateStoreTrimsOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// First newStateStore creates the file with our seed entries.
	store, err := newStateStore(path)
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	now := time.Now().UTC()
	store.mu.Lock()
	store.topics[store.topicKey("100", 1)] = &TopicState{
		ChatID: "100", TopicID: 1,
		UpdatedAt: now.Add(-365 * 24 * time.Hour), // ancient
	}
	store.topics[store.topicKey("200", 0)] = &TopicState{
		ChatID: "200", TopicID: 0,
		UpdatedAt: now.Add(-1 * time.Hour), // fresh
	}
	store.mu.Unlock()
	// Persist directly via save() (bypass putTopic which bumps UpdatedAt).
	if err := store.save(); err != nil {
		t.Fatalf("save seed: %v", err)
	}
	// Reload — on-load prune runs.
	store2, err := newStateStore(path)
	if err != nil {
		t.Fatalf("reload newStateStore: %v", err)
	}
	if _, ok := store2.topic("100", 1); ok {
		t.Fatal("ancient topic must be pruned on load")
	}
	if _, ok := store2.topic("200", 0); !ok {
		t.Fatal("fresh topic must survive load")
	}
	// The on-load save() should have written the trimmed state
	// back; verify by reading the raw JSON file directly.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if string(data) == "" {
		t.Fatal("state.json should have been rewritten by on-load prune")
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("short"); got != "short" {
		t.Errorf("shortID short = %q", got)
	}
	// Length == 16 stays as-is (per Telegram's 64-byte callback_data budget).
	if got := shortID("0123456789abcdef"); got != "0123456789abcdef" {
		t.Errorf("shortID 16 = %q, want unchanged", got)
	}
	// Length > 16 collapses to first 8 + "-" + last 8.
	if got := shortID("0123456789abcdefABCDEF12"); got != "01234567-ABCDEF12" {
		t.Errorf("shortID long = %q", got)
	}
}

func TestStringInt(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{12345, "12345"},
	}
	for _, tc := range cases {
		if got := stringInt(tc.in); got != tc.want {
			t.Errorf("stringInt(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
