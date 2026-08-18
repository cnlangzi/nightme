package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// StateStore persists per-run state to disk so in-flight runs
// survive daemon restarts. One JSON file per run under StateDir.
//
// Format is the same as wfe.RunState (JSON-tagged) plus a
// `version` field for forward-compat migrations.
type StateStore struct {
	dir string
}

func NewStateStore(dir string) (*StateStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("bot: state dir: %w", err)
	}
	return &StateStore{dir: dir}, nil
}

// Save atomically writes the state. tmp + rename, 0600 (local
// state, not readable by other users).
func (s *StateStore) Save(state *wfe.RunState) error {
	if state == nil {
		return errors.New("bot: nil state")
	}
	path := filepath.Join(s.dir, state.RunID+".json")
	tmp := path + ".tmp"

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("bot: marshal state: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("bot: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("bot: rename: %w", err)
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
		return nil, fmt.Errorf("bot: unmarshal state: %w", err)
	}
	return &state, nil
}

// fireWorkflow is the callback invoked by TriggerManager when a
// (workflow, event, workspace) match is found. It:
//  1. derives a runID + chatID
//  2. registers a botRun (per-run reply channel) in runsByChatID
//  3. pushes setup messages to bot.Incoming (cwd, use agent)
//  4. spawns a goroutine that drives wfe.Tick
//  5. on completion, removes the run from runsByChatID
func (b *Bot) onTrigger(ctx context.Context, wf *wfe.Workflow, ev wfe.Event, workspace string) {
	runID := fmt.Sprintf("%s-%s-%d", wf.Name, sanitize(workspace), time.Now().UnixNano())
	chatID := "bot:wf:" + runID

	r := &botRun{
		runID:     runID,
		chatID:    chatID,
		workspace: workspace,
		workflow:  wf,
		env:       defaultBotEnv(),
		reply:     make(chan string, 1),
	}
	b.muRuns.Lock()
	b.runsByChatID[chatID] = r
	b.muRuns.Unlock()

	b.logger.Info("bot: fireWorkflow", "workflow", wf.Name, "workspace", workspace, "run_id", runID, "chat_id", chatID)

	go b.driveRun(ctx, r, ev)
}

// driveRun is the per-run goroutine. It pushes setup messages,
// then loops wfe.Tick until the run terminates. Saves state
// after every tick.
func (b *Bot) driveRun(ctx context.Context, r *botRun, ev wfe.Event) {
	// Cleanup on exit.
	defer func() {
		b.muRuns.Lock()
		delete(b.runsByChatID, r.chatID)
		b.muRuns.Unlock()
		// Drain any remaining reply so the channel can be GC'd.
		select {
		case <-r.reply:
		default:
		}
	}()

	// 1. Setup messages: /cwd first, then /use agent (if workflow
	// has a top-level agent), pushed into bot.Incoming.
	setupMsgs := buildSetupMessages(r, ev)
	for _, m := range setupMsgs {
		select {
		case b.in <- m:
		case <-ctx.Done():
			return
		}
	}

	// 2. Initialize or resume state.
	state, err := b.stateStore.Load(r.runID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.logger.Error("bot: load state", "run_id", r.runID, "err", err)
			return
		}
		// fresh run
		state = newRunState(r.runID, r.workflow, r.workspace, ev, r.chatID, r.env, time.Now())
	} else {
		// resume: refresh env / chat / workspace (defensive — they
		// shouldn't change but be safe)
		state.Env = mergeEnv(r.env, state.Env)
		state.ChatID = r.chatID
		state.Workspace = r.workspace
		state.UpdatedAt = time.Now()
	}

	// 3. Tick loop.
	rt := &botRuntime{bot: b, run: r}
	for state.Status == wfe.StatusRunning {
		newState, err := wfe.Tick(ctx, state, r.workflow, rt)
		if err != nil {
			b.logger.Error("bot: tick", "run_id", r.runID, "err", err)
		}
		state = newState
		if perr := b.stateStore.Save(state); perr != nil {
			b.logger.Error("bot: save state", "run_id", r.runID, "err", perr)
		}
		if ctx.Err() != nil {
			state.Status = wfe.StatusCancelled
			b.stateStore.Save(state)
			return
		}
	}
	b.logger.Info("bot: run finished", "run_id", r.runID, "status", state.Status)
}

// buildSetupMessages returns the messages bot should push to
// Incoming() at run start, to set up the chat (cwd, agent) before
// the workflow's prompt step dispatches.
//
// Format mirrors slash-command syntax so the existing nightme
// command dispatcher picks them up:
//   - "/cwd <workspace>"     → sets the chat's CWD (first step
//     of any chat session, per WFE.md §3.5).
//   - "/use agent <name>"    → sets the chat's active agent
//     (only if workflow-level agent is configured).
func buildSetupMessages(r *botRun, _ wfe.Event) []messages.InboundMessage {
	msgs := []messages.InboundMessage{
		{
			ChatID: r.chatID,
			Text:   "/cwd " + r.workspace,
			Time:   time.Now(),
		},
	}
	if r.workflow.Agent != "" {
		msgs = append(msgs, messages.InboundMessage{
			ChatID: r.chatID,
			Text:   "/use agent " + r.workflow.Agent,
			Time:   time.Now(),
		})
	}
	return msgs
}

// defaultBotEnv returns env entries that bot always injects into
// every run (e.g. the workspace path, secrets loaded from
// nightme config). v0: just the workspace; secrets loading from
// nightme config is a Phase 1 task.
func defaultBotEnv() map[string]string {
	return map[string]string{
		"WORKSPACE_CWD": "", // populated per-run when driveRun starts
	}
}

// mergeEnv overlays override onto base.
func mergeEnv(base, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// sanitize converts a workspace path into a runID-safe component.
// Strips leading slashes (root), replaces remaining slashes with
// '_'. Replaces '~' with 'home' to avoid shell-meaningful chars.
func sanitize(workspace string) string {
	s := workspace
	if len(s) > 0 && s[0] == '~' {
		s = "home" + s[1:]
	}
	s = filepath.Clean(s)
	s = strings.TrimLeft(s, "/")
	r := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		" ", "_",
		":", "_",
	)
	return r.Replace(s)
}

// newRunState is a helper to build a fresh RunState with all
// fields populated. Local to bot (avoids coupling wfe to bot's
// persistence model). Mirrors wfe.RunState's JSON shape.
func newRunState(runID string, wf *wfe.Workflow, workspace string, ev wfe.Event, chatID string, env map[string]string, now time.Time) *wfe.RunState {
	var wfName string
	if wf != nil {
		wfName = wf.Name
	}
	return &wfe.RunState{
		RunID:        runID,
		WorkflowName: wfName,
		Workspace:    workspace,
		Status:       wfe.StatusRunning,
		Env:          env,
		Event:        ev,
		ChatID:       chatID,
		StartedAt:    now,
		UpdatedAt:    now,
	}
}

// Unused but kept for future: stub for runtime pool.
var _ = sync.Mutex{}
