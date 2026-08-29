package bot

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// TriggerManager subscribes to cron ticks and runs the workspace
// filter (3-stage pipeline: receive → filter → trigger), invoking
// bot.onTrigger for each (workflow, event, workspace) match.
//
// v0: cron-only. Git event support (PR / issue / branch / mention
// triggers) is planned but deferred — see commit history for
// details. When re-enabled, the EventSource abstraction will
// return to this file; until then, this struct holds no event
// source field.
//
// Locked design: bot's trigger manager is fully self-contained.
// It does NOT route through the gateway's dispatch chain; it
// produces events that bot.onTrigger processes. (The dispatch
// chain is for messages from external channels — feishu, etc.)
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
			cronExpr := s.Cron
			wf := wf // capture
			expr := cronExpr
			t.cron.AddFunc(cronExpr, func() {
				ev := wfe.Event{
					Kind: "schedule",
					Time: time.Now(),
					Data: map[string]any{"cron": expr},
				}
				t.onEvent(ctx, ev, wf)
			})
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

// onEvent is the callback fired by the cron scheduler for each tick.
// Stage 2: filter (cron events always match if any workflow has a
// matching schedule entry).
func (t *TriggerManager) onEvent(ctx context.Context, ev wfe.Event, originWorkflow *wfe.Workflow) {
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
		_ = originWorkflow
		return
	}
	// Other event kinds (pr / issue / branch / mention) are not
	// handled in v0 — bot is cron-only until git event support is
	// re-introduced. Log and drop so users see something happened.
	t.logger.Warn("trigger: dropping unhandled event (git events not yet wired)", "kind", ev.Kind)
}

// workspaceRepoMap maps a git remote URL (e.g. "cnlangzi/nightme")
// to the workspace path (e.g. "~/work/nightme"). Built at bot
// startup by reading `git -C <workspace> remote get-url origin`
// for every workspace mentioned in any workflow.
//
// In v0 (cron-only) this map is unused but kept constructed
// because it's cheap and lets us bring back git triggers without
// restructuring.
type workspaceRepoMap struct {
	byRepo map[string]string
	byPath map[string]string
}

func buildWorkspaceRepoMap(ctx context.Context, wfs []*wfe.Workflow) (*workspaceRepoMap, error) {
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
				return nil, &workspaceError{Workspace: ws, Reason: "no git remote origin"}
			}
			m.byRepo[repo] = ws
			m.byPath[ws] = repo
		}
	}
	return m, nil
}

// workspaceError is returned by buildWorkspaceRepoMap when a
// workspace's git origin can't be determined.
type workspaceError struct {
	Workspace string
	Reason    string
}

func (e *workspaceError) Error() string {
	return "bot: workspace " + e.Workspace + ": " + e.Reason
}
