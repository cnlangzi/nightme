package me

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// panickingDecider panics when the run asks it for a decision.
type panickingDecider struct{}

func (panickingDecider) Decide(context.Context, llm.DecisionRequest) (*llm.DecisionResponse, error) {
	panic("the decision model exploded")
}

// TestAPanicInTheRunFailsTheRunAndLeavesTheDaemonAlone — a bug in one
// run is that run's business: the run fails, the daemon goes on.
func TestAPanicInTheRunFailsTheRunAndLeavesTheDaemonAlone(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	svc, op := testService(t, dir, Config{Decider: panickingDecider{}, Logger: logger})
	op.steps = []fakeStep{{outcome: outcome.Outcome{
		Success:    false,
		Status:     http.StatusInternalServerError,
		StatusText: "the step failed",
	}}}

	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	body := recordEntry(t, op)
	if !strings.Contains(body, "■ [ one ] failed") {
		t.Errorf("the record does not name the phase of the run:\n%s", body)
	}
	if !strings.Contains(body, "reason: the run crashed: the decision model exploded") {
		t.Errorf("the record does not say why the run failed:\n%s", body)
	}

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("the panic was not logged as an error:\n%s", out)
	}
	if !strings.Contains(out, "me: a run panicked") {
		t.Errorf("the log does not name the panic:\n%s", out)
	}
	if !strings.Contains(out, "the decision model exploded") {
		t.Errorf("the log does not carry the panic:\n%s", out)
	}

	// The daemon goes on: the next run of the same service works.
	if _, _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-2", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("the service did not take the second run: %v", err)
	}
	waitRun(t, svc, "obs-2")
}
