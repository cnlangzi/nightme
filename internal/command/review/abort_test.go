package review

// Tests for `/review --abort`: stopping the review that is running
// in this chat, and picking the stopped groups up again.
//
// The abort reaches the review's agent sessions through the run's
// context — the command layer holds no handle on the fan-out's
// sessions — so the test double below opens a real agent session and
// holds its turn open until the run's context is cancelled.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// ─── test doubles ──────────────────────────────────────────────────

// abortStarter is a Starter whose review opens a live agent session
// and holds its turn open until the starter's hold is released or the
// run's context is cancelled.
//
// It is the shape of a review that is still running when the user
// aborts it.
type abortStarter struct {
	name string

	// hold keeps the session's turn open. A nil hold answers right
	// away.
	hold chan struct{}

	mu      sync.Mutex
	started []agent.StartConfig
	drivers []*abortDriver
}

func (s *abortStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "", nil, nil)
}
func (s *abortStarter) Detect() error { return nil }
func (s *abortStarter) Init(string)   {}

func (s *abortStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("abortStarter: RunOnce must not be used on the session path")
}

// Review opens the session and runs one turn over it.
//
// The group's outcome is reported through the observer the fan-out
// installs — with the session id the group ran under, even when the
// group was cancelled. That session id is what makes the stopped
// group resumable.
func (s *abortStarter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	res, err := agent.RunSession(ctx, s, cfg, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "review",
	}})
	if o := agent.ParseRunOnceOptions(opts); o.OnReviewJob != nil {
		o.OnReviewJob(agent.ReviewJob{
			Pattern:   "**/*.go",
			Source:    "abort-test",
			Files:     []string{"a.go"},
			SessionID: res.SessionID,
		}, err)
	}
	return res, err
}

func (s *abortStarter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	// A bridge refuses to spawn on a cancelled context — the shape of
	// the group the abort caught before it started.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	s.started = append(s.started, cfg)
	index := len(s.started)
	// A resume reopens the session it was given — the bridge keeps
	// the conversation under that id. A fresh session gets a new one.
	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("sess-%d", index)
	}
	d := &abortDriver{
		starter:     s,
		events:      make(chan agent.AgentEvent, 8),
		sessionID:   sessionID,
		hasDeadline: hasDeadline,
	}
	s.drivers = append(s.drivers, d)
	s.mu.Unlock()
	return agent.NewAgent(s.Info(), 0, d.events, d), nil
}

// startedConfigs returns the StartConfig each Start call received.
func (s *abortStarter) startedConfigs() []agent.StartConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.StartConfig(nil), s.started...)
}

// closedSessions returns how many of the sessions were closed.
func (s *abortStarter) closedSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.drivers {
		if d.isClosed() {
			n++
		}
	}
	return n
}

// releaseHold lets the held turn answer.
func (s *abortStarter) releaseHold() {
	s.mu.Lock()
	hold := s.hold
	s.hold = nil
	s.mu.Unlock()
	if hold != nil {
		close(hold)
	}
}

// hangClose makes the sessions' close hang — the shape of a bridge
// that does not come back — and returns the function that releases
// it.
func (s *abortStarter) hangClose() func() {
	block := make(chan struct{})
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.drivers {
		d.mu.Lock()
		d.closeBlock = block
		d.mu.Unlock()
	}
	return func() { close(block) }
}

// abortDriver replays a Ready event and then either answers or waits
// for the turn's context to be cancelled.
type abortDriver struct {
	starter   *abortStarter
	events    chan agent.AgentEvent
	sessionID string
	// hasDeadline records whether the session's context carried a
	// deadline.
	hasDeadline bool

	mu     sync.Mutex
	closed bool
	// closeBlock holds the session's close open — the shape of a
	// bridge that hangs on close.
	closeBlock chan struct{}
	// received are the blocks the session was sent.
	received []agent.ContentBlock
}

// deadlineSet reports whether the session ran with a deadline.
func (d *abortDriver) deadlineSet() bool {
	return d.hasDeadline
}

func (d *abortDriver) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	d.mu.Lock()
	d.received = append(d.received, blocks...)
	d.mu.Unlock()
	d.events <- agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.sessionID,
		Model:     "model-x",
	}
	d.starter.mu.Lock()
	hold := d.starter.hold
	d.starter.mu.Unlock()
	if hold == nil {
		d.events <- agent.AgentEvent{
			Kind:   agent.EventAgentResult,
			Result: &agent.AgentResultEvent{Text: "reviewed"},
		}
		return nil
	}
	go func() {
		select {
		case <-hold:
			d.events <- agent.AgentEvent{
				Kind:   agent.EventAgentResult,
				Result: &agent.AgentResultEvent{Text: "reviewed"},
			}
		case <-ctx.Done():
		}
	}()
	return nil
}

// receivedBlocks returns the blocks the session received.
func (d *abortDriver) receivedBlocks() []agent.ContentBlock {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]agent.ContentBlock(nil), d.received...)
}

func (d *abortDriver) SendPermission(string) error { return nil }
func (d *abortDriver) Reset(context.Context) error { return nil }

func (d *abortDriver) Close() error {
	d.mu.Lock()
	block := d.closeBlock
	d.mu.Unlock()
	if block != nil {
		<-block
	}
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

func (d *abortDriver) Stop(context.Context) error { return agent.ErrNotSupported }
func (d *abortDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// isClosed reports whether the session was closed.
func (d *abortDriver) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// ─── helpers ───────────────────────────────────────────────────────

// dispatchWithMessage invokes the factory's Handle for the given chat
// and message and returns the reply.
func dispatchWithMessage(t *testing.T, f *Factory, cs *chatsession.ChatSession,
	chatID, msgID string, argv ...string) *command.SlashOutput {
	t.Helper()
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    chatID,
		UserID:    "test-user",
		Text:      "/" + strings.Join(argv, " "),
		MessageID: msgID,
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

// waitForSessions waits until the starter opened n sessions.
func waitForSessions(t *testing.T, starter *abortStarter, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(starter.startedConfigs()) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the starter opened %d sessions, want %d", len(starter.startedConfigs()), n)
}

// waitForClose waits until the starter closed n sessions.
func waitForClose(t *testing.T, starter *abortStarter, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if starter.closedSessions() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d sessions were closed, want %d", starter.closedSessions(), n)
}

// abortNotices returns the abort notices the channel carries.
func abortNotices(em *recordingEmitter) []messages.OutboundMessage {
	var out []messages.OutboundMessage
	for _, msg := range em.messages() {
		if msg.Kind == messages.OutReply && strings.Contains(msg.Text, "review aborted") {
			out = append(out, msg)
		}
	}
	return out
}

// ─── the registry ──────────────────────────────────────────────────

// TestFactory_AFinishedFanoutIsNotAbortable covers the late abort: a
// run whose fan-out is over has nothing left to stop, and the
// delivery that follows must not be cancelled by an abort.
func TestFactory_AFinishedFanoutIsNotAbortable(t *testing.T) {
	f := NewFactory()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	h := &runHandle{cancel: cancel, done: make(chan struct{}), agent: "agent-x", kind: "review"}
	f.beginRun("chat-a", h)

	// The fan-out ends: the findings are in hand.
	f.finishFanout("chat-a", h)

	if _, _, ok := f.abortRun("chat-a"); ok {
		t.Error("a run whose fan-out is over was abortable")
	}
	if ctx.Err() != nil {
		t.Error("the late abort cancelled the run's context")
	}
}

// TestFactory_ASecondAbortDoesNotReportTheSameRun covers the abort
// issued while the first one is still winding down: the same run is
// not reported twice.
func TestFactory_ASecondAbortDoesNotReportTheSameRun(t *testing.T) {
	f := NewFactory()
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	h := &runHandle{cancel: cancel, done: make(chan struct{}), agent: "agent-x", kind: "review"}
	f.beginRun("chat-a", h)

	first, _, ok := f.abortRun("chat-a")
	if !ok {
		t.Fatal("the first abort found no run")
	}
	if first != h {
		t.Fatalf("the abort targeted %+v, want the registered run", first)
	}
	if _, _, ok := f.abortRun("chat-a"); ok {
		t.Error("the second abort reported the same run again")
	}
}

// TestCarrySessionIDs covers the group the abort caught before it
// spawned: its outcome carries no session id, but the session it was
// reopening is still the one the record holds.
func TestCarrySessionIDs(t *testing.T) {
	outcomes := []reviewOutcome{
		{pattern: "**/*.go", source: "system", sessionID: "sess-go"},
		{pattern: "**/*.ts", source: "system"},
	}
	jobs := []agent.ReviewJob{
		{Pattern: "**/*.go", Source: "system", SessionID: "sess-go"},
		{Pattern: "**/*.ts", Source: "system", SessionID: "sess-ts"},
	}

	carrySessionIDs(outcomes, jobs)

	if outcomes[1].sessionID != "sess-ts" {
		t.Errorf("the stopped group's session id = %q, want %q", outcomes[1].sessionID, "sess-ts")
	}
	if outcomes[0].sessionID != "sess-go" {
		t.Errorf("the group's own session id was overwritten: %q", outcomes[0].sessionID)
	}
}

// waitForFanout waits until the chat's run's fan-out is over — the
// run is registered but its findings are in hand.
func waitForFanout(t *testing.T, f *Factory, chatID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		runs := f.runs[chatID]
		over := len(runs) == 0 || !runs[0].abortable
		f.mu.Unlock()
		if over {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the fan-out did not end")
}

// ─── the abort ─────────────────────────────────────────────────────

// TestHandle_AbortStopsTheRunningReview covers R4 and R5: the abort
// cancels the run's context, the run's session is closed, the run
// reports `cancelled`, the user gets one abort notice on the review's
// own message, and the findings are not injected into the chat
// session's agent session.
func TestHandle_AbortStopsTheRunningReview(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-abort-agent", hold: make(chan struct{})}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-abort-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)
	enableThink(t, cs)

	f := NewFactory()

	// The dispatcher holds the review's message while the command
	// runs; the test holds it too, so the settled outcome carries
	// what the run reported.
	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review")
	waitForSessions(t, starter, 1)

	out := dispatchWithMessage(t, f, cs, chatID, "msg-2", "review", "--abort")

	if !strings.Contains(out.Reply, "aborted the review ("+starter.name+")") {
		t.Fatalf("the reply does not name the aborted run: %q", out.Reply)
	}

	// The run's session is closed — the abort reached the agent
	// sessions through the run's context.
	waitForClose(t, starter, 1)

	// The run reports `cancelled`, not a failure.
	hold.Release()
	got := waitOutcome(t, em, 5*time.Second)
	assertOutcome(t, got, messages.OutcomeCancelled, map[string]string{
		"agent":  starter.name,
		"reason": "aborted",
	})

	// Exactly one abort notice, on the review's own message.
	notices := abortNotices(em)
	if len(notices) != 1 {
		t.Fatalf("the channel carries %d abort notices, want 1: %+v", len(notices), notices)
	}
	if notices[0].ReplyTo != "msg-1" {
		t.Errorf("the notice is anchored to %q, want the review's message", notices[0].ReplyTo)
	}

	// The findings are not injected — the user chose to stop the run.
	if blocks := asDriver.blocks(); len(blocks) != 0 {
		t.Errorf("the agent session received %d blocks, want none", len(blocks))
	}
}

// TestHandle_AbortWithNoRunInFlight covers R9: an abort with nothing
// to stop says so and reports `noop`.
func TestHandle_AbortWithNoRunInFlight(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-abort-empty-agent"}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-abort-empty-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	hold := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), hold)
	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    chatID,
		MessageID: "msg-1",
		Text:      "/review --abort",
		Args:      []string{"review", "--abort"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	hold.Release()

	if !strings.Contains(out.Reply, "no review is running in this chat") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	got := waitOutcome(t, em, 5*time.Second)
	assertOutcome(t, got, messages.OutcomeNoop, nil)
}

// TestHandle_AbortNeedsNoWorkspaceNorAgent covers R3: the abort
// targets the run that is already in flight, so it works in a chat
// that never picked a workspace or an agent.
func TestHandle_AbortNeedsNoWorkspaceNorAgent(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-abort-bare-agent"}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-abort-bare-chat"

	// The chat has no workspace and no selected agent.
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate(chatID, "")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	cs.WithEmitter(em)

	f := NewFactory()
	out := dispatchWithMessage(t, f, cs, chatID, "msg-1", "review", "--abort")

	if !strings.Contains(out.Reply, "no review is running in this chat") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	if strings.Contains(out.Reply, "no workspace set") ||
		strings.Contains(out.Reply, "no active agent configured") {
		t.Errorf("the abort asked for a workspace or an agent: %q", out.Reply)
	}
}

// TestHandle_AbortKeepsTheStoppedGroupsResumable covers R6 and R7:
// the aborted run's record holds the stopped group with its session
// id, and a following `--continue` reopens that session and sends it
// the text "continue".
func TestHandle_AbortKeepsTheStoppedGroupsResumable(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-abort-resume-agent", hold: make(chan struct{})}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-abort-resume-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review")
	waitForSessions(t, starter, 1)

	dispatchWithMessage(t, f, cs, chatID, "msg-2", "review", "--abort")
	waitForClose(t, starter, 1)

	// The stopped group is in the record with its session id.
	rec, ok := f.lookup(chatID)
	if !ok {
		t.Fatal("the aborted run left no record")
	}
	if len(rec.outcomes) != 1 {
		t.Fatalf("the record holds %d outcomes, want 1", len(rec.outcomes))
	}
	if rec.outcomes[0].err == nil {
		t.Error("the stopped group carries no error")
	}
	if rec.outcomes[0].sessionID == "" {
		t.Error("the stopped group carries no session id")
	}
	sessionID := rec.outcomes[0].sessionID

	// The continuation reopens that session and sends it "continue".
	// The chat is free again — the abort waited for the run to wind
	// down, so the follow-up is not refused as "already running".
	starter.releaseHold()
	out := dispatchWithMessage(t, f, cs, chatID, "msg-3", "review", "-c")
	if strings.Contains(out.Reply, "a review is already running in this chat") {
		t.Fatalf("the follow-up continuation was refused: %q", out.Reply)
	}
	waitForSessions(t, starter, 2)

	cfgs := starter.startedConfigs()
	resumed := cfgs[len(cfgs)-1]
	if resumed.SessionID != sessionID {
		t.Errorf("the resumed session id = %q, want the recorded %q", resumed.SessionID, sessionID)
	}

	// The resumed session received the single text block "continue".
	blocks := starter.drivers[len(starter.drivers)-1].receivedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("the resumed session received %d blocks, want 1", len(blocks))
	}
	if blocks[0].Type != agent.ContentText {
		t.Errorf("the resumed session received a %v block, want a text block", blocks[0].Type)
	}
	if blocks[0].Text != "continue" {
		t.Errorf("the resumed session received %q, want %q", blocks[0].Text, "continue")
	}
	hold.Release()
}

// TestHandle_AbortOnlyStopsTheNewestRun covers R11: with two runs in
// flight, the abort stops the newer one and says how many are still
// running.
func TestHandle_AbortOnlyStopsTheNewestRun(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-abort-two-agent", hold: make(chan struct{})}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-abort-two-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review")
	waitForSessions(t, starter, 1)
	dispatchWithMessage(t, f, cs, chatID, "msg-2", "review")
	waitForSessions(t, starter, 2)

	out := dispatchWithMessage(t, f, cs, chatID, "msg-3", "review", "--abort")

	if !strings.Contains(out.Reply, "1 review still running") {
		t.Fatalf("the reply does not count the other run: %q", out.Reply)
	}
	// The newer run's session is closed; the older one still runs.
	waitForClose(t, starter, 1)
	if got := starter.closedSessions(); got != 1 {
		t.Errorf("%d sessions were closed, want only the newest run's", got)
	}
	hold.Release()
}

// TestHandle_AbortStopsARunningContinuation covers R12: a running
// `--continue` is aborted like a running review.
func TestHandle_AbortStopsARunningContinuation(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-abort-cont-agent", hold: make(chan struct{})}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-abort-cont-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, starter.name, "/recorded/ws", []reviewOutcome{
		{pattern: "**/*.go", files: []string{"a.go"}, sessionID: "sess-go", err: errors.New("boom")},
	})

	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review", "-c")
	waitForSessions(t, starter, 1)

	out := dispatchWithMessage(t, f, cs, chatID, "msg-2", "review", "--abort")
	if !strings.Contains(out.Reply, "aborted the review") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	waitForClose(t, starter, 1)

	hold.Release()
	got := waitOutcome(t, em, 5*time.Second)
	assertOutcome(t, got, messages.OutcomeCancelled, map[string]string{
		"agent":  starter.name,
		"reason": "aborted",
	})
}

// TestHandle_AbortSaysTheReviewIsStillWindingDown covers the case
// where the run does not wind down within the abort's wait: the reply
// says so, and the run keeps unwinding in the background.
func TestHandle_AbortSaysTheReviewIsStillWindingDown(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-abort-hang-agent", hold: make(chan struct{})}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-abort-hang-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	orig := abortWait
	abortWait = 50 * time.Millisecond
	t.Cleanup(func() { abortWait = orig })

	f := NewFactory()
	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review")
	waitForSessions(t, starter, 1)

	// The bridge hangs on close — the run cannot wind down in time.
	release := starter.hangClose()

	out := dispatchWithMessage(t, f, cs, chatID, "msg-2", "review", "--abort")
	if !strings.Contains(out.Reply, "still winding down") {
		t.Fatalf("the reply does not say the review is still winding down: %q", out.Reply)
	}

	release()
	waitForClose(t, starter, 1)
	hold.Release()
}

// TestHandle_ChatCloseIsNotReportedAsAnAbort covers R13: a chat close
// cancels the run's context too, but it is not an abort — the run
// reports a failure and sends no abort notice.
func TestHandle_ChatCloseIsNotReportedAsAnAbort(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-chat-close-agent", hold: make(chan struct{})}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-chat-close-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review")
	waitForSessions(t, starter, 1)

	// The chat's context is cancelled — not by an abort.
	cs.ResetContext()
	waitForClose(t, starter, 1)

	hold.Release()
	got := waitOutcome(t, em, 5*time.Second)
	if got.Status == messages.OutcomeCancelled {
		t.Fatalf("the chat close was reported as an abort: %+v", got)
	}
	if notices := abortNotices(em); len(notices) != 0 {
		t.Errorf("the chat close sent %d abort notices, want none", len(notices))
	}
}

// TestHandle_AbortAfterTheFanoutIsANoOp covers the late abort: the
// review's fan-out is over and its findings are on their way to the
// agent — an abort must not cancel that delivery and report the
// completed review as aborted.
func TestHandle_AbortAfterTheFanoutIsANoOp(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-late-abort-agent"}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-late-abort-chat"
	cs, asDriver := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)
	// The prompt-end bus is off — the test does not drive a real
	// readpump, so the injected turn never ends on its own.
	cs.PromptEndBus = nil

	// The delivery of the findings waits — the review is past its
	// fan-out but its turn is still in flight.
	gate := make(chan struct{})
	asDriver.gate = gate

	f := NewFactory()
	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review")
	waitForFanout(t, f, chatID)

	out := dispatchWithMessage(t, f, cs, chatID, "msg-2", "review", "--abort")
	if !strings.Contains(out.Reply, "no review is running in this chat") {
		t.Fatalf("the late abort reported a run: %q", out.Reply)
	}

	// The review's delivery completes: the findings reach the agent
	// and the run reports a plain success.
	close(gate)
	blocks := waitForBlocks(t, asDriver, 1)
	if len(blocks) != 1 {
		t.Fatalf("the agent session received %d blocks, want 1", len(blocks))
	}
	hold.Release()
	got := waitOutcome(t, em, 5*time.Second)
	assertOutcome(t, got, messages.OutcomeOK, map[string]string{"agent": starter.name})
}

// TestHandle_AbortedContinuationKeepsTheSessionIds covers the groups
// the abort caught before they spawned: the fan-out's semaphore
// queues them, so they report no session id — but the sessions they
// were reopening are still resumable.
func TestHandle_AbortedContinuationKeepsTheSessionIds(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-abort-ids-agent", hold: make(chan struct{})}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-abort-ids-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	// More groups than the fan-out's semaphore has slots, so the last
	// one waits for a slot that only frees when the abort unwinds the
	// running ones.
	var outcomes []reviewOutcome
	for i := 1; i <= 5; i++ {
		outcomes = append(outcomes, reviewOutcome{
			pattern:   fmt.Sprintf("group-%d", i),
			source:    "abort-test",
			files:     []string{"a.go"},
			sessionID: fmt.Sprintf("sess-%d", i),
			err:       errors.New("boom"),
		})
	}

	f := NewFactory()
	f.record(chatID, starter.name, "/recorded/ws", outcomes)

	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review", "-c")
	waitForSessions(t, starter, 4)

	dispatchWithMessage(t, f, cs, chatID, "msg-2", "review", "--abort")
	waitForClose(t, starter, 4)

	rec, ok := f.lookup(chatID)
	if !ok {
		t.Fatal("the aborted continuation left no record")
	}
	if len(rec.outcomes) != 5 {
		t.Fatalf("the record holds %d outcomes, want 5", len(rec.outcomes))
	}
	// The outcomes are reported in completion order, so the session
	// ids are compared as a set: every group kept the session it was
	// reopening — including the one the abort caught before it
	// spawned.
	ids := map[string]bool{}
	for _, o := range rec.outcomes {
		if o.sessionID == "" {
			t.Errorf("a stopped group lost its session id: %+v", o)
			continue
		}
		ids[o.sessionID] = true
	}
	for i := 1; i <= 5; i++ {
		want := fmt.Sprintf("sess-%d", i)
		if !ids[want] {
			t.Errorf("the record does not hold the session id %q: %v", want, ids)
		}
	}
	hold.Release()
}

// TestReviewRunHasNoDeadline covers R8: the review's context carries
// no deadline — the run ends when the chat closes or the user aborts
// it.
func TestReviewRunHasNoDeadline(t *testing.T) {
	agents := newTestAgents(t)
	starter := &abortStarter{name: "review-no-deadline-agent", hold: make(chan struct{})}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-no-deadline-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	hold := cs.Hold("msg-1")
	dispatchWithMessage(t, f, cs, chatID, "msg-1", "review")
	waitForSessions(t, starter, 1)

	// The session's context carries no deadline.
	for _, d := range starter.drivers {
		if d.deadlineSet() {
			t.Error("the review's session ran with a deadline, want none")
		}
	}

	dispatchWithMessage(t, f, cs, chatID, "msg-2", "review", "--abort")
	waitForClose(t, starter, 1)
	hold.Release()
}
