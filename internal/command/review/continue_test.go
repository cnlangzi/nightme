package review

// Tests for `/review --continue`: the resume state the factory holds,
// and the continuation path itself.
//
// The continuation runs on the agent's session path, so the test
// doubles here are session-shaped: a Starter whose Start returns a
// live *agent.Agent (the recording driver), and a chat session whose
// selected AS is live (the inject target).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/agentsession/agentstest"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/timeouts"
)

// ─── test doubles ──────────────────────────────────────────────────

// recordingDriver is an agent driver that records the blocks it
// receives. It stands in for the bridge behind the chat session's
// agent session (the inject target).
type recordingDriver struct {
	mu   sync.Mutex
	sent []agent.ContentBlock
}

func (d *recordingDriver) SendBlocks(_ context.Context, blocks []agent.ContentBlock) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, blocks...)
	return nil
}

func (d *recordingDriver) SendPermission(string) error { return nil }
func (d *recordingDriver) Reset(context.Context) error { return nil }
func (d *recordingDriver) Close() error                { return nil }
func (d *recordingDriver) Stop(context.Context) error  { return agent.ErrNotSupported }
func (d *recordingDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// blocks returns the blocks the driver received.
func (d *recordingDriver) blocks() []agent.ContentBlock {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]agent.ContentBlock(nil), d.sent...)
}

// liveAgent wraps a recordingDriver in a live *agent.Agent.
func liveAgent(name string, d *recordingDriver) *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo(name, agent.ModeACP, "fake", nil, nil),
		0, make(chan agent.AgentEvent, 8), d)
}

// reviewSessionStarter serves the session path: Start returns a live
// *agent.Agent whose driver records the blocks the session received
// and replays a scripted reply.
type reviewSessionStarter struct {
	name  string
	reply string

	// block, when non-nil, holds the review until the test closes
	// it — so a review can be kept "running" while the test
	// asserts on the concurrent command.
	block chan struct{}

	// fail makes the resumed session's turn fail instead of
	// answering.
	fail bool

	// reviewErr is the error the review path fails with.
	reviewErr error

	mu       sync.Mutex
	started  []agent.StartConfig
	received []agent.ContentBlock
	deadline time.Time
}

func (s *reviewSessionStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "", nil, nil)
}
func (s *reviewSessionStarter) Detect() error { return nil }
func (s *reviewSessionStarter) Init(string)   {}

func (s *reviewSessionStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("reviewSessionStarter: RunOnce must not be used on the session path")
}

func (s *reviewSessionStarter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	s.mu.Lock()
	s.started = append(s.started, cfg)
	if deadline, ok := ctx.Deadline(); ok {
		s.deadline = deadline
	}
	s.mu.Unlock()
	d := &sessionReplyDriver{starter: s, events: make(chan agent.AgentEvent, 8)}
	return agent.NewAgent(s.Info(), 0, d.events, d), nil
}

func (s *reviewSessionStarter) Review(ctx context.Context, _ agent.StartConfig, _ ...agent.RunOnceOption) (agent.RunResult, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return agent.RunResult{}, ctx.Err()
		}
	}
	if s.reviewErr != nil {
		return agent.RunResult{}, s.reviewErr
	}
	return agent.RunResult{Text: "reviewed"}, nil
}

// startedConfigs returns the StartConfig each Start call received.
func (s *reviewSessionStarter) startedConfigs() []agent.StartConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.StartConfig(nil), s.started...)
}

// receivedBlocks returns the blocks the sessions received.
func (s *reviewSessionStarter) receivedBlocks() []agent.ContentBlock {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.ContentBlock(nil), s.received...)
}

// startDeadline returns the deadline of the context the last Start
// received.
func (s *reviewSessionStarter) startDeadline() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadline
}

// sessionReplyDriver replays a Ready + Result onto the agent's event
// channel once the prompt has been sent.
type sessionReplyDriver struct {
	starter *reviewSessionStarter
	events  chan agent.AgentEvent
}

func (d *sessionReplyDriver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	d.starter.mu.Lock()
	d.starter.received = append(d.starter.received, blocks...)
	sessionID := "sess-new"
	d.starter.mu.Unlock()

	go func() {
		if blk := d.starter.block; blk != nil {
			select {
			case <-blk:
			case <-ctx.Done():
				return
			}
		}
		d.events <- agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			SessionID: sessionID,
			Model:     "model-x",
		}
		if d.starter.fail {
			d.events <- agent.AgentEvent{
				Kind: agent.EventAgentError,
				Err:  errors.New("resumed session failed"),
			}
		} else {
			d.events <- agent.AgentEvent{
				Kind:   agent.EventAgentResult,
				Result: &agent.AgentResultEvent{Text: d.starter.reply},
			}
		}
		close(d.events)
	}()
	return nil
}

func (d *sessionReplyDriver) SendPermission(string) error { return nil }
func (d *sessionReplyDriver) Reset(context.Context) error { return nil }
func (d *sessionReplyDriver) Close() error                { return nil }
func (d *sessionReplyDriver) Stop(context.Context) error  { return agent.ErrNotSupported }
func (d *sessionReplyDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// recordingEmitter records the outbound messages the command sends.
type recordingEmitter struct {
	mu   sync.Mutex
	msgs []messages.OutboundMessage
	sent chan messages.OutboundMessage
}

func newRecordingEmitter() *recordingEmitter {
	return &recordingEmitter{sent: make(chan messages.OutboundMessage, 64)}
}

func (e *recordingEmitter) Send(_ context.Context, msg messages.OutboundMessage) error {
	e.mu.Lock()
	e.msgs = append(e.msgs, msg)
	e.mu.Unlock()
	select {
	case e.sent <- msg:
	default:
	}
	return nil
}

// messages returns every outbound message the emitter received.
func (e *recordingEmitter) messages() []messages.OutboundMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]messages.OutboundMessage(nil), e.msgs...)
}

// waitForOutReply waits for an OutReply carrying want.
func (e *recordingEmitter) waitForOutReply(t *testing.T, want string) messages.OutboundMessage {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-e.sent:
			if msg.Kind == messages.OutReply && strings.Contains(msg.Text, want) {
				return msg
			}
		case <-deadline:
			t.Fatalf("no OutReply carrying %q; got %+v", want, e.messages())
			return messages.OutboundMessage{}
		}
	}
}

// newContinueChatSession builds a chat session whose selected agent
// session is live (so findings can be injected into it) and whose
// emitter records the outbound messages. It returns the AS's
// recording driver so the test can inspect the injected blocks.
func newContinueChatSession(t *testing.T, chatID, agentName, dir string, em messages.Emitter) (*chatsession.ChatSession, *recordingDriver) {
	t.Helper()
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate(chatID, agentName)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithEmitter(em)

	asDriver := &recordingDriver{}
	as := chatsession.NewAgentSession("as-"+chatID, cs.ID, agentName, dir, nil)
	agentstest.SetHandle(as, liveAgent(agentName, asDriver))
	agentstest.SetStatus(as, agentsession.StatusRunning)
	cs.AttachAgentSessionForTest(as)
	cs.SelectedAgentSessionForTest(as)
	return cs, asDriver
}

// dispatchContinue invokes the factory's Handle for the given chat and
// returns the reply.
func dispatchContinue(t *testing.T, f *Factory, cs *chatsession.ChatSession, chatID string, argv ...string) *command.SlashOutput {
	t.Helper()
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    chatID,
		UserID:    "test-user",
		Text:      "/" + strings.Join(argv, " "),
		MessageID: "msg-1",
		Args:      argv,
	})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if out == nil {
		t.Fatal("Handle returned nil SlashOutput")
	}
	return out
}

// ─── the resume state ──────────────────────────────────────────────

// TestFactory_RecordIsInMemory covers R11: the record of the last
// review lives in the factory's memory, keyed by chat id. Nothing is
// written to disk.
func TestFactory_RecordIsInMemory(t *testing.T) {
	dir := t.TempDir()
	f := NewFactory()

	f.record("chat-a", "agent-x", dir, []reviewOutcome{
		{pattern: "**/*.go", files: []string{"a.go"}},
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	})

	rec, ok := f.lookup("chat-a")
	if !ok {
		t.Fatal("the record was not found")
	}
	if rec.agent != "agent-x" {
		t.Errorf("record agent = %q, want %q", rec.agent, "agent-x")
	}
	if rec.workspace != dir {
		t.Errorf("record workspace = %q, want %q", rec.workspace, dir)
	}
	if len(rec.outcomes) != 2 {
		t.Fatalf("record carries %d outcomes, want 2", len(rec.outcomes))
	}
	if rec.outcomes[1].sessionID != "sess-ts" {
		t.Errorf("outcome session id = %q, want %q", rec.outcomes[1].sessionID, "sess-ts")
	}
	if rec.outcomes[1].err == nil {
		t.Error("the failed outcome carries no error")
	}

	// Another chat has no record of its own.
	if _, ok := f.lookup("chat-b"); ok {
		t.Error("a chat without a review has a record")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the record wrote %d files to disk", len(entries))
	}
}

// TestFactory_RunningReviewsAreCounted covers the running marker:
// overlapping reviews are counted, so the chat is only free again
// when the last one finishes.
func TestFactory_RunningReviewsAreCounted(t *testing.T) {
	f := NewFactory()
	if f.reviewRunning("chat-a") {
		t.Error("a fresh chat reports a running review")
	}
	f.startReview("chat-a")
	f.startReview("chat-a")
	if !f.reviewRunning("chat-a") {
		t.Error("the running review is not reported")
	}
	f.finishReview("chat-a")
	if !f.reviewRunning("chat-a") {
		t.Error("one of two overlapping reviews finished — the chat is still busy")
	}
	f.finishReview("chat-a")
	if f.reviewRunning("chat-a") {
		t.Error("the last review finished — the chat is free")
	}
}

// ─── the continuation path ─────────────────────────────────────────

// TestHandle_ContinueResumesTheFailedGroup covers R2, R3, R4, R5 and
// R9: only the failed group is resumed, it is resumed with its
// recorded session id in the recorded workspace, it receives the text
// "continue", the runner is the recorded agent (not the chat
// session's), and the merged result is delivered like a review's.
func TestHandle_ContinueResumesTheFailedGroup(t *testing.T) {
	agents := newTestAgents(t)
	recorded := &reviewSessionStarter{name: "review-recorded-agent", reply: "resumed findings"}
	chatRunner := &reviewSessionStarter{name: "review-chat-agent", reply: "must not run"}
	agents.registry.Register(recorded)
	agents.registry.Register(chatRunner)

	em := newRecordingEmitter()
	const chatID = "review-continue-chat"
	cs, asDriver := newContinueChatSession(t, chatID, chatRunner.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, recorded.name, "/recorded/ws", []reviewOutcome{
		{pattern: "**/*.go", files: []string{"a.go"}},
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	})

	out := dispatchContinue(t, f, cs, chatID, "review", "-c")
	if out.Reply != "" {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}

	em.waitForOutReply(t, "resumed findings")

	// Exactly one session was resumed — the failed one.
	cfgs := recorded.startedConfigs()
	if len(cfgs) != 1 {
		t.Fatalf("resumed %d sessions, want 1 (the failed group only)", len(cfgs))
	}
	if cfgs[0].SessionID != "sess-ts" {
		t.Errorf("resumed session id = %q, want %q", cfgs[0].SessionID, "sess-ts")
	}
	if cfgs[0].Workspace != "/recorded/ws" {
		t.Errorf("resumed workspace = %q, want the recorded workspace", cfgs[0].Workspace)
	}

	// The continuation runs under the same 30-minute review budget
	// as a review.
	deadline := recorded.startDeadline()
	if deadline.IsZero() {
		t.Error("the continuation ran without a deadline")
	} else if remaining := time.Until(deadline); remaining > timeouts.Review || remaining < timeouts.Review-time.Minute {
		t.Errorf("the continuation deadline is %v away, want ~%v", remaining, timeouts.Review)
	}

	// The runner is the recorded agent — not the chat session's.
	if got := chatRunner.startedConfigs(); len(got) != 0 {
		t.Errorf("the chat session's agent started %d sessions, want 0", len(got))
	}

	// The resumed session received the single text block "continue".
	blocks := recorded.receivedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("the resumed session received %d blocks, want 1", len(blocks))
	}
	if blocks[0].Type != agent.ContentText {
		t.Errorf("the resumed session received a %v block, want a text block", blocks[0].Type)
	}
	if blocks[0].Text != "continue" {
		t.Errorf("the resumed session received %q, want %q", blocks[0].Text, "continue")
	}

	// The findings landed in the chat session's AS, wrapped like a
	// review's.
	injected := asDriver.blocks()
	if len(injected) != 1 {
		t.Fatalf("the AS received %d blocks, want 1", len(injected))
	}
	wantPrefix := fmt.Sprintf("## Code review of /recorded/ws (run by %q)", recorded.name)
	if !strings.Contains(injected[0].Text, wantPrefix) {
		t.Errorf("the injected block does not carry the review wrapper; got:\n%s", injected[0].Text)
	}
	if !strings.Contains(injected[0].Text, "resumed findings") {
		t.Errorf("the injected block does not carry the findings; got:\n%s", injected[0].Text)
	}
}

// TestHandle_ContinueWithoutARecord covers the "nothing to continue"
// path: no review ran in this chat, so there is nothing to resume.
func TestHandle_ContinueWithoutARecord(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewSessionStarter{name: "review-no-record-agent"}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-no-record-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	out := dispatchContinue(t, f, cs, chatID, "review", "-c")

	if !strings.Contains(out.Reply, "no review to continue") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	if got := starter.startedConfigs(); len(got) != 0 {
		t.Errorf("a session was started without a record to resume")
	}
}

// TestHandle_ContinueWithAnUnknownAgent covers the recorded agent
// being gone from the registry.
func TestHandle_ContinueWithAnUnknownAgent(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewSessionStarter{name: "review-gone-agent"}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-unknown-agent-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, "review-never-registered", "/recorded/ws", []reviewOutcome{
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	})

	out := dispatchContinue(t, f, cs, chatID, "review", "-c")

	if !strings.Contains(out.Reply, `unknown agent "review-never-registered"`) {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	if got := starter.startedConfigs(); len(got) != 0 {
		t.Errorf("a session was started for an unknown agent")
	}
}

// TestHandle_ContinueWithNothingToResume covers the two "nothing left
// to resume" replies: the last review completed, or the unfinished
// group never reached the agent.
func TestHandle_ContinueWithNothingToResume(t *testing.T) {
	cases := []struct {
		name     string
		outcomes []reviewOutcome
		want     string
	}{
		{
			name: "the last review completed",
			outcomes: []reviewOutcome{
				{pattern: "**/*.go", files: []string{"a.go"}},
			},
			want: "the last review completed",
		},
		{
			name: "the unfinished group never reached the agent",
			outcomes: []reviewOutcome{
				{pattern: "**/*.go", files: []string{"a.go"}},
				{pattern: "**/*.ts", files: []string{"c.ts"}, err: errors.New("spawn failed")},
			},
			want: "the unfinished group never reached the agent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agents := newTestAgents(t)
			starter := &reviewSessionStarter{name: "review-nothing-agent"}
			agents.registry.Register(starter)

			em := newRecordingEmitter()
			const chatID = "review-nothing-chat"
			cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

			f := NewFactory()
			f.record(chatID, starter.name, "/recorded/ws", tc.outcomes)

			out := dispatchContinue(t, f, cs, chatID, "review", "-c")

			if !strings.Contains(out.Reply, tc.want) {
				t.Fatalf("reply %q does not contain %q", out.Reply, tc.want)
			}
			if got := starter.startedConfigs(); len(got) != 0 {
				t.Errorf("a session was started with nothing to resume")
			}
		})
	}
}

// TestHandle_ContinueRejectedWhileAReviewRuns covers R12: a
// continuation is refused while a review is running in the same chat
// — the two flows would fight over the same group sessions.
func TestHandle_ContinueRejectedWhileAReviewRuns(t *testing.T) {
	agents := newTestAgents(t)
	runner := &reviewSessionStarter{name: "review-busy-agent", block: make(chan struct{})}
	agents.registry.Register(runner)

	em := newRecordingEmitter()
	const chatID = "review-busy-chat"
	cs, _ := newContinueChatSession(t, chatID, runner.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, runner.name, "/recorded/ws", []reviewOutcome{
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	})

	// Start a review for this chat and hold it running.
	dispatchContinue(t, f, cs, chatID, "review")

	out := dispatchContinue(t, f, cs, chatID, "review", "-c")

	if !strings.Contains(out.Reply, "a review is already running in this chat") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	close(runner.block)
}

// TestHandle_ContinueTwiceReportsNothingLeft covers R13: after a
// continuation the record holds that run's outcomes, so a second
// continuation finds nothing left to resume.
func TestHandle_ContinueTwiceReportsNothingLeft(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewSessionStarter{name: "review-twice-agent", reply: "resumed findings"}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-twice-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, starter.name, "/recorded/ws", []reviewOutcome{
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	})

	dispatchContinue(t, f, cs, chatID, "review", "-c")
	em.waitForOutReply(t, "resumed findings")

	out := dispatchContinue(t, f, cs, chatID, "review", "-c")

	if !strings.Contains(out.Reply, "nothing to continue") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
}

// TestHandle_ContinueAfterAFailedRetryReportsTheSpentAttempt covers
// the group that failed a second time: its one resume attempt is
// spent, so the next `--continue` says so — the group did reach the
// agent, it was already retried.
func TestHandle_ContinueAfterAFailedRetryReportsTheSpentAttempt(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewSessionStarter{name: "review-retried-agent", fail: true}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-retried-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, starter.name, "/recorded/ws", []reviewOutcome{
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	})

	dispatchContinue(t, f, cs, chatID, "review", "-c")
	em.waitForOutReply(t, "/review continue failed")

	out := dispatchContinue(t, f, cs, chatID, "review", "-c")

	if !strings.Contains(out.Reply, "already retried") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	if strings.Contains(out.Reply, "never reached the agent") {
		t.Errorf("the reply blames the wrong cause: %q", out.Reply)
	}
}

// TestHandle_ReviewWithoutGroupsKeepsTheResumeState: a review that
// ran no group at all — the diff was empty, or the bridge cannot
// review — produces no outcomes, so it must not wipe the chat's
// resume state: the previous run's unfinished groups are still
// unfinished.
func TestHandle_ReviewWithoutGroupsKeepsTheResumeState(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewSessionStarter{name: "review-empty-diff-agent", reviewErr: agent.ErrNoDiff}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-empty-diff-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, starter.name, "/recorded/ws", []reviewOutcome{
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	})

	dispatchContinue(t, f, cs, chatID, "review")
	em.waitForOutReply(t, "no diff to review")

	rec, ok := f.lookup(chatID)
	if !ok {
		t.Fatal("the resume state was wiped")
	}
	if len(rec.outcomes) != 1 {
		t.Fatalf("the record carries %d outcomes, want the previous run's 1", len(rec.outcomes))
	}
	if rec.outcomes[0].sessionID != "sess-ts" {
		t.Errorf("the resumable session id was dropped: %+v", rec.outcomes[0])
	}
}

// TestHandle_ContinuationDoesNotClobberANewerRecord: a continuation
// that finishes after a newer review ran must not overwrite the
// newer review's resume state — the newer run owns the chat.
func TestHandle_ContinuationDoesNotClobberANewerRecord(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewSessionStarter{
		name:  "review-clobber-agent",
		reply: "resumed findings",
		block: make(chan struct{}),
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-clobber-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, starter.name, "/recorded/ws", []reviewOutcome{
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	})

	dispatchContinue(t, f, cs, chatID, "review", "-c")
	waitForStart(t, starter)

	// A newer review finished while the continuation was still
	// running: it owns the chat's resume state.
	f.record(chatID, starter.name, "/newer/ws", []reviewOutcome{
		{pattern: "**/*.go", files: []string{"a.go"}, sessionID: "sess-go", err: errors.New("boom")},
	})
	close(starter.block)
	em.waitForOutReply(t, "resumed findings")

	rec, ok := f.lookup(chatID)
	if !ok {
		t.Fatal("the record is gone")
	}
	if rec.workspace != "/newer/ws" {
		t.Errorf("the record workspace = %q, want the newer run's %q", rec.workspace, "/newer/ws")
	}
	if len(rec.outcomes) != 1 || rec.outcomes[0].sessionID != "sess-go" {
		t.Errorf("the newer run's outcomes were lost: %+v", rec.outcomes)
	}
}

// waitForStart waits until the starter's session has been opened.
func waitForStart(t *testing.T, starter *reviewSessionStarter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(starter.startedConfigs()) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the session was never started")
}
