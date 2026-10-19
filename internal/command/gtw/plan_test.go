package gtw

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/command"
)

func TestRenderPlanPrompt(t *testing.T) {
	for _, topic := range []string{"", "  \t "} {
		p := renderPlanPrompt(topic)
		if strings.Contains(p, placeholderTopicSection) {
			t.Errorf("topic %q: unsubstituted topic placeholder", topic)
		}
		if !strings.HasSuffix(p, "ask what the user wants to plan.") {
			t.Errorf("topic %q: prompt without a topic must end by asking for one", topic)
		}
	}

	p := renderPlanPrompt("  --retry policy for webhooks\n")
	if !strings.HasSuffix(p, "Topic to plan:\n--retry policy for webhooks") {
		t.Errorf("prompt must end with the trimmed topic; got tail %q", p[len(p)-80:])
	}
	if strings.HasPrefix(strings.TrimSpace(p), "/") {
		t.Error("prompt starts with '/'; agents would parse it as a slash command")
	}
	for _, want := range []string{
		"decision-complete design",
		"has NOT seen this conversation",
		"Do not change the project or its environment",
		"Do not run builds or tests",
		"Do not write implementation code",
		"Plan mode lasts until a later task states that it ends plan mode",
		"treat it as a request to plan that implementation",
		"If this task arrives again later",
		"never ask the user for them",
		`write one line starting with "Decision:"`,
		`"Decision (replaces <earlier decision>): ..."`,
		"It is not permission to implement",
		"tell the user to run /gtw new",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	for _, forbid := range []string{"nightme", "slash command", "intercept"} {
		if strings.Contains(strings.ToLower(p), forbid) {
			t.Errorf("prompt mentions %q; the agent only sees the conversation, not how it is relayed", forbid)
		}
	}
}

func TestNewIssuePromptEndsPlanMode(t *testing.T) {
	p := renderNewIssuePrompt("/tmp/drafts")
	for _, want := range []string{
		"If this conversation is in plan mode, this task ends it",
		`Lines starting with "Decision:" record settled points`,
		`A "Decision (replaces …)" line overrides the decision it names`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("new-issue prompt missing %q", want)
		}
	}
}

func (r *newIssueRig) handlePlan(t *testing.T, args ...string) {
	t.Helper()
	_, err := r.f.Handle(context.Background(), command.RuntimeServices{}, nil, r.cs, command.SlashInput{
		Args:      append([]string{"gtw", "plan"}, args...),
		ChatID:    r.cs.ChatID,
		MessageID: "msg-1",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestFactory_Plan_TopicIsNotParsedAsFlags(t *testing.T) {
	rig := newNewIssueRig(t, "chat-plan-1")

	rig.handlePlan(t, "--force", "-y", "retry", "policy")

	if got := rig.ch.lastText(); strings.HasPrefix(got, "❌") {
		t.Errorf("topic tokens starting with '-' must reach the agent, got reply %q", got)
	}
}

func TestFactory_Plan_NotInRepo(t *testing.T) {
	rig := newNewIssueRig(t, "chat-plan-2")
	if err := rig.cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	rig.handlePlan(t)

	if got := rig.ch.lastText(); !strings.Contains(got, "Not in a git repository") {
		t.Errorf("reply = %q", got)
	}
}

func TestFactory_Plan_TakesNoRunLock(t *testing.T) {
	rig := newNewIssueRig(t, "chat-plan-3")
	mu := rig.f.mgr.runLockFor(rig.cs.ChatID)
	mu.Lock()
	defer mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs, command.SlashInput{
			Args: []string{"gtw", "plan"}, ChatID: rig.cs.ChatID, MessageID: "msg-1",
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("/gtw plan blocked on the per-chat run lock")
	}
}
