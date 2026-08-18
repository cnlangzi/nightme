//go:build !windows

package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/claudecode"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
)

// fakeCh is a minimal outbound.Emitter for tests in this package.

// T-alive: end-to-end integration test that reproduces the
// "AgentSession events never reach the channel" regression
// observed on 2026-08-07 with feishu + claudecode.
//
// Wiring (mirrors cmd/nightme/run.go::wireRuntimeCallbacksAndRestore
// + the runtime's EventHandler closure):
//
//   ChatSession.AgentEventBus().Subscribe(translate + ch.Send)
//   ChatSession.PumpEvents(ctx)        // consumes as.Events()
//   AgentSession.Spawn(fakeSpawner)    // wires fake bridge handle
//   AgentSession.readpumpLoop()        // reads handle.Events() →
//                                      // pushes EnrichedEvent to as.eventQueue
//   AgentSession.Submit(prompt)        // drives bridge via SendBlocks
//   fake.PushEvent(EventAgentText "hello")  // simulates agent response
//   assert mock channel.Record() sees an OutReply{ChatID, ReplyTo, Text}
//
// This skips feishu / claudecode / --resume / persistence entirely:
// every component on the outbound path is exercised, but with a
// fake bridge + a mock channel that just records the OutboundMessages
// it receives. If any link on the path is broken, the assertion
// fails with the precise (empty ChatID / empty ReplyTo / missing
// kind) signal needed to localize the regression.

// --- mock channel.Channel ----------------------------------------------------

type recordingChannel struct {
	mu      sync.Mutex
	chatID  string
	captured []messages.OutboundMessage
}

func (c *recordingChannel) Name() string  { return "mock" }
func (c *recordingChannel) Start(_ context.Context) error { return nil }
func (c *recordingChannel) Stop(_ context.Context) error  { return nil }
func (c *recordingChannel) Incoming() <-chan messages.InboundMessage {
	ch := make(chan messages.InboundMessage, 1)
	close(ch)
	return ch
}
func (c *recordingChannel) Send(_ context.Context, msg messages.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = append(c.captured, msg)
	return nil
}
func (c *recordingChannel) SendCard(_ context.Context, msg messages.OutboundMessage) (string, error) {
	_ = c.Send(context.Background(), msg)
	return "mock-card-0", nil
}
func (c *recordingChannel) Record() []messages.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]messages.OutboundMessage, len(c.captured))
	copy(out, c.captured)
	return out
}

// Channel-interface extensions (Phase 2.1 + 2.2). recordingChannel
// has no live state — all four are trivial fallbacks.
func (c *recordingChannel) OnPromptEnded(_ context.Context, _, _ string)        {}
func (c *recordingChannel) HealthSnapshot() (string, json.RawMessage, error) {
	return "mock", json.RawMessage("{}"), nil
}
func (c *recordingChannel) SetLogger(_ *slog.Logger) {}
func (c *recordingChannel) BuildBlocks(text string, _ []messages.Attachment) []agent.ContentBlock {
	if text == "" {
		return nil
	}
	return []agent.ContentBlock{{Type: agent.ContentText, Text: text}}
}

var _ channel.Channel = (*recordingChannel)(nil)

// --- minimal runtime handler -----------------------------------------
//
// Mirrors newEventHandler's core (Translate + ReplyTo stamping +
// ch.Send) without the StatusBar / /think / /tools side-paths
// — those are not relevant to the regression we're hunting.

func integrationEventHandler(ch channel.Channel, _ *chatsession.ChatSession) func(env chatsession.AgentEventEnvelope) bool {
	return func(env chatsession.AgentEventEnvelope) bool {
		out, ok := outbound.Translate(env.ChatID, *env.Event)
		if !ok {
			return false
		}
		out.ReplyTo = env.UserMsgID
		_ = ch.Send(context.Background(), out)
		return false
	}
}

// --- helpers ----------------------------------------------------------

func newIntegrationChatSession(chatID string, spawner chatsession.Spawner) *chatsession.ChatSession {
	cs, _ := chatsession.New(chatID, "fake")
	cs = cs.WithSpawner(spawner)
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent("fake")
	return cs
}

// --- tests -----------------------------------------------------------

// TestIntegration_AgentEvent_ReachesChannel is the smallest
// reproduction: enqueue a message, submit it to the AS, push an
// EventAgentText from the fake bridge, assert an OutReply lands on the
// channel with the correct chat_id + reply_to + text.
func TestIntegration_AgentEvent_ReachesChannel(t *testing.T) {
	spawner := &integrationSpawner{}
	cs := newIntegrationChatSession("oc_test_chat", spawner)

	// Recording channel + runtime handler (same shape as the
	// production wireRuntimeCallbacksAndRestore onCreate).
	mock := &recordingChannel{chatID: cs.ChatID}
	cs.AgentEventBus.Subscribe(integrationEventHandler(mock, cs))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.PumpEvents(ctx)

	// Trigger Spawn (drives AgentSession.Spawn → fakeSpawner.Spawn →
	// fakeAgentSession becomes the handle → startReadPump launches).
	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	defer as.Shutdown()
	fake := spawner.lastFake
	if fake == nil {
		t.Fatal("spawner did not capture fakeAgentSession")
	}

	// Drive the queue + submit the prompt (real production path).
	const userMsgID = "om_msg_1"
	msg := chatsession.Message{
		ID:     userMsgID,
		ChatID: cs.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hi agent"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	// Fake bridge streams a reply chunk.
	fake.PushEvent(agent.AgentEvent{Kind: agent.EventAgentText, Text: "hello back"})

	// Wait for the round-trip.
	var rec []messages.OutboundMessage
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
	var outReply *messages.OutboundMessage
	for i := range rec {
		if rec[i].Kind == messages.OutReply {
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
	// that would normally fire on the PromptEndBus.
	fake.FinishEvent()
}

// TestIntegration_AgentEventResult_ReachesChannel exercises the
// final-result path (EventAgentResult → OutResult). This is the second
// half of the "no OutReply/OutResult to feishu" symptom.
func TestIntegration_AgentEventResult_ReachesChannel(t *testing.T) {
	spawner := &integrationSpawner{}
	cs := newIntegrationChatSession("oc_test_chat", spawner)

	mock := &recordingChannel{chatID: cs.ChatID}
	cs.AgentEventBus.Subscribe(integrationEventHandler(mock, cs))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.PumpEvents(ctx)

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	defer as.Shutdown()
	fake := spawner.lastFake
	if fake == nil {
		t.Fatal("spawner did not capture fakeAgentSession")
	}

	const userMsgID = "om_msg_2"
	msg := chatsession.Message{
		ID:     userMsgID,
		ChatID: cs.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "go"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	fake.PushEvent(agent.AgentEvent{
		Kind: agent.EventAgentResult,
		Result: &agent.AgentResultEvent{
			Text: "final answer",
		},
	})

	var rec []messages.OutboundMessage
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec = mock.Record()
		if len(rec) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rec) == 0 {
		t.Fatal("no OutboundMessage captured on channel within 2s — EventAgentResult never reached ch.Send")
	}

	var outResult *messages.OutboundMessage
	for i := range rec {
		if rec[i].Kind == messages.OutResult {
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

func (s *integrationSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	fake := newIntegrationFake(os.Getpid())
	s.lastFake = fake
	return fake.Start(context.Background(), agent.StartConfig{})
}

// integrationFake is a minimal agent.Agent that captures
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
func (f *integrationFake) Info() agent.Info {
	return agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil)
}
func (f *integrationFake) Detect() error { return nil }
func (f *integrationFake) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return f.buildLive(), nil
}

// buildLive wraps f in a *agent.Agent with integrationFakeDriver.
func (f *integrationFake) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		f.pid, f.events, &integrationFakeDriver{inner: f})
}

// integrationFakeDriver forwards driver calls to integrationFake.
type integrationFakeDriver struct{ inner *integrationFake }

func (d *integrationFakeDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *integrationFakeDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *integrationFakeDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *integrationFakeDriver) Stop(ctx context.Context) error  { return d.inner.Stop(ctx) }
func (d *integrationFakeDriver) Close() error                   { return d.inner.Close() }
func (d *integrationFakeDriver) Keepalive(ctx context.Context, _ func(context.Context) error) error {
	return nil
}
func (f *integrationFake) SendBlocks(context.Context, []agent.ContentBlock) error {
	return nil
}
func (f *integrationFake) SendPermission(string) error { return nil }
func (f *integrationFake) New(context.Context) error   { return nil }
func (f *integrationFake) Stop(context.Context) error { return agent.ErrNotSupported }
func (f *integrationFake) RunOnce(ctx context.Context, _ agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	if err := f.SendBlocks(ctx, blocks); err != nil {
		return agent.RunResult{}, err
	}
	for {
		select {
		case ev, ok := <-f.events:
			if !ok {
				return agent.RunResult{}, errors.New("integrationFake: event stream closed without result")
			}
			switch ev.Kind {
			case agent.EventAgentResult:
				if ev.Result == nil {
					return agent.RunResult{}, errors.New("integrationFake: nil result payload")
				}
				return agent.RunResult{
					Text:       ev.Result.Text,
					Usage:      ev.Result.Usage,
					DurationMs: ev.Result.DurationMs,
					Subtype:    ev.Result.Subtype,
				}, nil
			case agent.EventAgentDone:
				return agent.RunResult{}, errors.New("integrationFake: turn ended without result")
			case agent.EventAgentError:
				if ev.Err != nil {
					return agent.RunResult{}, ev.Err
				}
				return agent.RunResult{}, errors.New("integrationFake: nil error payload")
			}
		case <-ctx.Done():
			return agent.RunResult{}, ctx.Err()
		}
	}
}
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
	f.events <- agent.AgentEvent{Kind: agent.EventAgentDone}
	f.closed = true
	close(f.events)
}

// Review is unimplemented for the gateway integration fake —
// integration tests don't drive /review. Return
// ErrReviewNotSupported to satisfy the agent.Starter interface.
func (f *integrationFake) Review(_ context.Context, _ agent.StartConfig) (agent.RunResult, error) {
	return agent.RunResult{}, agent.ErrReviewNotSupported
}

var _ agent.Starter = (*integrationFake)(nil)

// --- helpers ----------------------------------------------------------

func summarizeKinds(msgs []messages.OutboundMessage) []messages.OutboundKind {
	out := make([]messages.OutboundKind, len(msgs))
	for i, m := range msgs {
		out[i] = m.Kind
	}
	return out
}

// --- bridge integration (real claudecode bridge, fake shell script) ---

// TestIntegration_RealBridge_FakeShell covers the layer the
// in-memory integrationFake skips: the claudecode bridge's
// pumpStream(stdout → s.events). It spawns a shell script via
// claudecode.NewStarter(...).Start() that emits a stream-json transcript
// on stdout, then drives the full Submit → readpump → PumpEvents
// → AgentEventBus fan-out. If pumpStream is broken (closed before
// first read, wrong channel buffering, parse failure), this test
// fails with a clear signal.
func TestIntegration_RealBridge_FakeShell(t *testing.T) {
//
// The shell script writes:
//   1. system/init             → EventAgentReady (immediate, no anchor)
//   2. (wait for stdin prompt)  → barrier: bridge.SendBlocks has
//                                 set currentPrompt by the time we
//                                 return from read
//   3. assistant message       → EventAgentText "hello back"
//   4. result                  → EventAgentResult "final answer"
//   5. EOF (exit 0)            → pumpStream closes events
//
// Why the stdin-wait barrier matters: the bridge spawns the script
// via exec.CommandContext and pumpStream starts reading stdout
// immediately. If the script eagerly emits init/assistant/result
// before the test calls QueueUserMessage, the readpump sees those
// events while currentPrompt is still nil, so they all carry
// UserMsgID="" and the OutReply.ReplyTo assertion below fails.
// Real claudecode behaves correctly because the real CLI waits for
// the prompt on stdin before producing the assistant turn — the
// fake must mirror that. See issue: anchor race in
// real-bridge + fake-shell integration test (env-sensitive).
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude.sh")
	//
	// init is emitted immediately (matches real claude: ready fires
	// before any prompt), then we block on stdin until the bridge's
	// writeLine writes the prompt. By the time read returns, the
	// ChatSession has currentPrompt set — see
	// internal/chatsession/agentsession.go:Submit for the assignment
	// order — so all subsequent events stamp UserMsgID and pass the
	// ReplyTo assertion below.
	//
	// `set -e` is intentionally OMITTED. `read` returns non-zero on
	// EOF or timeout; with set -e the script would exit on the very
	// first false return, skipping assistant/result and producing a
	// "no outbound" failure instead of a real anchor or bridge
	// failure signal. `|| true` makes the timeout fallback explicit;
	// 30s is comfortably above the test's 30s deadline so any hang
	// in the bridge layer still surfaces as a test failure, not a
	// hang. The previous 5s timeout was flaky under load — when
	// the full test suite runs concurrently the bridge's writeLine
	// can take longer than 5s to reach bash, so bash's read -t
	// would time out before the prompt arrived and the script
	// would exit without emitting the assistant/result events.
	//
	// `read -t 30 _PROMPT` takes one line. writeLine writes the
	// prompt body followed by a single \n, so one read is enough.
	body := `#!/bin/bash
echo '{"type":"system","subtype":"init","session_id":"test-session-1","model":"claude-test","cwd":"/tmp"}'
# Anchor barrier + stay-alive loop. The while-read serves two
# purposes:
#
#  (a) Anchor barrier: wait for the bridge to write the prompt on
#      stdin before emitting the assistant turn. Without this,
#      the readpump races the test's QueueUserMessage and the
#      assistant event lands with currentPrompt still nil, which
#      yields empty UserMsgID and breaks the OutReply.ReplyTo
#      assertion below.
#
#  (b) Stay alive: the script MUST NOT exit between prompts. A
#      real Claude Code CLI stays alive on stdin; if the script
#      exits 0, the bridge lifecycle sees isClosed(closed)==false
#      (no bridge.Close call yet) and emits EventAgentError,
#      which the readpump terminal-handler forwards to endPrompt,
#      clearing currentPrompt BEFORE the assistant/result events
#      are stamped with UserMsgID. The script exits naturally on
#      stdin EOF when the test's deferred as.Shutdown drives
#      bridge.Close, which sends SIGINT to the process group;
#      bash dies in non-interactive mode, the lifecycle sees
#      graceful==true, and no EventAgentError is emitted.
while read -t 30 _PROMPT; do
  echo '{"type":"assistant","message":{"id":"msg_1","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hello back"}]}}'
  echo '{"type":"result","result":"final answer","duration_ms":100,"is_error":false}'
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Sanity check: the bash on this host must understand `read -t N`
	// (POSIX-extended, not dash-builtin semantics). Without this
	// pre-flight, a sandbox where /bin/bash is missing or sh-linked
	// to a dash variant would explode on a different test, not
	// here, making the root cause harder to localize.
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH (%v) — fake script requires bash; skipping", err)
	}

	// Sanity check: `stdbuf` must be on PATH. Without it the bash
	// script uses 4 KiB block-buffered stdout when connected to a
	// pipe (Go's cmd.StdoutPipe), so the bridge's readpump can see
	// EOF before assistant/result are flushed — a race exposed by
	// `go test -race` (commit 7ad4432's `lifecycle()` goroutine
	// closes the events channel promptly, making the timing window
	// wider than the pre-refactor architecture).
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Skipf("stdbuf not on PATH (%v) — fake script requires stdbuf -oL for line-buffered stdout; skipping", err)
	}

	// Spawner that drives the real claudecode bridge against our
	// fake shell. We can't pass `stdbuf -oL` directly as the bridge's
	// command — the bridge prepends its DefaultArgs (--input-format,
	// --output-format, --permission-mode, --verbose) to every spawn,
	// and stdbuf would try (and fail) to parse those flags as its
	// own options. Instead we write a tiny wrapper script that
	// ignores $@ and `exec stdbuf -oL bash <fake>` itself; this puts
	// the fake behind line-buffered stdout regardless of how the
	// bridge decides to invoke it.
	wrapper := filepath.Join(dir, "fake-claude-wrapper.sh")
	wrapperBody := fmt.Sprintf("#!/bin/bash\nexec stdbuf -oL bash %q\n", script)
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o755); err != nil {
		t.Fatalf("WriteFile wrapper: %v", err)
	}
	realAgent := claudecode.NewStarter("claude", wrapper, nil)
	spawner := &realBridgeSpawner{agent: realAgent}

	cs := newIntegrationChatSession("oc_real_bridge", spawner)

	mock := &recordingChannel{chatID: cs.ChatID}
	cs.AgentEventBus.Subscribe(integrationEventHandler(mock, cs))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.PumpEvents(ctx)

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	defer as.Shutdown()

	const userMsgID = "om_real_bridge_1"
	msg := chatsession.Message{
		ID:     userMsgID,
		ChatID: cs.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "go"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	// Wait for the round-trip: we expect OutInit (from
	// system/init) + OutReply (assistant text) + OutResult
	// (final answer) to reach the channel. Poll until OutResult
	// arrives (or timeout) — checking len >= 2 races the
	// readpump when init + reply flow through faster than the
	// result event, producing a spurious "no OutResult" failure
	// even though the bridge is functioning correctly. The
	// previous "len >= 2" condition was the flake root cause:
	// when the fake script's `read -t 30` returns EOF
	// immediately (stdin is /dev/null on this test harness),
	// all three echos land at the subprocess stdout within a
	// few ms, the bridge processes init + reply faster than
	// the result event reaches channel.Send, and the test
	// breaks out of the loop before OutResult is captured.
	// The 30s deadline still matches the bash script's
	// `read -t 30` above so a real bridge hang surfaces as a
	// failure rather than a hang.
	var rec []messages.OutboundMessage
	deadline := time.Now().Add(30 * time.Second)
	var hasOutResult bool
	for time.Now().Before(deadline) {
		rec = mock.Record()
		hasOutResult = false
		for i := range rec {
			if rec[i].Kind == messages.OutResult {
				hasOutResult = true
				break
			}
		}
		if hasOutResult {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rec) == 0 {
		t.Fatal("no OutboundMessage captured within 30s — bridge did not produce events end-to-end")
	}

	// Find OutReply + OutResult.
	var outReply, outResult *messages.OutboundMessage
	for i := range rec {
		switch rec[i].Kind {
		case messages.OutReply:
			outReply = &rec[i]
		case messages.OutResult:
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

// realBridgeSpawner wraps a real claudecode.Starter (whose Start
// spawns an actual subprocess via exec.Command).
type realBridgeSpawner struct {
	agent *claudecode.Starter
}

func (s *realBridgeSpawner) Spawn(ctx context.Context, _, _ string, args []string, sessionID string) (*agent.Agent, error) {
	return s.agent.Start(ctx, agent.StartConfig{
		Workspace: "/tmp",
		Args:      args,
		SessionID: sessionID,
	})
}

// noopEmitter is a test-only outbound.Emitter that does nothing.
type noopEmitter struct{}

func (noopEmitter) Send(context.Context, messages.OutboundMessage) error {
	return nil
}
func (noopEmitter) SendCard(context.Context, messages.OutboundMessage) (string, error) {
	return "", nil
}
