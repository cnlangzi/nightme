package review

// What /review shows the chat: the plan line before the run, the
// coalesced stream of the groups' findings, and the verdict line at
// the end. The merged body is the main agent's input — it never
// reaches the channel.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestRenderPlanLine: the plan names the runner, the group count and
// every group with its file count — and nothing else.
func TestRenderPlanLine(t *testing.T) {
	got := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go", "b.go"}},
		{Pattern: "_nightme_simplify", Label: "simplify lens", Files: []string{"a.go"}},
	})

	for _, want := range []string{
		"codex",
		"2 groups",
		"1. **/*.go — 2 files",
		"2. simplify lens — 1 file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the plan line does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "findings") {
		t.Errorf("the plan line carries review content:\n%s", got)
	}
}

// TestRenderCountLine: the verdict counts the groups, the findings
// and their severities, names the groups that failed and points at
// the command that resumes them.
func TestRenderCountLine(t *testing.T) {
	reports := []agent.ReviewReport{
		{Parsed: true, Findings: []agent.ReviewFinding{
			{Severity: agent.SeverityCritical},
			{Severity: agent.SeverityHigh},
			{Severity: agent.SeverityHigh},
		}},
		{Parsed: true, Findings: []agent.ReviewFinding{
			{Severity: agent.SeverityLow},
		}},
	}

	got := renderCountLine(3, reports, nil)
	for _, want := range []string{
		"3 groups",
		"4 findings",
		"2 high",
		"1 critical",
		"1 low",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the verdict line does not carry %q: %s", want, got)
		}
	}
	// A bucket with no findings is not shown.
	if strings.Contains(got, "medium") {
		t.Errorf("the verdict line shows an empty severity bucket: %s", got)
	}

	failed := renderCountLine(3, reports, []string{"**/*.ts"})
	for _, want := range []string{"**/*.ts", "/review --continue"} {
		if !strings.Contains(failed, want) {
			t.Errorf("the verdict line does not carry %q: %s", want, failed)
		}
	}
}

// TestRenderCountLine_UnparsedGroups: a group whose output matched no
// known shape is reported as unparsed.
func TestRenderCountLine_UnparsedGroups(t *testing.T) {
	got := renderCountLine(2, []agent.ReviewReport{
		{Parsed: true},
		{Parsed: false},
	}, nil)
	if !strings.Contains(got, "1 unparsed group") {
		t.Errorf("the verdict line does not report the unparsed group: %s", got)
	}
}

// TestReviewVerdict_FailedGroupsAreNotUnparsed: a group that failed
// is counted as failed — never as unparsed.
func TestReviewVerdict_FailedGroupsAreNotUnparsed(t *testing.T) {
	outcomes := []reviewOutcome{
		{pattern: "**/*.go", report: agent.ReviewReport{Parsed: true}},
		{pattern: "**/*.ts", err: errors.New("boom")},
	}
	plan := []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "**/*.go"},
		{Pattern: "**/*.ts", Label: "**/*.ts"},
	}

	got := reviewVerdict(outcomes, plan)
	if !strings.Contains(got, "2 groups") {
		t.Errorf("the verdict does not count both groups: %s", got)
	}
	if !strings.Contains(got, "**/*.ts") || !strings.Contains(got, "/review --continue") {
		t.Errorf("the verdict does not name the failed group: %s", got)
	}
	if strings.Contains(got, "unparsed") {
		t.Errorf("the failed group was counted as unparsed: %s", got)
	}
}

// planReviewStarter is a fake starter that reports a plan and one
// outcome per group — the way the real fan-out does — and returns a
// merged text.
type planReviewStarter struct {
	name   string
	groups []agent.ReviewPlanGroup
	failed int
	merged string
}

func (s *planReviewStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "", nil, nil)
}
func (s *planReviewStarter) Detect() error { return nil }
func (s *planReviewStarter) Init(string)   {}
func (s *planReviewStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("planReviewStarter: Start not implemented")
}
func (s *planReviewStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("planReviewStarter: RunOnce not implemented")
}

func (s *planReviewStarter) Review(_ context.Context, _ agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	cfg := agent.ParseRunOnceOptions(opts)
	if cfg.OnReviewPlan != nil {
		cfg.OnReviewPlan(s.groups)
	}
	if cfg.OnReviewJob != nil {
		for i, g := range s.groups {
			var err error
			if i < s.failed {
				err = fmt.Errorf("group %d failed", i+1)
			}
			report := agent.ReviewReport{Parsed: true}
			if i >= s.failed {
				report.Findings = []agent.ReviewFinding{{Severity: agent.SeverityHigh}}
			}
			cfg.OnReviewJob(agent.ReviewJob{
				Pattern:   g.Pattern,
				Files:     g.Files,
				SessionID: fmt.Sprintf("sess-%d", i+1),
				Report:    report,
			}, err)
		}
	}
	return agent.RunResult{Text: s.merged}, nil
}

// outReplies returns the OutReply messages the emitter received — the
// review's own lines, in order.
func outReplies(em *recordingEmitter) []messages.OutboundMessage {
	var out []messages.OutboundMessage
	for _, msg := range em.messages() {
		if msg.Kind == messages.OutReply {
			out = append(out, msg)
		}
	}
	return out
}

// waitForOutReplies waits until the emitter has recorded n OutReply
// messages — the review's own lines.
func waitForOutReplies(t *testing.T, em *recordingEmitter, n int) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		replies := outReplies(em)
		if len(replies) >= n {
			return replies
		}
		if time.Now().After(deadline) {
			t.Fatalf("the emitter received %d lines, want %d: %+v", len(replies), n, replies)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForBlocks waits until the driver received n blocks.
func waitForBlocks(t *testing.T, d *recordingDriver, n int) []agent.ContentBlock {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		blocks := d.blocks()
		if len(blocks) >= n {
			return blocks
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent session received %d blocks, want %d", len(blocks), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHandle_ReviewDeliversThePlanAndTheVerdict: the channel sees
// the plan first and the verdict last, and never the merged body —
// the merged text goes to the main agent's session instead, with the
// instruction to answer with a verdict rather than restate it.
func TestHandle_ReviewDeliversThePlanAndTheVerdict(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-delivery-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go", "b.go"}},
			{Pattern: "_nightme_simplify", Label: "simplify lens", Files: []string{"a.go"}},
		},
		merged: "### Group: pattern **/*.go — files: a.go, b.go\n\nthe go findings\n\n" +
			"### Group: pattern _nightme_simplify — files: a.go\n\nthe simplify findings\n\n",
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-delivery-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	// The verdict line is the last thing the review sends.
	replies := waitForOutReplies(t, em, 2)
	if len(replies) < 2 {
		t.Fatalf("the review sent %d lines to the channel, want the plan and the verdict: %+v", len(replies), replies)
	}

	plan := replies[0]
	if !strings.Contains(plan.Text, starter.name) {
		t.Errorf("the first line does not name the runner: %q", plan.Text)
	}
	if !strings.Contains(plan.Text, "2 groups") {
		t.Errorf("the first line does not carry the group count: %q", plan.Text)
	}
	if !strings.Contains(plan.Text, "simplify lens") {
		t.Errorf("the first line does not list the groups: %q", plan.Text)
	}

	verdict := replies[len(replies)-1]
	if !strings.Contains(verdict.Text, "2 groups") {
		t.Errorf("the verdict does not report the group count: %q", verdict.Text)
	}
	if !strings.Contains(verdict.Text, "2 high") {
		t.Errorf("the verdict does not count the findings by severity: %q", verdict.Text)
	}

	// The merged body never reaches the channel.
	for _, msg := range replies {
		if strings.Contains(msg.Text, "### Group: pattern ") {
			t.Errorf("the merged body reached the channel: %q", msg.Text)
		}
	}

	// The main agent's session receives the merged text — in full —
	// plus the instruction to answer with a verdict.
	blocks := waitForBlocks(t, asDriver, 1)
	injected := blocks[0].Text
	if !strings.Contains(injected, "the go findings") || !strings.Contains(injected, "the simplify findings") {
		t.Errorf("the injected turn does not carry the merged text:\n%s", injected)
	}
	if !strings.Contains(injected, "verdict") {
		t.Errorf("the injected turn does not ask for a verdict:\n%s", injected)
	}
}

// TestHandle_ReviewNamesTheFailedGroup: a group that failed is named
// in the verdict, with the command that resumes it.
func TestHandle_ReviewNamesTheFailedGroup(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-failed-group-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
			{Pattern: "**/*.ts", Label: "**/*.ts", Files: []string{"c.ts"}},
		},
		failed: 1,
		merged: "### Group: pattern **/*.go — failed: boom\n\n" +
			"### Group: pattern **/*.ts — files: c.ts\n\nthe ts findings\n\n",
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-failed-group-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	em.waitForOutReply(t, "/review --continue")

	replies := waitForOutReplies(t, em, 2)
	verdict := replies[len(replies)-1]
	if !strings.Contains(verdict.Text, "**/*.go") {
		t.Errorf("the verdict does not name the failed group: %q", verdict.Text)
	}
	if !strings.Contains(verdict.Text, "2 groups") {
		t.Errorf("the verdict does not report the group count: %q", verdict.Text)
	}
}
