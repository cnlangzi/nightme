package review

// The delivery of a review: the findings of every group, handed to
// the chat's agent once, and the card that indexes them.
//
// A review is not over when its fan-out ends — a group that failed
// can still be picked up by /review --continue. The findings wait in
// the chat's record until every group is in, and then the review is
// delivered: one document for the agent, one table for the card.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/agentsession/agentstest"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// reviewFanoutStarter runs the review's groups on the session path: the
// review reports its groups through the observer, and a continuation
// can pick the failed groups up — the starter serves sessions too.
type reviewFanoutStarter struct {
	name string
	// groups is the plan the review reports.
	groups []agent.ReviewPlanGroup
	// failed is how many of the groups report an error.
	failed int
	// reply is the text a resumed group answers with.
	reply string
	// failAgain makes a resumed session fail instead of answering.
	failAgain bool
	// suffix is appended to every group's output — the text of the
	// round that produced it.
	suffix string

	mu      sync.Mutex
	started []agent.StartConfig
}

func (s *reviewFanoutStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "", nil, nil)
}
func (s *reviewFanoutStarter) Detect() error { return nil }
func (s *reviewFanoutStarter) Init(string)   {}

func (s *reviewFanoutStarter) Start(_ context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	s.mu.Lock()
	s.started = append(s.started, cfg)
	s.mu.Unlock()
	d := &fanoutDriver{starter: s, events: make(chan agent.AgentEvent, 8)}
	return agent.NewAgent(s.Info(), 0, d.events, d), nil
}

func (s *reviewFanoutStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("reviewFanoutStarter: RunOnce must not be used on the session path")
}

func (s *reviewFanoutStarter) Review(_ context.Context, _ agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	cfg := agent.ParseRunOnceOptions(opts)
	if cfg.OnReviewPlan != nil {
		cfg.OnReviewPlan(s.groups)
	}
	sections := make([]agent.ReviewGroupSection, 0, len(s.groups))
	for i, g := range s.groups {
		section := agent.ReviewGroupSection{Pattern: g.Pattern, Files: g.Files}
		if i < s.failed {
			section.Err = fmt.Errorf("group %d failed", i+1)
		} else {
			section.Texts = []string{s.groupText(i + 1)}
		}
		sections = append(sections, section)
	}
	if cfg.OnReviewJob != nil {
		for i, g := range s.groups {
			var err error
			report := agent.ReviewReport{Parsed: true}
			if i < s.failed {
				err = fmt.Errorf("group %d failed", i+1)
				report = agent.ReviewReport{}
			} else {
				report.Findings = []agent.ReviewFinding{{
					Path:     fmt.Sprintf("file%d.go", i+1),
					Line:     fmt.Sprintf("%d", i+1),
					Severity: agent.SeverityHigh,
					Content:  fmt.Sprintf("issue %d in group %d", i+1, i+1),
				}}
			}
			cfg.OnReviewJob(agent.ReviewJob{
				Pattern:   g.Pattern,
				Source:    g.Source,
				Files:     g.Files,
				SessionID: fmt.Sprintf("sess-%d", i+1),
				Report:    report,
				Text:      s.groupText(i + 1),
			}, err)
		}
	}
	return agent.RunResult{Text: agent.MergeReviewDocument(sections)}, nil
}

// groupText is the output of the nth group: what the group said,
// with the round it belongs to.
func (s *reviewFanoutStarter) groupText(n int) string {
	text := fmt.Sprintf("the findings of group %d", n)
	if s.suffix == "" {
		return text
	}
	return text + " " + s.suffix
}

// startedConfigs returns the StartConfig each Start call received.
func (s *reviewFanoutStarter) startedConfigs() []agent.StartConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.StartConfig(nil), s.started...)
}

// fanoutDriver answers a session: the group's own text, or a failure.
type fanoutDriver struct {
	starter *reviewFanoutStarter
	events  chan agent.AgentEvent
}

func (d *fanoutDriver) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	go func() {
		d.events <- agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			SessionID: "sess-new",
			Model:     "model-x",
		}
		if d.starter.failAgain {
			d.events <- agent.AgentEvent{
				Kind: agent.EventAgentError,
				Err:  errors.New("the resumed session failed"),
			}
			return
		}
		d.events <- agent.AgentEvent{
			Kind:   agent.EventAgentResult,
			Result: &agent.AgentResultEvent{Text: d.starter.reply},
		}
		close(d.events)
	}()
	return nil
}

func (d *fanoutDriver) SendPermission(string) error { return nil }
func (d *fanoutDriver) Reset(context.Context) error { return nil }
func (d *fanoutDriver) Close() error                { return nil }
func (d *fanoutDriver) Stop(context.Context) error  { return agent.ErrNotSupported }
func (d *fanoutDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// TestHandle_ReviewWithAFailedGroupDeliversNothing: a group that
// failed holds the review back — the card says what is missing, and
// nothing reaches the agent.
func TestHandle_ReviewWithAFailedGroupDeliversNothing(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewFanoutStarter{
		name: "review-pending-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
			{Pattern: "**/*.ts", Label: "**/*.ts", Files: []string{"c.ts"}},
		},
		failed: 1,
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-pending-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	verdict := em.waitForOutReply(t, "/review --continue")
	if !strings.Contains(verdict.Text, "1 of 2 subagents done") {
		t.Errorf("the verdict does not count the unfinished review:\\n%s", verdict.Text)
	}
	if !strings.Contains(verdict.Text, "1 finding so far") {
		t.Errorf("the verdict does not count the findings so far:\\n%s", verdict.Text)
	}
	if strings.Contains(verdict.Text, "| path |") {
		t.Errorf("the pending card carries a findings table:\\n%s", verdict.Text)
	}

	// Nothing reached the agent: the review is not over yet.
	time.Sleep(100 * time.Millisecond)
	if blocks := asDriver.blocks(); len(blocks) != 0 {
		t.Errorf("the pending review injected %d blocks, want none:\\n%s", len(blocks), blocks[0].Text)
	}
}

// TestHandle_ReviewAndContinueDeliverTheFindingsOnce: the review and
// the continuation that finishes it are delivered together — one
// document for the agent, one table for the card.
func TestHandle_ReviewAndContinueDeliverTheFindingsOnce(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewFanoutStarter{
		name: "review-two-round-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
			{Pattern: "**/*.ts", Label: "**/*.ts", Files: []string{"c.ts"}},
		},
		failed: 1,
		reply:  "- **high**: c.ts:7 — the resumed finding",
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-two-round-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")
	em.waitForOutReply(t, "/review --continue")
	waitUntilIdle(t, f, chatID)

	// The continuation picks the failed group up.
	dispatchContinue(t, f, cs, chatID, "review", "-c")
	waitUntilIdle(t, f, chatID)

	// The agent got the whole review in one turn: the group's own
	// output and the resumed group's.
	injected := waitForBlocks(t, asDriver, 1)
	if len(injected) != 1 {
		t.Fatalf("the agent received %d blocks, want 1", len(injected))
	}
	text := injected[0].Text
	for _, want := range []string{
		"Reviewed 2 groups.",
		"### Group: pattern **/*.go — files: a.go",
		"the resumed finding",
		"### Group: pattern **/*.ts — files: c.ts",
		"the findings of group 2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the injected document does not carry %q:\\n%s", want, text)
		}
	}
	if strings.Contains(text, "failed:") {
		t.Errorf("the group that came back is still marked as failed:\\n%s", text)
	}

	// The card carries one table with the findings of both rounds.
	verdict := em.waitForOutReply(t, "**high** · 2")
	for _, want := range []string{"| file2.go:2 |", "| c.ts:7 |"} {
		if !strings.Contains(verdict.Text, want) {
			t.Errorf("the table does not carry %q:\\n%s", want, verdict.Text)
		}
	}
}

// TestHandle_DeliveredReviewHasNothingToContinue: the review is
// delivered — a later `--continue` says so, and the findings are gone
// from the record.
func TestHandle_DeliveredReviewHasNothingToContinue(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewFanoutStarter{
		name: "review-delivered-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
		},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-delivered-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")
	// The findings reach the agent: the record holds the delivered
	// state by then.
	waitForBlocks(t, asDriver, 1)

	rec, ok := f.lookup(chatID)
	if !ok {
		t.Fatal("the review kept no record")
	}
	if len(rec.findings) != 0 {
		t.Errorf("the record still holds %d findings, want none", len(rec.findings))
	}
	if !rec.delivered {
		t.Error("the record does not know that its findings were delivered")
	}

	out := dispatchContinue(t, f, cs, chatID, "review", "-c")
	if !strings.Contains(out.Reply, "the last review completed") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
}

// TestHandle_ReviewDeliversTheDocumentTheFanOutProduced: the document
// the agent receives is the one the fan-out produced — the delivery
// rebuilds it from the record, and the two must not differ.
func TestHandle_ReviewDeliversTheDocumentTheFanOutProduced(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewFanoutStarter{
		name: "review-document-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go", "b.go"}},
			{Pattern: agent.PatternSimplify, Label: "simplify lens", Files: []string{"a.go"}},
		},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-document-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	injected := waitForBlocks(t, asDriver, 1)[0].Text
	want := agent.MergeReviewDocument([]agent.ReviewGroupSection{
		{Pattern: "**/*.go", Files: []string{"a.go", "b.go"}, Texts: []string{"the findings of group 1"}},
		{Pattern: agent.PatternSimplify, Files: []string{"a.go"}, Texts: []string{"the findings of group 2"}},
	})
	if !strings.Contains(injected, want) {
		t.Errorf("the injected document is not the one the fan-out produced:\n%s\nwant:\n%s", injected, want)
	}
}

// TestHandle_DeliveryMarksTheOutputAsDelivered: the output the
// delivery handed the agent does not go into the next delivery — the
// agent already has it.
func TestHandle_DeliveryMarksTheOutputAsDelivered(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewFanoutStarter{
		name:   "review-rounds-agent",
		groups: []agent.ReviewPlanGroup{{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}}},
		suffix: "of the first round",
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-rounds-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")
	first := waitForBlocks(t, asDriver, 1)[0].Text
	if !strings.Contains(first, "of the first round") {
		t.Fatalf("the first delivery does not carry the first round's output:\n%s", first)
	}

	rec, ok := f.lookup(chatID)
	if !ok {
		t.Fatal("the review kept no record")
	}
	if len(rec.contributions) != 1 {
		t.Fatalf("the record holds %d contributions, want 1", len(rec.contributions))
	}
	if !rec.contributions[0].delivered {
		t.Error("the output the agent received is not marked as delivered")
	}
	if doc := rec.mergedDocument(); strings.Contains(doc, "of the first round") {
		t.Errorf("the delivered output is in the next document again:\n%s", doc)
	}
}

// failingDriver refuses every prompt — the injection into the chat's
// agent fails.
type failingDriver struct{}

func (d *failingDriver) SendBlocks(context.Context, []agent.ContentBlock) error {
	return errors.New("failingDriver: the prompt was refused")
}
func (d *failingDriver) SendPermission(string) error { return nil }
func (d *failingDriver) Reset(context.Context) error { return nil }
func (d *failingDriver) Close() error                { return nil }
func (d *failingDriver) Stop(context.Context) error  { return agent.ErrNotSupported }
func (d *failingDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// TestHandle_FailedInjectionReportsNotDelivered: the findings that
// did not reach the agent are not reported as delivered — and they
// stay in the record, so the next review command hands them over.
func TestHandle_FailedInjectionReportsNotDelivered(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewFanoutStarter{
		name:   "review-inject-fail-agent",
		groups: []agent.ReviewPlanGroup{{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}}},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-inject-fail-chat"
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate(chatID, starter.name)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithEmitter(em)
	as := chatsession.NewAgentSession("as-"+chatID, cs.ID, starter.name, t.TempDir(), nil)
	agentstest.SetHandle(as, liveAgent(starter.name, &failingDriver{}))
	agentstest.SetStatus(as, agentsession.StatusRunning)
	cs.AttachAgentSessionForTest(as)
	cs.SelectedAgentSessionForTest(as)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")

	got := waitOutcome(t, em, 5*time.Second)
	if got.Data["delivered"] != "false" {
		t.Errorf("the outcome says delivered=%q, want false", got.Data["delivered"])
	}
	if got.Data["reason"] != "inject_failed" {
		t.Errorf("the outcome reason = %q, want inject_failed", got.Data["reason"])
	}

	// The findings stay in the record: the next review command hands
	// them over.
	rec, ok := f.lookup(chatID)
	if !ok {
		t.Fatal("the review kept no record")
	}
	if len(rec.findings) != 1 {
		t.Errorf("the record holds %d findings, want the one that was not delivered", len(rec.findings))
	}
	if rec.delivered {
		t.Error("the record says the findings were delivered")
	}
}
