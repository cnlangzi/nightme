// Package command_test — the settled-signal contract, end to end.
//
// Every inbound message gets exactly one settled message, carrying
// the structured Outcome of the work that message triggered. The
// point of these tests is the TIMING of that message: a command can
// finish while the agent work it started is still running, and the
// settled signal must not claim the message is done before the agent
// work is.
//
// The tests drive the real dispatch chain (inbound.Router →
// Commander → command → ChatSession → AgentSession → readpump →
// PromptEndBus) with a stub agent whose turns END. The stub's turn
// end is gated, so a test can hold a turn open and observe that no
// settled message has gone out yet.
//
// The settled message is invisible in every IM channel — the channels
// drop it by design — so the assertions read it off the echo
// channel, which records every outbound message.

package command_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel/echo"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/command/cwd"
	"github.com/cnlangzi/nightme/internal/command/queue"
	"github.com/cnlangzi/nightme/internal/command/review"
	"github.com/cnlangzi/nightme/internal/command/steer"
	"github.com/cnlangzi/nightme/internal/command/stop"
	"github.com/cnlangzi/nightme/internal/command/think"
	"github.com/cnlangzi/nightme/internal/command/tools"
	"github.com/cnlangzi/nightme/internal/command/watch"
	"github.com/cnlangzi/nightme/internal/gateway/inbound"
	"github.com/cnlangzi/nightme/internal/gatewaytest"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/shell"
)

// ─── stub agent ─────────────────────────────────────────────────────

// settleBridge is the state shared by every agent instance the
// harness spawns: the gate the turn's terminal event waits on, and
// the signals the tests synchronise on.
type settleBridge struct {
	mu sync.Mutex
	// failSend makes the next SendBlocks fail — the "injection
	// failed" path.
	failSend bool

	turnGate    chan struct{}
	turnStarted chan struct{}
	reviewCalls chan struct{}
}

func newSettleBridge() *settleBridge {
	return &settleBridge{
		turnGate:    make(chan struct{}),
		turnStarted: make(chan struct{}, 16),
		reviewCalls: make(chan struct{}, 4),
	}
}

// settleAgent is a stub agent whose turns END. A real turn ends with
// a terminal event, and that event is what makes the chat session
// publish the prompt-end the settled signal waits for.
//
// The turn's end is GATED: SendBlocks returns immediately and a
// goroutine waits for the gate before emitting the terminal event.
// A test can therefore hold a turn open and inspect the settled
// state while the agent work is still running.
type settleAgent struct {
	bridge *settleBridge
	pid    int
	events chan agent.AgentEvent

	mu     sync.Mutex
	closed bool
}

func newSettleAgent(b *settleBridge, pid int) *settleAgent {
	return &settleAgent{
		bridge: b,
		pid:    pid,
		events: make(chan agent.AgentEvent, 16),
	}
}

func (a *settleAgent) Info() agent.Info {
	return agent.NewInfo("settle", agent.ModeACP, "", nil, nil)
}
func (a *settleAgent) Detect() error { return nil }
func (a *settleAgent) Init(string)   {}
func (a *settleAgent) Clone() agent.Starter {
	return newSettleAgent(a.bridge, a.pid)
}
func (a *settleAgent) Start(_ context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	return agent.NewAgent(a.Info(), a.pid, a.events, &settleDriver{inner: a}), nil
}

// SendBlocks accepts the prompt and ends the turn — but only once
// the gate opens. The terminal event is emitted from a goroutine so
// the caller (Submit / the injection path) is never blocked.
func (a *settleAgent) SendBlocks(context.Context, []agent.ContentBlock) error {
	a.bridge.mu.Lock()
	fail := a.bridge.failSend
	a.bridge.mu.Unlock()
	if fail {
		return errors.New("settleAgent: send failed")
	}
	select {
	case a.bridge.turnStarted <- struct{}{}:
	default:
	}
	go func() {
		<-a.bridge.turnGate
		a.mu.Lock()
		closed := a.closed
		a.mu.Unlock()
		if closed {
			return
		}
		a.events <- agent.AgentEvent{Kind: agent.EventAgentDone}
	}()
	return nil
}

func (a *settleAgent) SendPermission(string) error { return nil }
func (a *settleAgent) New(context.Context) error   { return nil }
func (a *settleAgent) Stop(context.Context) error  { return agent.ErrNotSupported }

func (a *settleAgent) RunOnce(ctx context.Context, _ agent.StartConfig, blocks []agent.ContentBlock, _ ...agent.RunOnceOption) (agent.RunResult, error) {
	if err := a.SendBlocks(ctx, blocks); err != nil {
		return agent.RunResult{}, err
	}
	return agent.RunResult{Text: "one-shot"}, nil
}

// Review returns findings without touching the chat session's agent
// — /review runs its review on separate one-shot sessions.
func (a *settleAgent) Review(_ context.Context, _ agent.StartConfig, _ ...agent.RunOnceOption) (agent.RunResult, error) {
	select {
	case a.bridge.reviewCalls <- struct{}{}:
	default:
	}
	return agent.RunResult{Text: "## Review\n\none finding"}, nil
}

func (a *settleAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		close(a.events)
	}
	return nil
}

var _ agent.Starter = (*settleAgent)(nil)

// settleDriver forwards driver calls back to a settleAgent.
type settleDriver struct{ inner *settleAgent }

func (d *settleDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *settleDriver) SendPermission(resp string) error { return d.inner.SendPermission(resp) }
func (d *settleDriver) Reset(ctx context.Context) error  { return d.inner.New(ctx) }
func (d *settleDriver) Stop(ctx context.Context) error   { return d.inner.Stop(ctx) }
func (d *settleDriver) Close() error                     { return d.inner.Close() }
func (d *settleDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// settleSpawner hands out agents that share one bridge.
type settleSpawner struct {
	bridge *settleBridge
	next   int
}

func (s *settleSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	s.next++
	a := newSettleAgent(s.bridge, 4200+s.next)
	return a.Start(context.Background(), agent.StartConfig{})
}

// ─── harness ────────────────────────────────────────────────────────

type settleHarness struct {
	echoCh *echo.Channel
	mgr    *chatsession.Manager
	router *inbound.Router
	cs     *chatsession.ChatSession
	bridge *settleBridge
	chatID string
}

// newSettleHarness wires the same components as cmd/nightme/run.go,
// with the stub agent in place of a real bridge.
func newSettleHarness(t *testing.T, factories ...command.SlashCommandFactory) *settleHarness {
	t.Helper()

	bridge := newSettleBridge()
	return newSettleHarnessWith(t, bridge, &settleSpawner{bridge: bridge}, factories...)
}

// newSettleHarnessWithSpawner is newSettleHarness with a caller
// supplied spawner — the real-pi smoke plugs a real bridge in here.
func newSettleHarnessWithSpawner(t *testing.T, sp chatsession.Spawner, factories ...command.SlashCommandFactory) *settleHarness {
	t.Helper()
	return newSettleHarnessWith(t, newSettleBridge(), sp, factories...)
}

func newSettleHarnessWith(t *testing.T, bridge *settleBridge, sp chatsession.Spawner, factories ...command.SlashCommandFactory) *settleHarness {
	t.Helper()

	echoCh := echo.New("echo", nil)

	mgr := chatsession.NewManager()
	mgr.WithSpawner(sp)
	mgr.WithEmitter(&testChannelWrap{ch: echoCh})

	reg := command.NewRegistry()
	for _, f := range factories {
		reg.Register(f)
	}

	noop := &gatewaytest.NoopEmitter{}
	router := inbound.New(
		mgr,
		newRuntimeShim(mgr, reg, "settle"),
		shell.NewDispatcher(),
		&e2eStubRouter{},
		noop,
		"settle",
	)

	const chatID = "oc_settle"
	cs, err := mgr.GetOrCreate(chatID, "settle")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	dir := t.TempDir()
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	// The commands that need an agent session spawn it themselves;
	// the ones that only queue a message rely on the runtime's
	// dispatcher having resolved it. Do it here so the harness
	// mirrors the production precondition.
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}

	// The event pump is what carries the prompt-end from the agent
	// session to the chat session's PromptEndBus.
	ctx, cancel := context.WithCancel(context.Background())
	go cs.PumpEvents(ctx)
	t.Cleanup(cancel)

	return &settleHarness{
		echoCh: echoCh,
		mgr:    mgr,
		router: router,
		cs:     cs,
		bridge: bridge,
		chatID: chatID,
	}
}

// drive sends one inbound message through the dispatch chain and
// waits for the async command dispatch to finish.
func (h *settleHarness) drive(t *testing.T, text, msgID string) {
	t.Helper()
	if _, err := h.router.Dispatch(context.Background(), h.mgr, &messages.InboundMessage{
		ChatID:    h.chatID,
		UserID:    "ou_settle",
		Text:      text,
		MessageID: msgID,
		Time:      time.Now(),
	}); err != nil {
		t.Fatalf("Dispatch(%q): %v", text, err)
	}
	h.router.WaitExec()
}

// settled returns the settled messages captured so far.
func (h *settleHarness) settled() []messages.OutboundMessage {
	var out []messages.OutboundMessage
	for _, m := range h.echoCh.Record() {
		if m.Kind == messages.OutMessageSettled {
			out = append(out, m)
		}
	}
	return out
}

// waitSettled waits until want settled messages have arrived.
func (h *settleHarness) waitSettled(t *testing.T, want int, within time.Duration) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := h.settled()
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("settled messages = %d, want %d (within %s)", len(got), want, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitSettledFor waits until the settled message for msgID arrives.
func (h *settleHarness) waitSettledFor(t *testing.T, msgID string, within time.Duration) messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, m := range h.settled() {
			if m.ReplyTo == msgID {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no settled message for %q within %s", msgID, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertOneSettled waits for the settled message of msgID and
// asserts that it is the ONLY one for that message.
//
// Every inbound message settles exactly once — a command that
// reports twice, or a hold that is never released, would show up
// here as a second (or missing) settled message.
func (h *settleHarness) assertOneSettled(t *testing.T, msgID string, within time.Duration) messages.OutboundMessage {
	t.Helper()
	got := h.waitSettledFor(t, msgID, within)
	var count int
	for _, m := range h.settled() {
		if m.ReplyTo == msgID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("settled messages for %q = %d, want exactly 1", msgID, count)
	}
	return got
}

// openTurnGate lets the in-flight turn end.
func (h *settleHarness) openTurnGate() { close(h.bridge.turnGate) }

// waitTurnStarted waits until the agent accepted a prompt.
func (h *settleHarness) waitTurnStarted(t *testing.T) {
	t.Helper()
	select {
	case <-h.bridge.turnStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the agent never received a prompt")
	}
}

// ─── tests ──────────────────────────────────────────────────────────

// TestSettle_QueuedTurn_SettlesOnlyAfterTheTurnEnds covers the
// shape most commands have: the command hands a message to the
// agent and returns. The agent work outlives the command, so the
// settled message must wait for the turn to end.
func TestSettle_QueuedTurn_SettlesOnlyAfterTheTurnEnds(t *testing.T) {
	h := newSettleHarness(t, queue.NewFactory())

	h.drive(t, "/queue hello", "om_settle_queue")
	h.waitTurnStarted(t)

	// The command is done, the agent turn is still running.
	if got := h.settled(); len(got) != 0 {
		t.Fatalf("settled emitted %d times while the turn was still running, want 0", len(got))
	}

	h.openTurnGate()

	got := h.waitSettled(t, 1, 3*time.Second)
	if got[0].ReplyTo != "om_settle_queue" {
		t.Errorf("settled ReplyTo = %q, want om_settle_queue", got[0].ReplyTo)
	}
	if got[0].Outcome == nil || got[0].Outcome.Status != messages.OutcomeOK {
		t.Errorf("settled outcome = %+v, want ok", got[0].Outcome)
	}
}

// TestSettle_Review_SettlesOnlyAfterTheInjectedTurnEnds covers the
// two-phase /review: the review itself finishes first, but the
// findings it hands to the chat's agent are a turn of their own and
// can run for a long time. The settled message must wait for that
// turn, not for the review.
func TestSettle_Review_SettlesOnlyAfterTheInjectedTurnEnds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NIGHTME_CONFIG", filepath.Join(dir, "config.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("primary: settle\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	// The review resolves its runner through agent.Builtins.
	orig := agent.Builtins
	agent.Builtins = agent.New()
	t.Cleanup(func() { agent.Builtins = orig })

	h := newSettleHarness(t, review.NewFactory())
	agent.Builtins.Register(newSettleAgent(h.bridge, 7777))

	h.drive(t, "/review", "om_settle_review")

	// The review ran.
	select {
	case <-h.bridge.reviewCalls:
	case <-time.After(3 * time.Second):
		t.Fatal("the review never ran")
	}

	// The review is done; the injected turn has not ended yet.
	h.waitTurnStarted(t)
	if got := h.settled(); len(got) != 0 {
		t.Fatalf("settled emitted %d times before the injected turn ended, want 0", len(got))
	}

	h.openTurnGate()

	got := h.assertOneSettled(t, "om_settle_review", 3*time.Second)
	if got.Outcome == nil || got.Outcome.Status != messages.OutcomeOK {
		t.Errorf("settled outcome = %+v, want ok", got.Outcome)
	}
}

// TestSettle_Review_FailedInjection_SettlesAnyway covers the
// injection failing: no turn will ever run, so the message must
// settle instead of waiting for a turn that never comes.
func TestSettle_Review_FailedInjection_SettlesAnyway(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NIGHTME_CONFIG", filepath.Join(dir, "config.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("primary: settle\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	orig := agent.Builtins
	agent.Builtins = agent.New()
	t.Cleanup(func() { agent.Builtins = orig })

	h := newSettleHarness(t, review.NewFactory())
	agent.Builtins.Register(newSettleAgent(h.bridge, 7778))

	// The injection into the chat's agent fails.
	h.bridge.mu.Lock()
	h.bridge.failSend = true
	h.bridge.mu.Unlock()

	h.drive(t, "/review", "om_settle_review_fail")

	h.assertOneSettled(t, "om_settle_review_fail", 3*time.Second)
}

// TestSettle_StoppedTurn_SettlesAsCancelled covers /stop: the turn
// it interrupts belongs to an EARLIER inbound message, and that
// message settles as cancelled. The /stop command's own message
// settles on its own.
func TestSettle_StoppedTurn_SettlesAsCancelled(t *testing.T) {
	h := newSettleHarness(t, queue.NewFactory(), stop.NewFactory())

	// A queued message starts a turn that stays in flight.
	h.drive(t, "/queue hello", "om_settle_queued")
	h.waitTurnStarted(t)

	// /stop ends that turn.
	h.drive(t, "/stop", "om_settle_stop")

	// The STOPPED turn belongs to the queued message, and it settles
	// as cancelled. The /stop command's own message settles on its
	// own — it is a different inbound.
	queued := h.waitSettledFor(t, "om_settle_queued", 3*time.Second)
	if queued.Outcome == nil || queued.Outcome.Status != messages.OutcomeCancelled {
		t.Errorf("settled outcome = %+v, want cancelled", queued.Outcome)
	}
}

// TestSettle_CommandsWithoutAgentWork_SettleImmediately covers the
// commands that start no agent work at all: their message is done
// when the command returns, and the settled message says so without
// waiting for anything.
//
// This is also the cheapest net against a command that hangs: a
// command that never settles would blow the wait below.
//
// /cwd reports its own outcome (the workspace it set); the mode
// switches still report nothing, so they settle as unknown.
func TestSettle_CommandsWithoutAgentWork_SettleImmediately(t *testing.T) {
	h := newSettleHarness(t,
		think.NewFactory(),
		tools.NewFactory(),
		watch.NewFactory(),
		cwd.NewFactory(),
	)

	cases := []struct {
		text   string
		msgID  string
		status messages.OutcomeStatus
	}{
		{"/think", "om_settle_think", messages.OutcomeUnknown},
		{"/tools", "om_settle_tools", messages.OutcomeUnknown},
		{"/watch", "om_settle_watch", messages.OutcomeUnknown},
		{"/cwd " + h.cs.SelectedCwd(), "om_settle_cwd", messages.OutcomeOK},
	}

	for _, tc := range cases {
		h.drive(t, tc.text, tc.msgID)
		got := h.assertOneSettled(t, tc.msgID, 3*time.Second)
		if got.Outcome == nil || got.Outcome.Status != tc.status {
			t.Errorf("%s: settled outcome = %+v, want %s", tc.text, got.Outcome, tc.status)
		}
	}
}

// TestSettle_Steer_SettlesWhenTheSteeredTurnEnds covers /steer: the
// steered text rides the NEXT turn, so the message must not settle
// when the command returns — the agent has not seen the text yet.
//
// The steered text is queued at the head; the next command that
// queues a message flushes the batch, and the turn that carries both
// ends the steered message's work.
func TestSettle_Steer_SettlesWhenTheSteeredTurnEnds(t *testing.T) {
	h := newSettleHarness(t, steer.NewFactory(), queue.NewFactory())

	h.drive(t, "/steer keep going", "om_settle_steer")

	// The command is done; the steered text has not reached the
	// agent yet.
	if got := h.settled(); len(got) != 0 {
		t.Fatalf("settled emitted %d times before the steered turn ran, want 0", len(got))
	}
	select {
	case <-h.bridge.turnStarted:
		t.Error("a turn started for the steered text; the queue should still hold it")
	default:
	}

	// The next queued message flushes the batch the steered text is
	// part of.
	h.drive(t, "/queue hello", "om_settle_after_steer")
	h.waitTurnStarted(t)

	// The turn is in flight — still no settled for the steer.
	if got := h.settled(); len(got) != 0 {
		t.Fatalf("settled emitted %d times while the steered turn was running, want 0", len(got))
	}

	h.openTurnGate()

	got := h.waitSettledFor(t, "om_settle_steer", 3*time.Second)
	if got.Outcome == nil || got.Outcome.Status != messages.OutcomeOK {
		t.Errorf("settled outcome = %+v, want ok", got.Outcome)
	}
}
