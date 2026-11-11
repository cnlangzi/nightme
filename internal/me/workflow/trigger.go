package workflow

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/cnlangzi/nightme/internal/proc"
	"github.com/cnlangzi/nightme/internal/timeouts"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// TriggerManager subscribes to cron ticks and runs the workspace
// filter (3-stage pipeline: receive → filter → trigger), invoking
// the service's onTrigger for each (workflow, event, workspace)
// match.
//
// v0: cron-only. Git event support (PR / issue / branch / mention
// triggers) is planned but deferred. When re-enabled, the
// EventSource abstraction will return to this file; until then,
// this struct holds no event source field.
//
// Locked design: the trigger manager is fully self-contained. It
// does NOT route through the gateway's dispatch chain; it produces
// events that the service processes.
type TriggerManager struct {
	workflows []*wfe.Workflow
	wsMap     *workspaceRepoMap
	onTrigger func(ctx context.Context, wf *wfe.Workflow, ev wfe.Event, workspace string)
	logger    *slog.Logger

	cron *cron.Cron
	mu   sync.Mutex
}

// newTriggerManager constructs a TriggerManager.
func newTriggerManager(
	workflows []*wfe.Workflow,
	wsMap *workspaceRepoMap,
	onTrigger func(ctx context.Context, wf *wfe.Workflow, ev wfe.Event, workspace string),
	logger *slog.Logger,
) *TriggerManager {
	return &TriggerManager{
		workflows: workflows,
		wsMap:     wsMap,
		onTrigger: onTrigger,
		logger:    logger,
	}
}

// Start launches the cron scheduler. Each cron tick for a workflow's
// schedule entries becomes a "schedule" event routed through the
// 3-stage filter (which currently matches any workflow whose on:
// has a schedule entry — for cron events, repo doesn't apply).
func (t *TriggerManager) Start(ctx context.Context) error {
	t.mu.Lock()
	t.cron = cron.New()
	t.mu.Unlock()

	// cron: register a tick for each workflow's schedule entries
	for _, wf := range t.workflows {
		if wf.On.Schedule == nil {
			continue
		}
		for _, s := range wf.On.Schedule {
			expr := s.Cron
			if _, err := t.cron.AddFunc(expr, func() {
				ev := wfe.Event{
					Kind: "schedule",
					Time: time.Now(),
					Data: map[string]any{"cron": expr},
				}
				t.onEvent(ctx, ev)
			}); err != nil {
				// A malformed expression would otherwise register
				// nothing and the workflow would never fire, with
				// no trace anywhere.
				t.logger.Warn("trigger: bad cron expression; the workflow will not fire",
					"workflow", wf.Name, "cron", expr, "err", err)
			}
		}
	}

	t.cron.Start()
	t.logger.Info("trigger: started (cron-only)", "cron_jobs", len(t.cron.Entries()))
	return nil
}

// Stop stops the cron scheduler. Idempotent.
func (t *TriggerManager) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cron == nil {
		return nil
	}
	<-t.cron.Stop().Done()
	t.cron = nil
	return nil
}

// Entries returns the number of cron jobs the manager registered.
func (t *TriggerManager) Entries() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cron == nil {
		return 0
	}
	return len(t.cron.Entries())
}

// onEvent is the callback fired by the cron scheduler for each tick.
// Stage 2: filter (cron events always match if any workflow has a
// matching schedule entry).
func (t *TriggerManager) onEvent(ctx context.Context, ev wfe.Event) {
	// cron events: no event.repo; fire all workflows that match
	// the schedule entry. Iterating per-workspace fires once per
	// workspace in each matching workflow.
	if ev.Kind == "schedule" {
		for _, wf := range t.workflows {
			if !wfe.Match(wf, ev) {
				continue
			}
			for _, ws := range wf.Workspaces {
				t.onTrigger(ctx, wf, ev, ws)
			}
		}
		return
	}
	// Other event kinds (pr / issue / branch / mention) are not
	// handled in v0 — the service is cron-only until git event
	// support is re-introduced. Log and drop so users see something
	// happened.
	t.logger.Warn("trigger: dropping unhandled event (git events not yet wired)", "kind", ev.Kind)
}

// workspaceRepoMap maps a git remote URL (e.g. "cnlangzi/nightme")
// to the workspace path (e.g. "~/work/nightme"). Built at startup
// by reading `git -C <workspace> remote get-url origin` for every
// workspace mentioned in any workflow.
//
// In v0 (cron-only) this map is unused but kept constructed
// because it's cheap and lets us bring back git triggers without
// restructuring. A workspace the map cannot resolve is skipped, not
// fatal: the map serves triggers that are not wired yet, and one
// workspace without a git origin must not stop the cron workflows
// from running.
type workspaceRepoMap struct {
	byRepo map[string]string
	byPath map[string]string
}

// expandWorkspace expands a leading "~" to the user's home
// directory.
//
// A workflow that names its workspace "~/work/repo" has to reach
// the shell and git as an absolute path: neither expands "~". The
// /cwd command expands it too, so the chat and the shell agree on
// where the run works.
func expandWorkspace(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home, p[2:])
	}
	return p
}

func buildWorkspaceRepoMap(ctx context.Context, wfs []*wfe.Workflow, logger *slog.Logger) *workspaceRepoMap {
	m := &workspaceRepoMap{
		byRepo: map[string]string{},
		byPath: map[string]string{},
	}
	seen := map[string]bool{}
	for _, wf := range wfs {
		for _, ws := range wf.Workspaces {
			if seen[ws] {
				continue
			}
			seen[ws] = true
			repo := gitOrigin(ctx, ws)
			if repo == "" {
				logger.Warn("me: workspace has no git origin; it cannot match git events",
					"workflow", wf.Name, "workspace", ws)
				continue
			}
			m.byRepo[repo] = ws
			m.byPath[ws] = repo
		}
	}
	return m
}

// gitOrigin returns the canonical owner/repo for a workspace's
// git origin, or empty string if it can't be determined.
//
// The command is read-only, so it never touches .git/index.lock.
// It goes through proc.New so the platform-spanning cmd.Cancel /
// WaitDelay shape stays consistent across the daemon, and so the
// call gets a context-bound timeout for free.
func gitOrigin(ctx context.Context, workspace string) string {
	runCtx, cancel := context.WithTimeout(ctx, timeouts.CLI)
	defer cancel()
	cmd := proc.New(runCtx, "git", "-C", workspace, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return canonicalRepoURL(strings.TrimSpace(string(out)))
}

// canonicalRepoURL normalizes a git remote URL to "owner/repo".
// Handles both HTTPS (https://github.com/foo/bar.git) and SSH
// (git@github.com:foo/bar.git) forms, with or without .git suffix.
func canonicalRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	// Strip protocol
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	} else if strings.HasPrefix(raw, "git@") {
		raw = strings.TrimPrefix(raw, "git@")
		if i := strings.Index(raw, ":"); i >= 0 {
			raw = raw[:i] + "/" + raw[i+1:]
		}
	}
	// Strip host (the bit before the first /)
	if i := strings.Index(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	// Strip .git suffix
	raw = strings.TrimSuffix(raw, ".git")
	return raw
}
