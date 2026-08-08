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
	// QueueUserMessage removes the message from messagesByID when
	// the queue is full, so it doesn't leak as a ghost. For
	// MarkDropped we want the message to STAY in the map and in
	// the queue. This cs has no activeAS, so the TryFlush that
	// QueueUserMessage triggers is a no-op (TryFlush SKIP
	// reason=activeAS_nil) and the message stays queued.
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

// TestSubmit_InstallsPromptOnAS verifies that the submit path
// installs the resulting Prompt on `AgentSession.currentPrompt`
// and flips the queued Message to MessageSubmitted.
//
// CS-AS 边界重构 Phase 1 port: this used to drive
// `cs.defaultPromptHookLocked()` directly. That hook is gone —
// `cs.TryFlush()` now builds the Prompt from the queue
// (buildPromptLocked) and hands it to `as.Submit`, which assigns
// the IDs/timestamps and installs currentPrompt. QueueUserMessage
// calls TryFlush for us, so queueing IS the submit path.
func TestSubmit_InstallsPromptOnAS(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.WithSpawner(&spySpawner{})

	as := newActiveAgentNoop()
	cs.mu.Lock()
	cs.activeAS = as
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "hi"}}, "om_x")
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	installed := as.CurrentPrompt()
	if installed == nil {
		t.Fatalf("AgentSession.currentPrompt is nil; want installed")
	}
	if installed.LastMessageID != "om_x" {
		t.Errorf("Prompt.LastMessageID = %q; want om_x", installed.LastMessageID)
	}
	if installed.AgentSessionID != as.ID {
		t.Errorf("Prompt.AgentSessionID = %q; want %q", installed.AgentSessionID, as.ID)
	}
	if installed.ID == "" {
		t.Errorf("Prompt.ID is empty; want assigned by Submit")
	}
	if installed.AckedAt.IsZero() {
		t.Errorf("Prompt.AckedAt is zero; want set after SendBlocks success")
	}
	if installed.LastProgressAt.IsZero() {
		t.Errorf("Prompt.LastProgressAt is zero; want set on submit")
	}

	// A submitted Prompt means the AS is mid-turn.
	if as.IsReady() {
		t.Errorf("AS is ready after Submit; want not-ready until the Prompt ends")
	}

	// The queue is drained and the message flipped to Submitted.
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("QueueLen = %d after submit; want 0", got)
	}
	if m := cs.GetMessage("om_x"); m == nil {
		t.Errorf("GetMessage(om_x) = nil after submit; want non-nil")
	} else if m.Stage != agent.MessageSubmitted {
		t.Errorf("msg.Stage = %v; want MessageSubmitted", m.Stage)
	}
}

// spySpawner is a no-op Spawner for tests that don't actually
// fork a process.
type spySpawner struct{}

func (s *spySpawner) Spawn(_ context.Context, _ string, _ string, _ []string, _ string) (agent.AgentSession, error) {
	return nil, errors.New("spySpawner: not implemented")
}

// newActiveAgentNoop creates a minimal AgentSession whose
// SendBlocks is a no-op, so submit-path tests can run without
// spawning a real CLI.
func newActiveAgentNoop() *AgentSession {
	as := NewAgentSession("test-as", "test-cs", "claude", "/tmp", nil)
	// Inject a fake handle so SendBlocks succeeds. We use the
	// recordingAgentSession pattern from test_helpers_recording_test.go.
	rec := newRecordingAgentSession(1)
	as.handle = rec
	return as
}

// TestSubmit_FailedSendBlocksKeepsMessageQueued verifies the F-53
// retry semantics (docs/feat/message_lifecycle.md §3 原则 5): when
// SendBlocks returns an error, the message stays in MessageQueued
// so the next TryFlush retries it.
//
// Critical assertion: NO MessageSubmitted is wire-emitted on a
// failed SendBlocks. If it were, the channel would lie to the user
// ("your message was delivered" when it wasn't).
func TestSubmit_FailedSendBlocksKeepsMessageQueued(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.WithSpawner(&spySpawner{})

	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	failingAS := NewAgentSession("test-as-fail", "test-cs", "claude", "/tmp", nil)
	failingAS.handle = newFailingAgentSession()
	cs.mu.Lock()
	cs.activeAS = failingAS
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "x"}}, "om_x")
	// QueueUserMessage swallows the TryFlush error (`_ =
	// cs.TryFlush()`) — the queue is intentionally left intact for
	// the retry. Call TryFlush explicitly so we can assert on the
	// error the submit path returned.
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if err := cs.TryFlush(); err == nil {
		t.Fatalf("TryFlush returned nil; want error from SendBlocks")
	}

	// Message must still be Queued — failed SendBlocks does NOT
	// flip Stage, and does NOT install a Prompt on the AS.
	if msg.Stage != agent.MessageQueued {
		t.Errorf("after failed SendBlocks, msg.Stage = %v; want MessageQueued", msg.Stage)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen = %d after failed SendBlocks; want 1 (retained for retry)", got)
	}
	if installed := failingAS.CurrentPrompt(); installed != nil {
		t.Errorf("after failed SendBlocks, currentPrompt = %+v; want nil", installed)
	}
	if !failingAS.IsReady() {
		t.Errorf("AS is not-ready after a FAILED Submit; a failed submit must not consume readiness")
	}

	// NO MessageSubmitted emit on the wire (the channel must not
	// be told the message was delivered when it wasn't).
	for _, c := range cap.snapshot() {
		if c.state == agent.MessageSubmitted {
			t.Errorf("wire emit MessageSubmitted on failed SendBlocks; chatID=%q msgID=%q", c.chatID, c.userMsgID)
		}
	}
}

// TestSubmit_NextFlushRetriesFailedMessage verifies the retry path:
// a SendBlocks-failed message left Queued gets re-tried by the next
// TryFlush. The retry uses the SAME path — no special "retry" code
// path exists.
func TestSubmit_NextFlushRetriesFailedMessage(t *testing.T) {
	cs := New("oc_chat", "claude")
	cs.WithSpawner(&spySpawner{})

	failingAS := NewAgentSession("test-as-fail", "test-cs", "claude", "/tmp", nil)
	failingAS.handle = newFailingAgentSession()
	cs.mu.Lock()
	cs.activeAS = failingAS
	cs.mu.Unlock()

	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "x"}}, "om_x")
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	// First flush: fails, message stays queued.
	if err := cs.TryFlush(); err == nil {
		t.Fatalf("first TryFlush should fail")
	}
	if msg.Stage != agent.MessageQueued {
		t.Fatalf("after first failure, msg.Stage = %v; want Queued", msg.Stage)
	}

	// Switch to a working AS (simulating the user /use'ing back to
	// a healthy agent, OR plain retry semantics — both work
	// because TryFlush re-reads cs.activeAS on every call).
	workingAS := NewAgentSession("test-as-2", "test-cs", "claude", "/tmp", nil)
	workingAS.handle = newRecordingAgentSession(1)
	cs.mu.Lock()
	cs.activeAS = workingAS
	cs.mu.Unlock()

	// Second flush: succeeds.
	if err := cs.TryFlush(); err != nil {
		t.Fatalf("second TryFlush should succeed; got %v", err)
	}

	if msg.Stage != agent.MessageSubmitted {
		t.Errorf("after retry success, msg.Stage = %v; want Submitted", msg.Stage)
	}
	if installed := workingAS.CurrentPrompt(); installed == nil {
		t.Errorf("after retry success, currentPrompt is nil; want installed")
	} else if installed.LastMessageID != "om_x" {
		t.Errorf("retried Prompt.LastMessageID = %q; want om_x", installed.LastMessageID)
	}
	if got := cs.QueueLen(); got != 0 {
		t.Errorf("QueueLen = %d after retry success; want 0", got)
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

// TestDispatcher_DoesNotEmitMessageSubmitted locks the dispatcher
// contract introduced by PR #69/#70: newMessageDispatcher emits
// only MessageQueued, not MessageSubmitted. TryFlush is the sole
// MessageSubmitted emit point.
//
// Regression guard for the L587 emit (which fired MessageSubmitted
// in the dispatcher before QueueUserMessage, leading to duplicate
// submits and false-positive OnIt on failure paths).
func TestDispatcher_DoesNotEmitMessageSubmitted(t *testing.T) {
	cs := New("oc_chat_dispatch", "claude")
	cs.WithSpawner(&spySpawner{})

	// Wire a working AS so the QueueUserMessage → TryFlush
	// → as.Submit → success path runs cleanly.
	workingAS := newActiveAgentNoop()
	cs.mu.Lock()
	cs.activeAS = workingAS
	cs.mu.Unlock()

	cap := &captureHandler{}
	cs.SetMessageStateHandler(cap.handler)

	// Mimic the dispatcher's hot path:
	//  1. emit MessageQueued (FastAck UX)
	//  2. LookupActiveAgentSession (would lazy-spawn in production)
	//  3. QueueUserMessage → TryFlush → as.Submit → MessageSubmitted
	msg := makeTestMessage(cs, []agent.ContentBlock{{Text: "hello"}}, "om_dispatch_test")
	cs.EmitMessageState("om_dispatch_test", agent.MessageQueued) // dispatcher step 1
	if err := cs.QueueUserMessage(msg); err != nil {              // dispatcher step 3
		t.Fatalf("QueueUserMessage: %v", err)
	}

	// Exactly 1 MessageQueued (from step 1) and 1 MessageSubmitted
	// (from TryFlush after Submit success). ZERO MessageSubmitted
	// from the dispatcher layer itself.
	queuedCount := 0
	submittedCount := 0
	for _, c := range cap.snapshot() {
		if c.userMsgID != "om_dispatch_test" {
			continue
		}
		switch c.state {
		case agent.MessageQueued:
			queuedCount++
		case agent.MessageSubmitted:
			submittedCount++
		}
	}
	if queuedCount != 1 {
		t.Errorf("MessageQueued count for om_dispatch_test = %d; want 1", queuedCount)
	}
	if submittedCount != 1 {
		t.Errorf("MessageSubmitted count for om_dispatch_test = %d; want 1 (sentinel: dispatcher MUST NOT have emitted — TryFlush is the sole point). All emits: %+v", submittedCount, cap.snapshot())
	}
}
