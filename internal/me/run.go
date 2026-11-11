package me

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/timeouts"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// run is one workflow execution: the chat it drives, the workspace
// it works in, and the workflow it follows.
type run struct {
	runID     string
	chatID    string
	workspace string
	workflow  *wfe.Workflow
	env       map[string]string
}

// onTrigger is the callback the TriggerManager invokes when a
// (workflow, event, workspace) triple matches. It starts the run in
// its own goroutine — the trigger fires from the cron scheduler and
// must not wait for the workflow.
func (s *Service) onTrigger(ctx context.Context, wf *wfe.Workflow, ev wfe.Event, workspace string) {
	r := s.startRun(wf, workspace)
	s.logger.Info("me: fireWorkflow",
		"workflow", wf.Name,
		"workspace", workspace,
		"run_id", r.runID,
		"chat_id", r.chatID)
	s.runs.Add(1)
	go func() {
		defer s.runs.Done()
		s.driveRun(ctx, r, ev)
	}()
}

// startRun derives the identity of a run: its id, and the chat it
// drives. The chat id lives in the loopback namespace — the daemon
// only accepts chat ids a channel owns.
func (s *Service) startRun(wf *wfe.Workflow, workspace string) *run {
	runID := fmt.Sprintf("%s-%s-%d", wf.Name, sanitize(workspace), time.Now().UnixNano())
	return &run{
		runID:     runID,
		chatID:    loopback.ChatIDPrefix + runID,
		workspace: workspace,
		workflow:  wf,
		env:       defaultEnv(workspace),
	}
}

// driveRun is the body of a run: it sets the chat up, then loops
// wfe.Tick until the run terminates. The state is saved after every
// tick, so an interrupted run leaves a trail behind.
func (s *Service) driveRun(ctx context.Context, r *run, ev wfe.Event) {
	if s.stateStore == nil {
		s.logger.Error("me: no state store; the run cannot be persisted", "run_id", r.runID)
		return
	}
	state := newRunState(r.runID, r.workflow, r.workspace, ev, r.chatID, r.env, time.Now())

	// The chat learns its workspace and its agent before the first
	// step runs. A setup command that did not do its job fails the
	// run: every step after it would work against a chat that is
	// not ready.
	if err := s.setupChat(ctx, r); err != nil {
		s.logger.Error("me: setup", "run_id", r.runID, "err", err)
		state.Status = wfe.StatusFailed
		state.UpdatedAt = time.Now()
		if perr := s.stateStore.Save(state); perr != nil {
			s.logger.Error("me: save state", "run_id", r.runID, "err", perr)
		}
		return
	}

	rt := &runRuntime{svc: s, run: r}
	for state.Status == wfe.StatusRunning {
		newState, err := wfe.Tick(ctx, state, r.workflow, rt)
		if err != nil {
			s.logger.Error("me: tick", "run_id", r.runID, "err", err)
			if newState != nil && newState.Status == wfe.StatusRunning {
				// The tick made no progress — the step could not
				// be evaluated. Ticking it again would spin
				// forever, so the run ends here.
				newState.Status = wfe.StatusFailed
			}
		}
		if newState != nil {
			state = newState
		}
		if perr := s.stateStore.Save(state); perr != nil {
			s.logger.Error("me: save state", "run_id", r.runID, "err", perr)
		}
		if ctx.Err() != nil {
			state.Status = wfe.StatusCancelled
			if perr := s.stateStore.Save(state); perr != nil {
				s.logger.Error("me: save state", "run_id", r.runID, "err", perr)
			}
			return
		}
	}
	s.logger.Info("me: run finished", "run_id", r.runID, "status", state.Status)
}

// setupChat points the chat at the workflow's workspace and selects
// the workflow's agent.
//
// The commands are sent one at a time and each one is awaited: the
// daemon answers /cwd before /use is sent, so /use never runs
// against a chat that has no workspace yet.
func (s *Service) setupChat(ctx context.Context, r *run) error {
	if err := s.setupStep(ctx, r, "/cwd "+r.workspace); err != nil {
		return err
	}
	if r.workflow.Agent != "" {
		if err := s.setupStep(ctx, r, "/use "+r.workflow.Agent); err != nil {
			return err
		}
	}
	return nil
}

// setupStep sends one setup command and checks the outcome it
// produced.
//
// The wait is bounded: a command the daemon never settles would
// otherwise hang the run forever.
func (s *Service) setupStep(ctx context.Context, r *run, text string) error {
	ctx, cancel := context.WithTimeout(ctx, timeouts.CLI)
	defer cancel()

	ex, err := s.client.Do(ctx, r.chatID, text)
	if err != nil {
		return fmt.Errorf("me: setup %q: %w", text, err)
	}
	if ex.Outcome.Status != messages.OutcomeOK {
		return fmt.Errorf("me: setup %q: %s: %s", text, ex.Outcome.Status, ex.Outcome.Summary)
	}
	return nil
}

// defaultEnv returns the env entries every run starts with.
func defaultEnv(workspace string) map[string]string {
	return map[string]string{
		"WORKSPACE_CWD": workspace,
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
// Cross-platform: filepath.Clean on Windows converts "/" to "\"
// AFTER Clean, so ToSlash must run AFTER Clean to keep "/" as
// the canonical separator. Replaces '~' with 'home' to avoid
// shell-meaningful chars.
func sanitize(workspace string) string {
	s := workspace
	if len(s) > 0 && s[0] == '~' {
		s = "home" + s[1:]
	}
	// Clean first (collapses "..", removes double slashes, etc).
	// Then ToSlash to normalize Windows "\" back to "/" so the
	// TrimLeft below strips the root prefix consistently.
	s = filepath.Clean(s)
	s = filepath.ToSlash(s)
	s = strings.TrimLeft(s, "/")
	r := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		" ", "_",
		":", "_",
	)
	return r.Replace(s)
}

// newRunState builds a fresh RunState with all fields populated.
// Local to me (avoids coupling wfe to me's persistence model).
// Mirrors wfe.RunState's JSON shape.
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
