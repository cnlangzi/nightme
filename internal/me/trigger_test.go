package me

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// fired is one trigger the manager produced.
type fired struct {
	workflow  *wfe.Workflow
	event     wfe.Event
	workspace string
}

// recorder collects the triggers a manager produces.
type recorder struct {
	fired []fired
}

func (r *recorder) onTrigger(_ context.Context, wf *wfe.Workflow, ev wfe.Event, workspace string) {
	r.fired = append(r.fired, fired{workflow: wf, event: ev, workspace: workspace})
}

// scheduledWorkflow is a workflow with two cron entries.
const scheduledWorkflow = `
name: nightly
workspaces: ['/tmp/nightly-workspace']
on:
  schedule:
    - cron: "0 3 * * *"
    - cron: "30 4 * * *"
jobs:
  main:
    steps:
      - id: only
        prompt: "hello"
`

// TestTriggerManager_RegistersCronEntries — one cron job per
// schedule entry.
func TestTriggerManager_RegistersCronEntries(t *testing.T) {
	wf, err := wfe.Parse([]byte(scheduledWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rec := &recorder{}
	tm := newTriggerManager([]*wfe.Workflow{wf}, &workspaceRepoMap{}, rec.onTrigger, slog.Default())
	if err := tm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := tm.Entries(); got != 2 {
		t.Errorf("cron entries = %d, want 2", got)
	}
	if err := tm.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := tm.Entries(); got != 0 {
		t.Errorf("cron entries after Stop = %d, want 0", got)
	}
}

// TestTriggerManager_OnEvent_FiresMatchingWorkflow — a schedule
// event fires the workflow that declares that cron expression, once
// per workspace.
func TestTriggerManager_OnEvent_FiresMatchingWorkflow(t *testing.T) {
	wf, err := wfe.Parse([]byte(scheduledWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rec := &recorder{}
	tm := newTriggerManager([]*wfe.Workflow{wf}, &workspaceRepoMap{}, rec.onTrigger, slog.Default())

	tm.onEvent(context.Background(), wfe.Event{
		Kind: "schedule",
		Time: time.Now(),
		Data: map[string]any{"cron": "0 3 * * *"},
	})

	if len(rec.fired) != 1 {
		t.Fatalf("fired = %d, want 1", len(rec.fired))
	}
	if rec.fired[0].workflow != wf {
		t.Error("fired the wrong workflow")
	}
	if rec.fired[0].workspace != wf.Workspaces[0] {
		t.Errorf("workspace = %q, want %q", rec.fired[0].workspace, wf.Workspaces[0])
	}
}

// TestTriggerManager_OnEvent_IgnoresOtherCron — an event for a cron
// expression no workflow declares fires nothing.
func TestTriggerManager_OnEvent_IgnoresOtherCron(t *testing.T) {
	wf, err := wfe.Parse([]byte(scheduledWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rec := &recorder{}
	tm := newTriggerManager([]*wfe.Workflow{wf}, &workspaceRepoMap{}, rec.onTrigger, slog.Default())

	tm.onEvent(context.Background(), wfe.Event{
		Kind: "schedule",
		Time: time.Now(),
		Data: map[string]any{"cron": "15 5 * * *"},
	})

	if len(rec.fired) != 0 {
		t.Errorf("fired = %d, want 0", len(rec.fired))
	}
}

// TestTriggerManager_OnEvent_DropsGitEvents — git events are not
// wired yet: they are dropped, not fired.
func TestTriggerManager_OnEvent_DropsGitEvents(t *testing.T) {
	wf, err := wfe.Parse([]byte(scheduledWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rec := &recorder{}
	tm := newTriggerManager([]*wfe.Workflow{wf}, &workspaceRepoMap{}, rec.onTrigger, slog.Default())

	tm.onEvent(context.Background(), wfe.Event{
		Kind: "pull_request",
		Time: time.Now(),
		Data: map[string]any{"branch": "main", "action": "opened"},
	})

	if len(rec.fired) != 0 {
		t.Errorf("fired = %d, want 0", len(rec.fired))
	}
}

// TestBuildWorkspaceRepoMap — the map holds every workspace of every
// workflow, keyed by the workspace path and by its canonical repo.
func TestBuildWorkspaceRepoMap(t *testing.T) {
	ws := gitRepo(t)
	wf, err := wfe.Parse([]byte(scheduledWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf.Workspaces = []string{ws}

	m := buildWorkspaceRepoMap(context.Background(), []*wfe.Workflow{wf}, slog.Default())
	if got := m.byPath[ws]; got != "foo/bar" {
		t.Errorf("byPath[%q] = %q, want %q", ws, got, "foo/bar")
	}
	if got := m.byRepo["foo/bar"]; got != ws {
		t.Errorf("byRepo[foo/bar] = %q, want %q", got, ws)
	}
}

// TestBuildWorkspaceRepoMap_SkipsWorkspaceWithoutOrigin — a
// workspace the map cannot resolve is skipped: the map serves git
// event triggers, which are not wired yet, so it must not stop the
// cron workflows from running.
func TestBuildWorkspaceRepoMap_SkipsWorkspaceWithoutOrigin(t *testing.T) {
	ws := gitInit(t)
	wf, err := wfe.Parse([]byte(scheduledWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf.Workspaces = []string{ws}

	m := buildWorkspaceRepoMap(context.Background(), []*wfe.Workflow{wf}, slog.Default())
	if len(m.byPath) != 0 {
		t.Errorf("byPath = %v, want empty", m.byPath)
	}
}

// badCronWorkflow carries a cron expression the parser rejects.
const badCronWorkflow = `
name: nightly
workspaces: ['/tmp/nightly-workspace']
on:
  schedule:
    - cron: "not a cron expression"
jobs:
  main:
    steps:
      - id: only
        prompt: "hello"
`

// TestTriggerManager_BadCronExpressionIsSkipped — a malformed cron
// expression registers nothing. The manager still starts, and the
// workflow that carries it never fires — with a log line saying so,
// instead of silence.
func TestTriggerManager_BadCronExpressionIsSkipped(t *testing.T) {
	wf, err := wfe.Parse([]byte(badCronWorkflow))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rec := &recorder{}
	tm := newTriggerManager([]*wfe.Workflow{wf}, &workspaceRepoMap{}, rec.onTrigger, slog.Default())

	if err := tm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = tm.Stop() }()

	if got := tm.Entries(); got != 0 {
		t.Errorf("cron entries = %d, want 0", got)
	}
}

// TestExpandWorkspace — a workspace named "~/work/repo" reaches the
// shell and git as an absolute path: neither expands "~".
func TestExpandWorkspace(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	tests := []struct {
		in, want string
	}{
		{"~", home},
		{"~/work/nightme", filepath.Join(home, "work", "nightme")},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := expandWorkspace(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
