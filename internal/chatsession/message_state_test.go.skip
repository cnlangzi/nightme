package chatsession

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// captureHandler records every callback invocation for assertions.
// Implements the func(chatID, userMsgID, state) signature.
type captureHandler struct {
	mu    sync.Mutex
	calls []messageStateCall
}

type messageStateCall struct {
	chatID, userMsgID string
	state             agent.MessageState
}

func (c *captureHandler) handler(chatID, userMsgID string, state agent.MessageState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, messageStateCall{chatID, userMsgID, state})
}

func (c *captureHandler) snapshot() []messageStateCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]messageStateCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// TestEmitMessageState_HandlerInvoked verifies that EmitMessageState
// fires the registered handler with the correct (chatID, userMsgID,
// state) triple. No-op when no handler installed.
//
// F-53: enum is now MessageQueued / MessageSubmitted /
// MessageDropped (was Received/Forwarded/Done/Failed).
func TestEmitMessageState_HandlerInvoked(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	cs.EmitMessageState("om_msg_1", agent.MessageQueued)
	cs.EmitMessageState("om_msg_2", agent.MessageSubmitted)
	cs.EmitMessageState("om_msg_3", agent.MessageDropped)

	got := cap.snapshot()
	if len(got) != 3 {
		t.Fatalf("captured %d calls; want 3", len(got))
	}
	want := []messageStateCall{
		{"oc_chat", "om_msg_1", agent.MessageQueued},
		{"oc_chat", "om_msg_2", agent.MessageSubmitted},
		{"oc_chat", "om_msg_3", agent.MessageDropped},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("call[%d] = %+v; want %+v", i, got[i], w)
		}
	}
}

// TestEmitMessageState_NoHandlerIsNoop confirms EmitMessageState is
// safe to call without a registered handler — must not panic.
func TestEmitMessageState_NoHandlerIsNoop(t *testing.T) {
	cs := New("oc_chat", "claude")
	// No SetMessageStateHandler call.
	cs.EmitMessageState("om_msg", agent.MessageQueued)
	// If we got here without panic, success.
}

// TestSetMessageStateHandler_NilClears confirms passing nil clears
// the handler (subsequent emits become no-ops).
func TestSetMessageStateHandler_NilClears(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)
	cs.EmitMessageState("om_1", agent.MessageQueued)
	cs.SetMessageStateHandler(nil)
	cs.EmitMessageState("om_2", agent.MessageDropped)

	if got := len(cap.snapshot()); got != 1 {
		t.Fatalf("captured %d calls after nil-clear; want 1 (only the first emit)", got)
	}
}

// TestMarkDropped_FlipsStageAndEmits verifies that MarkDropped
// (F-53's new explicit-clear path) flips a single Message to
// MessageDropped AND wire-emits MessageDropped exactly once.
//
// F-53: replaces the old `emitMessageStateForCurrentTurn` test —
// Message.Stage is mutated directly per-message, no fan-out,
// no anchor concept.
func TestMarkDropped_FlipsStageAndEmits(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "hi"}}, "om_x")
	// F-53 P1 fix: QueueUserMessage now removes the message from
	// messagesByID on Add failure (so it doesn't leak as a ghost
	// in the buffer-full path). For MarkDropped tests we want
	// the message to STAY in the map; the simplest way is to
	// set Busy before QueueUserMessage so Add appends to the
	// queue without invoking the hook.
	cs.SetBusy()
	cs.QueueUserMessage(msg)

	cs.MarkDropped("om_x")

	if got := msg.Stage; got != agent.MessageDropped {
		t.Errorf("msg.Stage = %v; want MessageDropped", got)
	}
	calls := cap.snapshot()
	if len(calls) != 1 {
		t.Fatalf("captured %d calls; want 1", len(calls))
	}
	if calls[0] != (messageStateCall{"oc_chat", "om_x", agent.MessageDropped}) {
		t.Errorf("call = %+v; want om_x/Dropped", calls[0])
	}

	// Idempotent: a second MarkDropped call is a no-op (still emits,
	// but Stage is already Dropped — wire emit stays for downstream
	// idempotency / reactions).
	// (Actually: MarkDropped emits even on second call so the wire
	// sees the explicit-clear. This is by design — see
	// docs/feat/message_lifecycle.md §5.1.)
	cs.MarkDropped("om_x")
	if got := len(cap.snapshot()); got != 2 {
		t.Errorf("captured %d calls after second MarkDropped; want 2", got)
	}
}

// TestMarkDropped_UnknownMessageIsNoop verifies that calling
// MarkDropped for a message ID that's not in messagesByID is a
// safe no-op (returns false, no emit, no panic).
func TestMarkDropped_UnknownMessageIsNoop(t *testing.T) {
	cs := New("oc_chat", "claude")
	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	if cs.MarkDropped("om_unknown") {
		t.Errorf("MarkDropped(unknown) = true; want false")
	}
	if got := len(cap.snapshot()); got != 0 {
		t.Errorf("captured %d calls for unknown message; want 0", got)
	}
}

// TestDefaultPromptHook_InstallsPromptOnAS verifies that
// defaultPromptHookLocked (F-53's renamed default hook) actually
// submits to the active AS and installs the resulting Prompt on
// `AgentSession.currentPrompt`.
//
// F-53: replaces the old `TestDefaultFlushHook_TracksUserMsgIDs`,
// which asserted on the (now-removed) `currentTurnUserMsgID`
// scalar. The new anchor lives on `AgentSession.currentPrompt.
// LastMessageID`.
func TestDefaultPromptHook_InstallsPromptOnAS(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.WithSpawner(&spySpawner{})

	cs.mu.Lock()
	hook := cs.defaultPromptHookLocked()
	cs.activeAS = newActiveAgentNoop()
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "hi"}}, "om_x")
	// Pre-register the message in cs.messagesByID so the hook's
	// Stage-flip / PromptID-stamp have a target. In production
	// this happens inside QueueUserMessage before the hook is
	// ever invoked; here we bypass QueueUserMessage (which would
	// call the hook itself) and call the hook directly.
	cs.messagesByID.Store(msg.ID, msg)
	p := &Prompt{
		MessageIDs:    []string{"om_x"},
		LastMessageID: "om_x",
		Blocks:        msg.Blocks,
		ChatSessionID: cs.ChatID,
	}
	if err := hook(p); err != nil {
		t.Fatalf("hook: %v", err)
	}

	cs.mu.RLock()
	as := cs.activeAS
	var installed *Prompt
	if as != nil {
		installed = as.CurrentPrompt()
	}
	cs.mu.RUnlock()

	if installed == nil {
		t.Fatalf("AgentSession.currentPrompt is nil; want installed")
	}
	if installed.LastMessageID != "om_x" {
		t.Errorf("Prompt.LastMessageID = %q; want om_x", installed.LastMessageID)
	}
	if installed.AgentSessionID != as.ID {
		t.Errorf("Prompt.AgentSessionID = %q; want %q", installed.AgentSessionID, as.ID)
	}
	if installed.AckedAt.IsZero() {
		t.Errorf("Prompt.AckedAt is zero; want set after SendBlocks success")
	}
	// LastProgressAt touched by hook (not the readpump in this
	// test path — the hook sets it to time.Now() on success).
	if installed.LastProgressAt.IsZero() {
		t.Errorf("Prompt.LastProgressAt is zero; want set on submit")
	}
	if msg := cs.GetMessage("om_x"); msg == nil {
		t.Errorf("GetMessage(om_x) = nil after submit; want non-nil")
	} else if msg.Stage != agent.MessageSubmitted {
		t.Errorf("msg.Stage = %v; want MessageSubmitted", msg.Stage)
	} else if msg.PromptID != installed.ID {
		t.Errorf("msg.PromptID = %q; want %q", msg.PromptID, installed.ID)
	}
}

// spySpawner is a no-op Spawner for tests that don't actually
// fork a process.
type spySpawner struct{}

func (s *spySpawner) Spawn(_ context.Context, _ string, _ string, _ []string, _ string) (agent.AgentSession, error) {
	return nil, errors.New("spySpawner: not implemented")
}

// newActiveAgentNoop creates a minimal AgentSession whose
// SendBlocks is a no-op, so PromptHook tests can run without
// spawning a real CLI.
func newActiveAgentNoop() *AgentSession {
	as := NewAgentSession("test-as", "test-cs", "claude", "/tmp", nil)
	// Inject a fake handle so SendBlocks succeeds. We use the
	// recordingAgentSession pattern from flushhook_test.go.
	rec := &recordingAgentSession{pid: 1, events: make(chan agent.AgentEvent, 16)}
	as.handle = rec
	return as
}

// Use the existing recordingAgentSession type from flushhook_test.go
// to satisfy AgentSession.Handle(). This avoids an import cycle
// (test types live in the same package).
var _ = (*recordingAgentSession)(nil)

// TestDefaultPromptHook_FailedSendBlocksKeepsMessageQueued verifies
// the F-53 retry semantics (docs/feat/message_lifecycle.md §3
// 原则 5): when SendBlocks returns an error, the message stays
// in MessageQueued so the next flushPending() retries it.
//
// Critical assertion: NO MessageSubmitted is wire-emitted on a
// failed SendBlocks. If it were, the channel would lie to the user
// ("your message was delivered" when it wasn't).
func TestDefaultPromptHook_FailedSendBlocksKeepsMessageQueued(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.WithSpawner(&spySpawner{})

	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	cs.mu.Lock()
	hook := cs.defaultPromptHookLocked()
	failing := newFailingAgentSession()
	failingAS := NewAgentSession("test-as-fail", "test-cs", "claude", "/tmp", nil)
	failingAS.handle = failing
	cs.activeAS = failingAS
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "x"}}, "om_x")
	cs.messagesByID.Store(msg.ID, msg)
	p := &Prompt{
		MessageIDs:    []string{"om_x"},
		LastMessageID: "om_x",
		Blocks:        msg.Blocks,
		ChatSessionID: cs.ChatID,
	}

	if err := hook(p); err == nil {
		t.Fatalf("hook returned nil; want error from SendBlocks")
	}

	// Message must still be Queued — failed SendBlocks does NOT
	// flip Stage, and does NOT create a Prompt on the AS.
	if msg.Stage != agent.MessageQueued {
		t.Errorf("after failed SendBlocks, msg.Stage = %v; want MessageQueued", msg.Stage)
	}
	if msg.PromptID != "" {
		t.Errorf("after failed SendBlocks, msg.PromptID = %q; want empty", msg.PromptID)
	}

	cs.mu.RLock()
	installed := cs.activeAS.CurrentPrompt()
	cs.mu.RUnlock()
	if installed != nil {
		t.Errorf("after failed SendBlocks, AgentSession.currentPrompt = %+v; want nil", installed)
	}

	// NO MessageSubmitted emit on the wire (the channel must not
	// be told the message was delivered when it wasn't).
	for _, c := range cap.snapshot() {
		if c.state == agent.MessageSubmitted {
			t.Errorf("wire emit MessageSubmitted on failed SendBlocks; chatID=%q msgID=%q", c.chatID, c.userMsgID)
		}
	}
}

// TestDefaultPromptHook_NextFlushRetriesFailedMessage verifies the
// retry path: a SendBlocks-failed message left Queued gets re-tried
// by the next flushPending() call. The retry path itself uses the
// SAME hook — no special "retry" code path exists.
func TestDefaultPromptHook_NextFlushRetriesFailedMessage(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.WithSpawner(&spySpawner{})

	cs.mu.Lock()
	hook := cs.defaultPromptHookLocked()
	failing := newFailingAgentSession()
	failingAS := NewAgentSession("test-as-fail", "test-cs", "claude", "/tmp", nil)
	failingAS.handle = failing
	cs.activeAS = failingAS
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "x"}}, "om_x")
	cs.messagesByID.Store(msg.ID, msg)
	p := &Prompt{
		MessageIDs:    []string{"om_x"},
		LastMessageID: "om_x",
		Blocks:        msg.Blocks,
		ChatSessionID: cs.ChatID,
	}

	// First call: fails.
	if err := hook(p); err == nil {
		t.Fatalf("first hook should fail")
	}
	if msg.Stage != agent.MessageQueued {
		t.Fatalf("after first failure, msg.Stage = %v; want Queued", msg.Stage)
	}

	// Switch AS to a working one (simulating the user /use'ing
	// back to a healthy agent, OR just retry semantics with the
	// same AS — both work because the hook re-reads cs.activeAS
	// on every call).
	rec := newRecordingAgentSession(1)
	workingAS := NewAgentSession("test-as-2", "test-cs", "claude", "/tmp", nil)
	workingAS.handle = rec
	cs.mu.Lock()
	cs.activeAS = workingAS
	cs.mu.Unlock()

	// Second call: succeeds.
	if err := hook(p); err != nil {
		t.Fatalf("second hook should succeed; got %v", err)
	}

	if msg.Stage != agent.MessageSubmitted {
		t.Errorf("after retry success, msg.Stage = %v; want Submitted", msg.Stage)
	}
	if msg.PromptID == "" {
		t.Errorf("after retry success, msg.PromptID empty; want set")
	}
}

// failingAgentSession is a minimal AgentSession implementation
// whose SendBlocks always returns an error. Used by the
// retry-semantics tests above. Other methods are no-op stubs
// because the tests only exercise SendBlocks.
type failingAgentSession struct {
	events chan agent.AgentEvent
}

func newFailingAgentSession() *failingAgentSession {
	return &failingAgentSession{events: make(chan agent.AgentEvent, 4)}
}

func (f *failingAgentSession) Events() <-chan agent.AgentEvent { return f.events }
func (f *failingAgentSession) PID() int                      { return 99 }
func (f *failingAgentSession) SendText(_ string) error       { return nil }
func (f *failingAgentSession) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	return errors.New("failingAgentSession: SendBlocks always fails")
}
func (f *failingAgentSession) SendPermission(_ string) error { return nil }
func (f *failingAgentSession) New(_ context.Context) error   { return nil }
func (f *failingAgentSession) Close() error                  { return nil }
