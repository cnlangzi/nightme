package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// TriggerManager subscribes to all trigger sources, runs the
// 3-stage filter pipeline (receive → filter by workspaces →
// trigger), and invokes bot.onTrigger for each (workflow, event,
// workspace) match.
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

	// source is the git event source (v0: polling; future: webhook).
	// nil = cron-only mode.
	source gtw.EventSource
}

// newTriggerManager constructs a TriggerManager. cron and git
// subscription are wired in Start.
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

// setEventSource wires the git event source. Must be called
// before Start if git triggers are needed.
func (t *TriggerManager) setEventSource(s gtw.EventSource) {
	t.source = s
}

// Start launches the cron scheduler and the git event subscription.
// Both feed into a single onEvent callback.
func (t *TriggerManager) Start(ctx context.Context) error {
	t.mu.Lock()
	t.cron = cron.New()
	t.mu.Unlock()

	// 1. cron: register a tick for each workflow's schedule entries
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

	// 2. git events: single subscription via the configured source.
	if t.source != nil && anyNeedsGitEvents(t.workflows) {
		events, err := t.source.Subscribe(ctx)
		if err != nil {
			return fmt.Errorf("bot: subscribe git events: %w", err)
		}
		go t.consumeGitEvents(ctx, events)
		t.logger.Info("trigger: subscribed to git event source")
	} else {
		t.logger.Info("trigger: no git event source wired (cron-only)")
	}

	t.cron.Start()
	t.logger.Info("trigger: started", "cron_jobs", len(t.cron.Entries()))
	return nil
}

// consumeGitEvents reads from the git event source and dispatches
// each event to onEvent (which goes through the same 3-stage
// filter as cron events).
func (t *TriggerManager) consumeGitEvents(ctx context.Context, events <-chan gtw.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			// Translate gtw.Event → wfe.Event
			wfEv := translateEvent(ev)
			t.onEvent(ctx, wfEv, nil) // origin workflow nil for git events; we iterate all
		}
	}
}

// translateEvent converts a gtw.Event (from the polling source)
// to a wfe.Event (which bot's trigger pipeline consumes).
func translateEvent(ev gtw.Event) wfe.Event {
	data := map[string]any{
		"repo":  ev.Repo,
		"action": ev.Action,
	}
	if ev.PR > 0 {
		data["pr_number"] = ev.PR
	}
	if ev.Issue > 0 {
		data["issue_number"] = ev.Issue
	}
	if ev.Branch != "" {
		data["name"] = ev.Branch
	}
	if ev.Author != "" {
		data["author"] = ev.Author
	}
	if ev.Command != "" {
		data["command"] = ev.Command
	}
	if ev.CommentBody != "" {
		data["text"] = ev.CommentBody
	}
	if ev.URL != "" {
		data["url"] = ev.URL
	}
	data["source"] = ev.Kind

	return wfe.Event{
		Kind: ev.Kind,
		Time: ev.Time,
		Data: data,
	}
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

// onEvent is the single callback for both cron and git events.
// Stage 2: filter by workspace + wfe.Match.
func (t *TriggerManager) onEvent(ctx context.Context, ev wfe.Event, originWorkflow *wfe.Workflow) {
	// cron events: no event.repo; fire ALL workflows that match
	// this cron expression and have a schedule entry.
	if ev.Kind == "schedule" {
		for _, wf := range t.workflows {
			if !wfe.Match(wf, ev) {
				continue
			}
			// For cron, fire once per workspace in the workflow.
			for _, ws := range wf.Workspaces {
				t.onTrigger(ctx, wf, ev, ws)
			}
		}
		// originWorkflow is ignored for cron — we iterate all.
		_ = originWorkflow
		return
	}

	// git events: reverse-lookup repo → workspace, then match.
	repo, _ := ev.Data["repo"].(string)
	if repo == "" {
		t.logger.Warn("trigger: git event missing repo", "kind", ev.Kind)
		return
	}
	workspace, ok := t.wsMap.byRepo[repo]
	if !ok {
		t.logger.Warn("trigger: git event for unknown repo", "kind", ev.Kind, "repo", repo)
		return
	}
	for _, wf := range t.workflows {
		if !containsString(wf.Workspaces, workspace) {
			continue
		}
		if !wfe.Match(wf, ev) {
			continue
		}
		t.onTrigger(ctx, wf, ev, workspace)
	}
}

// (originWorkflow retained as a future knob for per-event workflow
// scoping; v0 always iterates all matching workflows.)
var _ = func(_ *wfe.Workflow) {} // keep the param in the signature

// anyNeedsGitEvents reports whether any workflow in the set has
// PR/branch/issue/mention triggers (and thus needs the git
// subscription).
func anyNeedsGitEvents(wfs []*wfe.Workflow) bool {
	for _, wf := range wfs {
		if wf.On.PullRequest != nil || wf.On.Branch != nil ||
			wf.On.Issue != nil || wf.On.Mention != nil {
			return true
		}
	}
	return false
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// workspaceRepoMap is built at bot startup by reading
// `git -C <workspace> remote get-url origin` for every workspace
// mentioned in any workflow. Used to map git events (which carry
// a repo URL) back to a workspace path.
type workspaceRepoMap struct {
	byRepo map[string]string // "cnlangzi/nightme" → "~/work/nightme"
	byPath map[string]string // "~/work/nightme" → "cnlangzi/nightme"
}

func buildWorkspaceRepoMap(wfs []*wfe.Workflow) (*workspaceRepoMap, error) {
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
			repo := gitOrigin(ws)
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
