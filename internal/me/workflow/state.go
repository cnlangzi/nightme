package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// StateStore persists the per-run state to disk. One JSON file per
// run under StateDir.
//
// The state is the run's record: it is written after every tick, so
// it shows how far a run got, what each step produced, and how the
// run ended.
//
// Format is the same as wfe.RunState (JSON-tagged).
type StateStore struct {
	dir string
}

func NewStateStore(dir string) (*StateStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("me: state dir: %w", err)
	}
	return &StateStore{dir: dir}, nil
}

// Save atomically writes the state. tmp + rename, 0600 (local
// state, not readable by other users).
func (s *StateStore) Save(state *wfe.RunState) error {
	if state == nil {
		return errors.New("me: nil state")
	}
	path := filepath.Join(s.dir, state.RunID+".json")
	tmp := path + ".tmp"

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("me: marshal state: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("me: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("me: rename: %w", err)
	}
	return nil
}

// Load reads a state file. Returns os.ErrNotExist when the run
// has never been persisted.
func (s *StateStore) Load(runID string) (*wfe.RunState, error) {
	path := filepath.Join(s.dir, runID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state wfe.RunState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("me: unmarshal state: %w", err)
	}
	return &state, nil
}
