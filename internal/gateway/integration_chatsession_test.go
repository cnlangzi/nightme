package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/claudecode"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// T-alive: end-to-end integration test that reproduces the
// "AgentSession events never reach the channel" regression
// observed on 2026-08-07 with feishu + claudecode.
//
// Wiring (mirrors cmd/nightme/run.go::wireRuntimeCallbacksAndRestore
// + the runtime's EventHandler closure):
//
//   ChatSession.SetEventHandler(translate + ch.Send)
//   ChatSession.PumpEvents(ctx)        // consumes as.Events()
//   AgentSession.Spawn(fakeSpawner)    // wires fake bridge handle
//   AgentSession.readpumpLoop()        // reads handle.Events() →
//                                      // pushes EnrichedEvent to as.eventQueue
//   AgentSession.Submit(prompt)        // drives bridge via SendBlocks
//   fake.PushEvent(EventText "hello")  // simulates agent response
//   assert mock Channel.Record() sees OutReply{ChatID, ReplyTo, Text}
//
// This skips feishu / claudecode / --resume / persistence entirely:
// every component on the outbound path is exercised, but with a
// fake bridge + a mock channel that just records the OutboundMessages
// it receives. If any link on the path is broken, the assertion
// fails with the precise (empty ChatID / empty ReplyTo / missing
// kind) signal needed to localize the regression.

// --- mock Channel ----------------------------------------------------

type recordingChannel struct {
	mu      sync.Mutex
	chatID  string
	captured []OutboundMessage
}

func (c *recordingChannel) Name() string  { return "mock" }
func (c *recordingChannel) Start(_ context.Context) error { return nil }
func (c *recordingChannel) Stop(_ context.Context) error  { return nil }
func (c *recordingChannel) Incoming() <-chan InboundMessage {
	ch := make(chan InboundMessage, 1)
	close(ch)
	return ch
}
func (c *recordingChannel) Send(_ context.Context, msg OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = append(c.captured, msg)
	return nil
}
func (c *recordingChannel) SendCard(_ context.Context, msg OutboundMessage) (string, error) {
	_ = c.Send(context.Background(), msg)
	return "mock-card-0", nil
}
func (c *recordingChannel) Record() []OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]OutboundMessage, len(c.captured))
	copy(out, c.captured)
	return out
}

var _ Channel = (*recordingChannel)(nil)

// --- minimal runtime handler -----------------------------------------
//
// Mirrors newEventHandler's core (Translate + ReplyTo stamping +
// ch.Send) without the SessionContext / /think / /tools side-paths
// — those are not relevant to the regression we're hunting.

func integrationEventHandler(ch Channel, _ *chatsession.ChatSession) chatsession.EventHandler {
	return func(chatID string, _ *chatsession.AgentSession, ev agent.AgentEvent, userMsgID string) {
		out, ok := Translate(chatID, ev)
		if !ok {
			return
		}
		out.ReplyTo = userMsgID
		_ = ch.Send(context.Background(), out)
	}
}

// --- helpers ----------------------------------------------------------

func newIntegrationChatSession(chatID string, spawner chatsession.Spawner) *chatsession.ChatSession {
	cs := chatsession.New(chatID, "fake").WithSpawner(spawner)
	cs.SetActiveCwd("/tmp")
	cs.SetActiveAgent("fake")
	return cs
}

// --- tests -----------------------------------------------------------

// TestIntegration_AgentEvent_ReachesChannel is the smallest
// reproduction: enqueue a message, submit it to the AS, push an
// EventText from the fake bridge, assert OutReply lands on the
// channel with the correct chat_id + reply_to + text.
func TestIntegration_AgentEvent_ReachesChannel(t *testing.T) {
	spawner := &integrationSpawner{}
	cs := newIntegrationChatSession("oc_test_chat", spawner)

	// Recording channel + runtime handler (same shape as the
	// production wireRuntimeCallbacksAndRestore onCreate).
	mock := &recordingChannel{chatID: cs.ChatID}
	cs.SetEventHandler(integrationEventHandler(mock, cs))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.PumpEvents(ctx)

	// Trigger Spawn (drives AgentSession.Spawn → fakeSpawner.Spawn →
	// fakeAgentSession becomes the handle → startReadPump launches).
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	defer as.Shutdown()
	fake := spawner.lastFake
	if fake == nil {
		t.Fatal("spawner did not capture fakeAgentSession")
	}

	// Drive the queue + submit the prompt (real production path).
	const userMsgID = "om_msg_1"
	msg := &chatsession.Message{
		ID:     userMsgID,
		ChatID: cs.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hi agent"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	// Fake bridge streams a reply chunk.
	fake.PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "hello back"})

	// Wait for the round-trip.
	var rec []OutboundMessage
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec = mock.Record()
		if len(rec) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rec) == 0 {
		t.Fatal("no OutboundMessage captured on channel within 2s — AgentEvent never reached ch.Send")
	}

	// Find the OutReply specifically (other Kinds may also fire —
	// MessageState events come through a different path).
	var outReply *OutboundMessage
	for i := range rec {
		if rec[i].Kind == OutReply {
			outReply = &rec[i]
			break
		}
	}
	if outReply == nil {
		t.Fatalf("no OutReply in captured messages (got %d total, kinds: %v)",
			len(rec), summarizeKinds(rec))
	}

	// LastMessageID is the receipt-card anchor — must equal the
	// userMsgID we queued. If empty, the placeholder card on
	// feishu would never be created (see T-alive root cause).
	if outReply.ReplyTo != userMsgID {
		t.Errorf("OutReply.ReplyTo = %q, want %q (placeholder-card anchor broken)",
			outReply.ReplyTo, userMsgID)
	}
	if outReply.ChatID != cs.ChatID {
		t.Errorf("OutReply.ChatID = %q, want %q", outReply.ChatID, cs.ChatID)
	}
	if outReply.Text != "hello back" {
		t.Errorf("OutReply.Text = %q, want %q", outReply.Text, "hello back")
	}

	// Tell the test we're done; Shutdown triggers a KindPromptEnded
	// that would normally fire onPromptEnd.
	fake.FinishEvent()
}

// TestIntegration_AgentEventResult_ReachesChannel exercises the
// final-result path (EventResult → OutResult). This is the second
// half of the "no OutReply/OutResult to feishu" symptom.
func TestIntegration_AgentEventResult_ReachesChannel(t *testing.T) {
	spawner := &integrationSpawner{}
	cs := newIntegrationChatSession("oc_test_chat", spawner)

	mock := &recordingChannel{chatID: cs.ChatID}
	cs.SetEventHandler(integrationEventHandler(mock, cs))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.PumpEvents(ctx)

	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	defer as.Shutdown()
	fake := spawner.lastFake
	if fake == nil {
		t.Fatal("spawner did not capture fakeAgentSession")
	}

	const userMsgID = "om_msg_2"
	msg := &chatsession.Message{
		ID:     userMsgID,
		ChatID: cs.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "go"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	fake.PushEvent(agent.AgentEvent{
		Kind: agent.EventResult,
		Result: &agent.ResultEvent{
			Text: "final answer",
		},
	})

	var rec []OutboundMessage
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec = mock.Record()
		if len(rec) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rec) == 0 {
		t.Fatal("no OutboundMessage captured on channel within 2s — EventResult never reached ch.Send")
	}

	var outResult *OutboundMessage
	for i := range rec {
		if rec[i].Kind == OutResult {
			outResult = &rec[i]
			break
		}
	}
	if outResult == nil {
		t.Fatalf("no OutResult in captured messages (got %d total, kinds: %v)",
			len(rec), summarizeKinds(rec))
	}
	if outResult.ReplyTo != userMsgID {
		t.Errorf("OutResult.ReplyTo = %q, want %q", outResult.ReplyTo, userMsgID)
	}
	if outResult.Text != "final answer" {
		t.Errorf("OutResult.Text = %q, want %q", outResult.Text, "final answer")
	}

	fake.FinishEvent()
}

// --- shared Spawner fake ---------------------------------------------

// integrationSpawner wraps a fakeAgentSession (chatsession.fakeAgentSession
// is unexported, so we mirror its surface here). Tests retrieve the
// last fake via lastFake to drive PushEvent/FinishEvent.
type integrationSpawner struct {
	mu       sync.Mutex
	lastFake *integrationFake
	calls    int
}

func (s *integrationSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (agent.AgentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	fake := newIntegrationFake(99000 + s.calls)
	s.lastFake = fake
	return fake, nil
}

// integrationFake is a minimal agent.AgentSession that captures
// every PushEvent so the test can drive the bridge.
type integrationFake struct {
	mu     sync.Mutex
	pid    int
	events chan agent.AgentEvent
	closed bool
}

func newIntegrationFake(pid int) *integrationFake {
	return &integrationFake{pid: pid, events: make(chan agent.AgentEvent, 32)}
}

func (f *integrationFake) Events() <-chan agent.AgentEvent { return f.events }
func (f *integrationFake) PID() int                       { return f.pid }
func (f *integrationFake) SendText(string) error          { return nil }
func (f *integrationFake) SendBlocks(context.Context, []agent.ContentBlock) error {
	return nil
}
func (f *integrationFake) SendPermission(string) error { return nil }
func (f *integrationFake) New(context.Context) error   { return nil }
func (f *integrationFake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}
func (f *integrationFake) PushEvent(ev agent.AgentEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- ev
}
func (f *integrationFake) FinishEvent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- agent.AgentEvent{Kind: agent.EventDone}
	f.closed = true
	close(f.events)
}

var _ agent.AgentSession = (*integrationFake)(nil)

// --- helpers ----------------------------------------------------------

func summarizeKinds(msgs []OutboundMessage) []OutboundKind {
	out := make([]OutboundKind, len(msgs))
	for i, m := range msgs {
		out[i] = m.Kind
	}
	return out
}

// --- bridge integration (real claudecode bridge, fake shell script) ---

// TestIntegration_RealBridge_FakeShell covers the layer the
// in-memory integrationFake skips: the claudecode bridge's
// pumpStream(stdout → s.events). It spawns a shell script via
// claudecode.New(...).Start() that emits a stream-json transcript
// on stdout, then drives the full Submit → readpump → PumpEvents
// → eventHandler chain. If pumpStream is broken (closed before
// first read, wrong channel buffering, parse failure), this test
// fails with a clear signal.
//
// The shell script writes:
//   1. system/init             → EventAgentConnected
//   2. assistant message       → EventText "hello back"
//   3. result                  → EventResult "final answer"
//   4. EOF (exit 0)            → pumpStream closes events
func TestIntegration_RealBridge_FakeShell(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude.sh")
	// Shell script: emit system/init + assistant + result, then exit.
	// `read` is added so the bridge's stdin write doesn't fail with
	// EPIPE before we finish emitting (the script exits after one
	// read attempt with a 1s timeout — bridge's SendBlocks returns
	// once the prompt is written, so the script just needs to drain
	// a line).
	body := `#!/bin/bash
set -e
echo '{"type":"system","subtype":"init","session_id":"test-session-1","model":"claude-test","cwd":"/tmp"}'
echo '{"type":"assistant","message":{"id":"msg_1","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hello back"}]}}'
echo '{"type":"result","result":"final answer","duration_ms":100,"is_error":false}'
# Drain one line of stdin (the bridge's SendBlocks prompt) so the
# pipe doesn't EPIPE before we finish emitting. 1s timeout so we
# don't hang the test.
read -t 1 _LINE || true
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Spawner that drives the real claudecode bridge against our
	// fake shell. registrySpawner calls Agent.Start which calls
	// newSession which exec's our script.
	realAgent := claudecode.New("claude", script, nil)
	spawner := &realBridgeSpawner{agent: realAgent}

	cs := newIntegrationChatSession("oc_real_bridge", spawner)

	mock := &recordingChannel{chatID: cs.ChatID}
	cs.SetEventHandler(integrationEventHandler(mock, cs))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.PumpEvents(ctx)

	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	defer as.Shutdown()

	const userMsgID = "om_real_bridge_1"
	msg := &chatsession.Message{
		ID:     userMsgID,
		ChatID: cs.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "go"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	// Wait for the round-trip: at minimum we expect OutReply +
	// OutResult to reach the channel.
	var rec []OutboundMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec = mock.Record()
		if len(rec) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rec) == 0 {
		t.Fatal("no OutboundMessage captured within 5s — bridge did not produce events end-to-end")
	}

	// Find OutReply + OutResult.
	var outReply, outResult *OutboundMessage
	for i := range rec {
		switch rec[i].Kind {
		case OutReply:
			outReply = &rec[i]
		case OutResult:
			outResult = &rec[i]
		}
	}
	if outReply == nil {
		t.Errorf("no OutReply in captured messages (kinds: %v)", summarizeKinds(rec))
	} else if outReply.ReplyTo != userMsgID {
		t.Errorf("OutReply.ReplyTo = %q, want %q", outReply.ReplyTo, userMsgID)
	} else if outReply.Text != "hello back" {
		t.Errorf("OutReply.Text = %q, want %q", outReply.Text, "hello back")
	}
	if outResult == nil {
		t.Errorf("no OutResult in captured messages (kinds: %v)", summarizeKinds(rec))
	} else if outResult.Text != "final answer" {
		t.Errorf("OutResult.Text = %q, want %q", outResult.Text, "final answer")
	}
}

// realBridgeSpawner wraps a real claudecode.Agent (whose Start
// spawns an actual subprocess via exec.Command).
type realBridgeSpawner struct {
	agent *claudecode.Agent
}

func (s *realBridgeSpawner) Spawn(ctx context.Context, _, _ string, args []string, resumeID string) (agent.AgentSession, error) {
	return s.agent.Start(ctx, agent.StartConfig{
		Workspace: "/tmp",
		Args:      args,
		ResumeID:  resumeID,
	})
}
