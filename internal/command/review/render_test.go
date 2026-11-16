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

// TestRenderPlanLine: the plan names the runner, the rule set, the
// subagent count and every group with its file count — and nothing
// else.
func TestRenderPlanLine(t *testing.T) {
	got := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "system / **/*.go · Go", Files: []string{"a.go", "b.go"}},
		{Pattern: agent.PatternSimplify, Label: "Simplify review lens · Go", Files: []string{"a.go"}},
	})

	for _, want := range []string{
		"codex",
		"open-code-review",
		"2 subagents",
		"1. system / **/*.go · Go — 2 files",
		"2. Simplify review lens · Go — 1 file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the plan line does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "findings") {
		t.Errorf("the plan line carries review content:\n%s", got)
	}
}

// TestRenderPlanLine_NamesTheRuleSet: the first line names the rule
// set the review ran against — nightme's own rubric when the plan
// holds the built-in group, ocr's rules otherwise.
func TestRenderPlanLine_NamesTheRuleSet(t *testing.T) {
	builtin := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: agent.PatternBuiltin, Label: "nightme code reviewer · Go", Files: []string{"a.go"}},
	})
	if !strings.Contains(builtin, "nightme code reviewer") {
		t.Errorf("the plan line does not name nightme's own rule set:\n%s", builtin)
	}

	ocr := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "system / **/*.go · Go", Files: []string{"a.go"}},
	})
	if !strings.Contains(ocr, "open-code-review") {
		t.Errorf("the plan line does not name ocr's rule set:\n%s", ocr)
	}
}

// TestRenderPlanLine_OneSubagent: a review of one group says "1
// subagent", not "1 subagents".
func TestRenderPlanLine_OneSubagent(t *testing.T) {
	got := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: agent.PatternBuiltin, Label: "nightme code reviewer · Go", Files: []string{"a.go"}},
	})
	if !strings.Contains(got, "1 subagent") {
		t.Errorf("the plan line does not count the single subagent:\n%s", got)
	}
	if strings.Contains(got, "1 subagents") {
		t.Errorf("the plan line pluralized the single subagent:\n%s", got)
	}
}

// TestVerdictLine: the verdict counts the subagents, the findings
// and their severities, names the groups that failed and points at
// the command that resumes them.
func TestVerdictLine(t *testing.T) {
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

	got := newVerdict(3, reports, nil).line()
	for _, want := range []string{
		"3 subagents",
		"4 findings",
		"2 high",
		"1 critical",
		"1 low",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the verdict line does not carry %q: %s", want, got)
		}
	}
	if strings.Contains(got, "group") {
		t.Errorf("the verdict line counts groups instead of subagents: %s", got)
	}
	// A bucket with no findings is not shown.
	if strings.Contains(got, "medium") {
		t.Errorf("the verdict line shows an empty severity bucket: %s", got)
	}

	failed := newVerdict(3, reports, []string{"**/*.ts"}).line()
	for _, want := range []string{"**/*.ts", "/review --continue"} {
		if !strings.Contains(failed, want) {
			t.Errorf("the verdict line does not carry %q: %s", want, failed)
		}
	}
}

// TestVerdictLine_UnparsedGroups: a group whose output matched no
// known shape is reported as unparsed.
func TestVerdictLine_UnparsedGroups(t *testing.T) {
	got := newVerdict(2, []agent.ReviewReport{
		{Parsed: true},
		{Parsed: false},
	}, nil).line()
	if !strings.Contains(got, "1 unparsed subagent") {
		t.Errorf("the verdict line does not report the unparsed subagent: %s", got)
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
	if !strings.Contains(got, "2 subagents") {
		t.Errorf("the verdict does not count both subagents: %s", got)
	}
	if !strings.Contains(got, "**/*.ts") || !strings.Contains(got, "/review --continue") {
		t.Errorf("the verdict does not name the failed group: %s", got)
	}
	if strings.Contains(got, "unparsed") {
		t.Errorf("the failed group was counted as unparsed: %s", got)
	}
}

// TestReviewVerdict_NamesTheFailedGroupByItsIdentity: two ocr rules
// over the same glob are two groups — the verdict names the one that
// failed, not the other.
func TestReviewVerdict_NamesTheFailedGroupByItsIdentity(t *testing.T) {
	outcomes := []reviewOutcome{
		{pattern: "**/*.go", source: "security", err: errors.New("boom")},
		{pattern: "**/*.go", source: "performance", report: agent.ReviewReport{Parsed: true}},
	}
	plan := []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Source: "security", Label: "security / **/*.go · Go"},
		{Pattern: "**/*.go", Source: "performance", Label: "performance / **/*.go · Go"},
	}

	got := reviewVerdict(outcomes, plan)
	if !strings.Contains(got, "security / **/*.go") {
		t.Errorf("the verdict does not name the failed group: %s", got)
	}
	if strings.Contains(got, "performance") {
		t.Errorf("the verdict names the group that did not fail: %s", got)
	}
}

// TestReviewVerdict_FallsBackToTheGroupsName: a group the plan did
// not name is still named by its identity — the source travels with
// the pattern.
func TestReviewVerdict_FallsBackToTheGroupsName(t *testing.T) {
	outcomes := []reviewOutcome{
		{pattern: "**/*.go", source: "system", err: errors.New("boom")},
	}

	got := reviewVerdict(outcomes, nil)
	if !strings.Contains(got, "system / **/*.go") {
		t.Errorf("the verdict does not name the failed group: %s", got)
	}
}

// TestReviewVerdictHint: the thread's end hint carries the same
// numbers as the card's verdict, in Markdown.
func TestReviewVerdictHint(t *testing.T) {
	outcomes := []reviewOutcome{
		{pattern: "**/*.go", report: agent.ReviewReport{Parsed: true, Findings: []agent.ReviewFinding{
			{Severity: agent.SeverityHigh},
		}}},
	}
	plan := []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "**/*.go"},
	}

	got := reviewVerdictHint(outcomes, plan)
	for _, want := range []string{"**review done**", "1 subagent", "1 finding", "1 high"} {
		if !strings.Contains(got, want) {
			t.Errorf("the end hint does not carry %q: %s", want, got)
		}
	}
}

// planReviewStarter is a fake starter that reports a plan and one
// outcome per group — the way the real fan-out does — and returns a
// merged text.
//
// reply, when set, is the text every group produces: the fake emits
// it through the review's sink, so the findings reach the chat the way
// a real group's do.
type planReviewStarter struct {
	name   string
	groups []agent.ReviewPlanGroup
	failed int
	merged string
	reply  string
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
	if cfg.OnEvent != nil && s.reply != "" {
		for i := range s.groups {
			cfg.OnEvent(agent.AgentEvent{
				Kind:      agent.EventAgentReady,
				SessionID: fmt.Sprintf("sess-%d", i+1),
				Model:     "model-x",
			})
			// The aggregator hands a group's findings over as one
			// thinking event carrying the group's number.
			cfg.OnEvent(agent.AgentEvent{
				Kind: agent.EventAgentThinking,
				Text: fmt.Sprintf("[%d] %s of group %d", i+1, s.reply, i+1),
			})
			cfg.OnEvent(agent.AgentEvent{
				Kind:   agent.EventAgentResult,
				Result: &agent.AgentResultEvent{},
			})
		}
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

// waitForMessages waits until the emitter has recorded n messages of
// any kind — the review's card lines and the thread's messages.
func waitForMessages(t *testing.T, em *recordingEmitter, n int) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs := em.messages()
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the emitter received %d messages, want %d: %+v", len(msgs), n, msgs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForReviewMessages waits until the emitter has recorded n of the
// review's own messages — its card lines and the thread's messages,
// without the session's init and heartbeat traffic.
func waitForReviewMessages(t *testing.T, em *recordingEmitter, n int) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var msgs []messages.OutboundMessage
		for _, msg := range em.messages() {
			if msg.Kind != messages.OutReply && msg.Kind != messages.OutThinking {
				continue
			}
			msgs = append(msgs, msg)
		}
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the emitter received %d review messages, want %d: %+v", len(msgs), n, msgs)
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
	if !strings.Contains(plan.Text, "2 subagents") {
		t.Errorf("the first line does not carry the subagent count: %q", plan.Text)
	}
	if !strings.Contains(plan.Text, "simplify lens") {
		t.Errorf("the first line does not list the groups: %q", plan.Text)
	}

	verdict := replies[len(replies)-1]
	if !strings.Contains(verdict.Text, "2 subagents") {
		t.Errorf("the verdict does not report the subagent count: %q", verdict.Text)
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

// TestHandle_ReviewDeliversTheFindingsToTheThread: the card carries
// the plan and the verdict — and nothing else. The findings reach the
// chat in the thread, one reply per group, each starting with the
// group's number, and the review closes with its verdict.
//
// The fake starter emits the events the aggregator produces; the
// aggregator's own contract is covered by the agent package's tests.
func TestHandle_ReviewDeliversTheFindingsToTheThread(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-thread-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "system / **/*.go · Go", Files: []string{"a.go", "b.go"}},
			{Pattern: agent.PatternSimplify, Label: "Simplify review lens · Go", Files: []string{"a.go"}},
		},
		reply: "the findings",
		merged: "### Group: pattern **/*.go — files: a.go, b.go\n\nthe go findings\n\n" +
			"### Group: pattern _nightme_simplify — files: a.go\n\nthe simplify findings\n\n",
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-thread-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)
	enableThink(t, cs)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	msgs := waitForReviewMessages(t, em, 4)

	// The card carries the plan and the verdict — and no group text.
	var replies []string
	var thinking []string
	for _, msg := range msgs {
		switch msg.Kind {
		case messages.OutReply:
			replies = append(replies, msg.Text)
		case messages.OutThinking:
			thinking = append(thinking, msg.Text)
		}
	}
	if len(replies) != 2 {
		t.Fatalf("the card carries %d lines, want the plan and the verdict: %+v", len(replies), replies)
	}
	if !strings.Contains(replies[0], "🔍 review") || !strings.Contains(replies[0], "2 subagents") {
		t.Errorf("the first line is not the plan: %q", replies[0])
	}
	if !strings.Contains(replies[0], "1. system / **/*.go · Go — 2 files") {
		t.Errorf("the first line does not list the groups: %q", replies[0])
	}
	if !strings.Contains(replies[1], "2 subagents") || !strings.Contains(replies[1], "2 high") {
		t.Errorf("the second line is not the verdict: %q", replies[1])
	}
	for _, reply := range replies {
		if strings.Contains(reply, "the findings") {
			t.Errorf("a group's findings reached the card: %q", reply)
		}
	}

	// The thread carries the findings — one reply per group, each
	// starting with the group's number — and the review's end hint.
	if len(thinking) != 3 {
		t.Fatalf("the thread carries %d messages, want 2 findings and the end hint: %+v", len(thinking), thinking)
	}
	if !strings.Contains(thinking[0], "[1] the findings of group 1") {
		t.Errorf("the first group's findings do not carry the group's number: %q", thinking[0])
	}
	if !strings.Contains(thinking[1], "[2] the findings of group 2") {
		t.Errorf("the second group's findings do not carry the group's number: %q", thinking[1])
	}
	if !strings.Contains(thinking[2], "review done") {
		t.Errorf("the thread does not close on the verdict: %q", thinking[2])
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
	if !strings.Contains(verdict.Text, "2 subagents") {
		t.Errorf("the verdict does not report the subagent count: %q", verdict.Text)
	}
}

// TestVerdictLine_UnparsedGroupsAreNotGreen: a verdict whose
// count is incomplete (a group's output could not be parsed) must not
// read as a clean "nothing found".
func TestVerdictLine_UnparsedGroupsAreNotGreen(t *testing.T) {
	got := newVerdict(2, []agent.ReviewReport{{Parsed: false}}, nil).line()
	if !strings.Contains(got, "⚠️") {
		t.Errorf("the verdict of an unparsed run is not marked: %s", got)
	}
	if !strings.Contains(got, "1 unparsed subagent") {
		t.Errorf("the verdict does not report the unparsed subagent: %s", got)
	}
}
