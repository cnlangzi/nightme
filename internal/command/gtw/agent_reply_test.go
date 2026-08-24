// Tests for the EventSink / HeartbeatTracker wiring inside
// runAgentFor. F-CODEX-RUNONCE-REVIEW-EVENT made the same
// StreamRunOnceToEmitter → dispatchSinkEvent → Heartbeat.Observe
// pipeline available to /gtw commit and /gtw pr; these tests
// guard that contract so a future refactor of either layer
// (emitter_sink.go or agent_reply.go) can't silently break the
// gtw path's live-stream + heartbeat counters.
//
// Three cases, each small and deterministic:
//   - TestRunAgentFor_SinkInstalled: sink callback is wired into
//     RunOnce; the bridge fires it for every AgentEvent.
//   - TestRunAgentFor_HeartbeatObserved: HeartbeatTracker
//     counters + OutHeartbeat emission both work end-to-end.
//   - TestRunAgentFor_Filtering: the Translate contract (text →
//     OutReply, thinking → OutThinking, tool → OutToolStart,
//     result → OutResult) is preserved on the gtw path.
//
// Why a separate eventEmitterStarter instead of reusing
// hooks_test.go's testStarter: that one returns nil sink; we
// need to drive the bridge-side drain loop with a captured
// callback to assert the contract. Keep it small and
// self-contained so the file is easy to delete if the
// underlying pipeline is rewritten.

package gtw

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// eventEmitterStarter is a Starter fake that, when RunOnce is
// invoked, captures the per-call sink callback (from
// agent.ParseRunOnceOptions) and pumps the configured AgentEvents
// through it. Mirrors what a real bridge (dsh / codex / pi / …)
// does on its drain goroutine: emit per-turn events synchronously
// to the sink, return RunResult.
type eventEmitterStarter struct {
	name string

	mu          sync.Mutex
	events      []agent.AgentEvent
	captured    func(agent.AgentEvent)
	runOnceText string
	runOnceErr  error

	// callCounts is exposed for the sink-installed test: counts
	// how many times the bridge drain (sink) was invoked.
	sinkCalls int
}

func (s *eventEmitterStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModePTY, "fake-"+s.name, nil, nil)
}
func (s *eventEmitterStarter) Detect() error { return nil }
func (s *eventEmitterStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("eventEmitterStarter: Start not implemented")
}
func (s *eventEmitterStarter) RunOnce(_ context.Context, cfg agent.StartConfig, _ []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	s.mu.Lock()
	cfgOpts := agent.ParseRunOnceOptions(opts)
	s.captured = cfgOpts.OnEvent
	events := append([]agent.AgentEvent(nil), s.events...)
	s.mu.Unlock()

	for _, ev := range events {
		if s.captured != nil {
			s.mu.Lock()
			s.sinkCalls++
			s.mu.Unlock()
			s.captured(ev)
		}
	}
	return agent.RunResult{Text: s.runOnceText}, s.runOnceErr
}
func (s *eventEmitterStarter) Review(_ context.Context, _ agent.StartConfig, _ ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, agent.ErrReviewNotSupported
}

// recSinkCalls returns the number of times the bridge sink was
// invoked during the last RunOnce. Read-only — no lock copy
// needed for the test's monotonic-counter assertion (each test
// drives one RunOnce then reads once).
func (s *eventEmitterStarter) recSinkCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sinkCalls
}

// sinkCapture is the gtw-package test emitter used by the
// existing commit / pr test suites. Reimplemented locally so
// the agent_reply tests stay self-contained — capturing the
// minimal surface (Sent + Send) without pulling in any
// channel-specific behaviour.
type sinkCapture struct {
	mu  sync.Mutex
	sent []messages.OutboundMessage
}

func (c *sinkCapture) Send(_ context.Context, msg messages.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return nil
}

func (c *sinkCapture) snapshot() []messages.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]messages.OutboundMessage, len(c.sent))
	copy(out, c.sent)
	return out
}

func (c *sinkCapture) heartbeatMsgs() []messages.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []messages.OutboundMessage
	for _, m := range c.sent {
		if m.Kind == messages.OutHeartbeat {
			out = append(out, m)
		}
	}
	return out
}

// newSinkTestRig wires the per-test scaffolding: a fresh
// ChatSession (constructed via chatsession.New so Heartbeat is
// non-nil), a capture emitter bound to it, and the supplied
// starter swapped into agent.Builtins.
//
// Think and Tools are both turned ON (the default per
// chatsession.New is Hide for both). Tests want to observe the
// full event stream reaching the emitter, including tool calls
// and thinking — turning them off would let the policy gate
// drop the very events the assertions check for.
func newSinkTestRig(t *testing.T, starter *eventEmitterStarter) (*chatsession.ChatSession, *sinkCapture) {
	t.Helper()
	cs, err := chatsession.New("chat-test", starter.name)
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	if err := cs.SetSelectedAgent(starter.name); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}
	// csStore is nil in this rig (no persistence), so
	// SetToolsMode / SetThinkMode just mutate the field
	// without trying to write through to disk.
	if err := cs.SetToolsMode(chatsession.ToolsModeShow); err != nil {
		t.Fatalf("SetToolsMode(Show): %v", err)
	}
	if err := cs.SetThinkMode(chatsession.ThinkModeShow); err != nil {
		t.Fatalf("SetThinkMode(Show): %v", err)
	}
	ch := &sinkCapture{}
	cs.WithEmitter(ch)
	withSinkAgent(t, starter)
	return cs, ch
}

// withSinkAgent is a tiny copy of push_test.go's withAgent that
// targets the local eventEmitterStarter type. Keeping it local
// avoids importing a starter-kind that push_test.go's
// recordingAgent doesn't expose.
func withSinkAgent(t *testing.T, starter *eventEmitterStarter) {
	t.Helper()
	orig := agent.Builtins
	clean := agent.New()
	clean.Register(starter)
	agent.Builtins = clean
	t.Cleanup(func() { agent.Builtins = orig })
}

// waitForSent polls sinkCapture until at least n messages have
// arrived or the deadline expires. The drain goroutine inside
// StreamRunOnceToEmitter decouples bridge emission from emitter
// delivery — runAgentFor returning only means RunOnce returned,
// not that the sink's events have finished draining. Polling
// is the cheapest deterministic barrier for these tests.
func (c *sinkCapture) waitForSent(n int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(c.snapshot()) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestRunAgentFor_SinkInstalled asserts the gtw path hands a
// non-nil sink callback to the bridge (via WithEventSink) and
// the bridge drain calls it for every AgentEvent it would have
// emitted. This is the basic "wire is connected" check —
// without it, the gtw dispatchers would silently drop every
// intermediate event while still returning RunResult.Text.
func TestRunAgentFor_SinkInstalled(t *testing.T) {
	starter := &eventEmitterStarter{
		name: "sink-installed",
		events: []agent.AgentEvent{
			{Kind: agent.EventAgentText, Text: "hello"},
			{Kind: agent.EventAgentText, Text: "world"},
			{Kind: agent.EventAgentResult, Result: &agent.AgentResultEvent{Text: "ok"}},
		},
		runOnceText: "ok",
	}
	cs, _ := newSinkTestRig(t, starter)

	res, agentName, err := runAgentFor(
		context.Background(), cs, t.TempDir(),
		"prompt", "chat-test", "msg-test",
		"", "", // cliAgent, ymlAgent — let ResolveAgent pick from SelectedAgent
	)
	if err != nil {
		t.Fatalf("runAgentFor: %v", err)
	}
	if agentName != "sink-installed" {
		t.Fatalf("agentName = %q, want sink-installed", agentName)
	}
	if res.Text != "ok" {
		t.Fatalf("RunResult.Text = %q, want ok", res.Text)
	}

	// Bridge drain invoked the sink once per AgentEvent.
	if got := starter.recSinkCalls(); got != 3 {
		t.Errorf("sink callback called %d times, want 3", got)
	}
}

// TestRunAgentFor_HeartbeatObserved asserts the F-63
// HeartbeatTracker is bumped on every OutToolStart / OutThinking
// the dispatcher emits, and the auto-emitted OutHeartbeat
// carries the latest snapshot. This is the "user sees the
// agent working" guarantee — without it, /gtw commit / /gtw pr
// would show a static receipt card with no progress signal
// while the agent's subprocess grinds through tools.
func TestRunAgentFor_HeartbeatObserved(t *testing.T) {
	starter := &eventEmitterStarter{
		name: "heartbeat-observed",
		events: []agent.AgentEvent{
			{Kind: agent.EventAgentText, Text: "plain"}, // OutReply — no counter bump
			{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: "t1", Name: "Bash", Args: "ls"}},
			{Kind: agent.EventAgentToolEnd, ToolEnd: &agent.AgentToolEndEvent{ID: "t1", Name: "Bash"}},
			// Thinking text in Claude Code wire format carries the
			// ThinkingPrefix marker ("[思考] ", defined in
			// internal/gateway/outbound/translate.go); gateway.Translate
			// recognises it and maps to OutThinking.
			{Kind: agent.EventAgentText, Text: "[思考] reasoning about the tool output"},
			{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: "t2", Name: "Read", Args: "f.go"}},
			{Kind: agent.EventAgentResult, Result: &agent.AgentResultEvent{Text: "done"}},
		},
		runOnceText: "done",
	}
	cs, ch := newSinkTestRig(t, starter)

	_, _, err := runAgentFor(
		context.Background(), cs, t.TempDir(),
		"prompt", "chat-test", "msg-test", "", "",
	)
	if err != nil {
		t.Fatalf("runAgentFor: %v", err)
	}

	// Wait for drain — we expect 6 translated messages + at
	// least 3 OutHeartbeat follow-ups (one per counter change:
	// ToolStart, OutThinking, ToolStart). Counter increments
	// happen BEFORE the policy gate, so even if a future change
	// hides a kind, the heartbeat counter would still track it.
	want := 9
	if !ch.waitForSent(want, 2*time.Second) {
		t.Fatalf("emitter never received %d messages; got %d sent",
			want, len(ch.snapshot()))
	}

	// Heartbeat counters reflect real activity: 2 tool starts +
	// 1 thinking = ThinkCount 1, ToolCount 2.
	snap := cs.Heartbeat().Snapshot("msg-test")
	if snap.ThinkCount != 1 {
		t.Errorf("ThinkCount = %d, want 1", snap.ThinkCount)
	}
	if snap.ToolCount != 2 {
		t.Errorf("ToolCount = %d, want 2", snap.ToolCount)
	}
	if snap.LastBeatAt.IsZero() {
		t.Errorf("LastBeatAt must be refreshed even on non-counter events")
	}

	// At least one OutHeartbeat was emitted to the channel
	// (the counter changes mean dispatchSinkEvent fires the
	// follow-up). 3 changes (ToolStart, Thinking, ToolStart) →
	// up to 3 OutHeartbeat messages; we don't pin the exact
	// count because Observe's "changed" return trips per-event
	// and the policy gate doesn't drop heartbeat emits.
	hbs := ch.heartbeatMsgs()
	if len(hbs) == 0 {
		t.Fatalf("expected at least one OutHeartbeat, got 0; sent=%+v",
			ch.snapshot())
	}
	for _, hb := range hbs {
		if hb.Heartbeat == nil {
			t.Errorf("OutHeartbeat missing Heartbeat snapshot: %+v", hb)
			continue
		}
		if hb.Heartbeat.Empty() {
			t.Errorf("OutHeartbeat snapshot is Empty — that should never reach the channel")
		}
	}
}

// TestRunAgentFor_Filtering locks down the per-kind Translate
// mapping as observed from the gtw sink. /gtw commit / /gtw pr
// depend on these kinds reaching the channel adapter intact:
// OutReply drives the rolling-log; OutToolStart / OutToolEnd
// drive the tool call feed; OutResult carries the final reply
// (with usage); OutThinking drives the thinking feed.
func TestRunAgentFor_Filtering(t *testing.T) {
	starter := &eventEmitterStarter{
		name: "filtering",
		events: []agent.AgentEvent{
			{Kind: agent.EventAgentText, Text: "first reply"},
			{Kind: agent.EventAgentText, Text: "[思考] step by step reasoning"},
			{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: "t1", Name: "Edit", Args: "go.mod"}},
			{Kind: agent.EventAgentToolEnd, ToolEnd: &agent.AgentToolEndEvent{ID: "t1", Name: "Edit", Output: "ok"}},
			{Kind: agent.EventAgentResult, Result: &agent.AgentResultEvent{Text: "final"}},
		},
		runOnceText: "final",
	}
	cs, ch := newSinkTestRig(t, starter)

	_, _, err := runAgentFor(
		context.Background(), cs, t.TempDir(),
		"prompt", "chat-test", "msg-test", "", "",
	)
	if err != nil {
		t.Fatalf("runAgentFor: %v", err)
	}
	// 5 primary messages + 2 OutHeartbeat follow-ups (one each
	// for the OutToolStart and OutThinking counter increments).
	if !ch.waitForSent(7, 2*time.Second) {
		t.Fatalf("emitter never received 7 messages; got %d: %+v",
			len(ch.snapshot()), ch.snapshot())
	}

	got := ch.snapshot()
	// We only check the FIRST occurrence of each non-heartbeat
	// kind — heartbeat follow-ups interleave with the primary
	// stream, and asserting exact position would couple the
	// test to the precise Observe-then-Send order. The point of
	// this test is "the right primary kinds reach the emitter",
	// not "they arrive in this exact order".
	wantKinds := []messages.OutboundKind{
		messages.OutReply,
		messages.OutThinking,
		messages.OutToolStart,
		messages.OutToolEnd,
		messages.OutResult,
	}
	primaryKinds := make([]messages.OutboundKind, 0, len(wantKinds))
	for _, m := range got {
		if m.Kind == messages.OutHeartbeat {
			continue
		}
		primaryKinds = append(primaryKinds, m.Kind)
	}
	if len(primaryKinds) != len(wantKinds) {
		t.Fatalf("primary messages: got %d, want %d (all=%+v)",
			len(primaryKinds), len(wantKinds), got)
	}
	for i, want := range wantKinds {
		if primaryKinds[i] != want {
			t.Errorf("primary[%d].Kind = %v, want %v", i, primaryKinds[i], want)
		}
	}

	// Locate the OutResult (final primary message) and check
	// it carries the final text — Translate copies it verbatim
	// onto the OutboundMessage so the channel can render the
	// dedicated "📝 final" card.
	var resultMsg *messages.OutboundMessage
	for i := range got {
		if got[i].Kind == messages.OutResult {
			resultMsg = &got[i]
			break
		}
	}
	if resultMsg == nil {
		t.Fatalf("no OutResult in sent=%+v", got)
	}
	if resultMsg.Text != "final" {
		t.Errorf("OutResult.Text = %q, want final", resultMsg.Text)
	}
}

// TestRunAgentFor_SinkNilEmitter verifies the nil-emitter
// short-circuit. StreamRunOnceToEmitter returns a no-op sink
// when em is nil (so the bridge's drain loop never blocks on
// a missing channel). This test pins that contract: a gtw
// dispatcher invoked without a wired Emitter must still
// return RunResult, not error out.
func TestRunAgentFor_SinkNilEmitter(t *testing.T) {
	starter := &eventEmitterStarter{
		name: "nil-emitter",
		events: []agent.AgentEvent{
			{Kind: agent.EventAgentText, Text: "ignored"},
		},
		runOnceText: "ignored",
	}
	withSinkAgent(t, starter)

	cs, err := chatsession.New("chat-nil-emit", starter.name)
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	_ = cs.SetSelectedAgent(starter.name)
	// Deliberately do NOT bind an emitter — cs.Emitter() == nil.

	// slog.Default() inside StreamRunOnceToEmitter must not
	// panic on the nil-cs + nil-emitter combination.
	_ = slog.Default()

	res, agentName, err := runAgentFor(
		context.Background(), cs, t.TempDir(),
		"prompt", "chat-nil-emit", "msg-nil-emit", "", "",
	)
	if err != nil {
		t.Fatalf("runAgentFor with nil emitter: %v", err)
	}
	if agentName != "nil-emitter" {
		t.Errorf("agentName = %q, want nil-emitter", agentName)
	}
	if res.Text != "ignored" {
		t.Errorf("res.Text = %q, want ignored", res.Text)
	}
}