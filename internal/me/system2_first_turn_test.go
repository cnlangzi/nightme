package me

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestTheFirstTurnOfTheChatModelHasAUserMessage — a chat model with
// nothing but a system prompt has no conversation to answer: the API
// refuses the request with "chat content is empty". The run opens the
// conversation itself.
func TestTheFirstTurnOfTheChatModelHasAUserMessage(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "done", "summary": "done"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "the step did not work"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the step did not work",
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() == 0 {
		t.Fatal("the chat model was never called")
	}
	req := provider.Request(0)
	if len(req.Messages) == 0 {
		t.Fatal("the first request carries no conversation at all")
	}
	if req.Messages[0].Role != llm.RoleUser {
		t.Fatalf("the first message is a %q, want a user turn", req.Messages[0].Role)
	}
	var text string
	for _, p := range req.Messages[0].Parts {
		text += p.Text
	}
	if !strings.Contains(text, "The run needs you") {
		t.Errorf("the opening turn is %q, want it to say what the run is up against", text)
	}
}
