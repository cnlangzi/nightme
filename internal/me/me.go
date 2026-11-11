// Package me — the user half of nightme.
//
// A workflow automation needs somebody to drive the daemon: set the
// chat's workspace, pick the agent, ask the question, read the
// answer.
//
// me is that user, and nothing else. It is a runtime service, not a
// channel: it never receives inbound messages and it renders
// nothing. It talks to the daemon through the loopback channel's
// client, the way a person types into a chat window — every command
// is a real message with a real id, and every answer comes back as
// a structured outcome.
//
// The split:
//
//   - internal/channel/loopback — transport. How a message gets
//     into the daemon, and how its outcome comes back.
//   - internal/me — the workflow half. Triggers, actions, run
//     state, and the steps that drive the daemon.
package me

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// CommandRunner is the slice of the loopback client the service
// needs: send a command into a chat and wait for it to settle.
//
// The service depends on the interface rather than on the concrete
// client, so a run can be driven without a daemon behind it.
type CommandRunner interface {
	// Do sends text into chatID and returns the execution once the
	// daemon settled it.
	Do(ctx context.Context, chatID, text string) (loopback.Execution, error)
}

// Config holds the service's external configuration.
type Config struct {
	// WorkflowsDir is the directory the service scans for *.yaml
	// workflow files. The action scripts live in <WorkflowsDir>/actions
	// and the per-run state in <WorkflowsDir>/state.
	WorkflowsDir string
}

// Service runs workflow automations on behalf of the daemon.
//
// It owns the workflow catalogue, the cron triggers, the action
// registry and the per-run state. The service has its own
// lifecycle: Start loads the workflows and arms the triggers, Stop
// disarms them.
type Service struct {
	cfg    Config
	logger *slog.Logger
	client CommandRunner

	workflows  []*wfe.Workflow
	triggers   *TriggerManager
	actions    *ActionRegistry
	stateStore *StateStore

	mu      sync.Mutex
	running bool
}

// New constructs a Service. The workflows are loaded at Start, not
// here.
func New(cfg Config, client CommandRunner) *Service {
	return &Service{
		cfg:    cfg,
		logger: slog.Default(),
		client: client,
	}
}

// SetLogger installs the logger the service logs through.
func (s *Service) SetLogger(l *slog.Logger) {
	if l == nil {
		return
	}
	s.logger = l
}

// Start loads the workflows, prepares the actions and the state
// store, and arms the triggers.
//
// An empty or missing workflows directory is not an error: the
// service starts and stays idle — it has nothing to trigger and
// nothing to run.
func (s *Service) Start(ctx context.Context) error {
	if s.client == nil {
		return errors.New("me: no command runner — the service cannot drive the daemon")
	}
	if s.cfg.WorkflowsDir == "" {
		s.logger.Info("me: no workflows dir; the service stays idle")
		s.markStarted()
		return nil
	}

	wfs, _, err := wfe.LoadDir(s.cfg.WorkflowsDir)
	if err != nil {
		return fmt.Errorf("me: load workflows: %w", err)
	}
	s.workflows = wfs
	s.logger.Info("me: loaded workflows", "count", len(wfs), "dir", s.cfg.WorkflowsDir)

	wsMap, err := buildWorkspaceRepoMap(ctx, wfs)
	if err != nil {
		return fmt.Errorf("me: build workspace map: %w", err)
	}

	actions := NewActionRegistry()
	registerBuiltinActions(actions)
	if err := actions.ScanUserScripts(s.actionsDir()); err != nil {
		return fmt.Errorf("me: scan action scripts: %w", err)
	}
	s.actions = actions

	if len(wfs) > 0 {
		store, err := NewStateStore(s.stateDir())
		if err != nil {
			return fmt.Errorf("me: init state store: %w", err)
		}
		s.stateStore = store
	}

	s.triggers = newTriggerManager(wfs, wsMap, s.onTrigger, s.logger)
	if err := s.triggers.Start(ctx); err != nil {
		return err
	}
	s.markStarted()
	return nil
}

// Stop disarms the triggers. Idempotent.
func (s *Service) Stop(context.Context) error {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
	if s.triggers != nil {
		return s.triggers.Stop()
	}
	return nil
}

// started reports whether the service is running.
func (s *Service) started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// markStarted records that the service is up.
func (s *Service) markStarted() {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
}

// actionsDir is the directory the user-defined action scripts live
// in.
func (s *Service) actionsDir() string {
	return filepath.Join(s.cfg.WorkflowsDir, "actions")
}

// stateDir is the directory the per-run state is persisted in.
func (s *Service) stateDir() string {
	return filepath.Join(s.cfg.WorkflowsDir, "state")
}
