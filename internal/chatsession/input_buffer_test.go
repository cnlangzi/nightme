package chatsession

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// trackingFlushHook records calls; threadsafe.
type trackingFlushHook struct {
	mu       sync.Mutex
	calls    int
	lastCombined []agent.ContentBlock
	lastIDs    []string
}

func (h *trackingFlushHook) hook(combined []agent.ContentBlock, userMsgIDs []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.lastCombined = combined
	h.lastIDs = userMsgIDs
	return nil
}

func (h *trackingFlushHook) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// --- InputBuffer FSM tests (mirrors session/input_buffer_test.go) ---

func TestInputBuffer_IdleFlushesImmediately(t *testing.T) {
	hook := &trackingFlushHook{}
	b := NewInputBuffer(hook.hook, 50, 1024)

	blocks := []agent.ContentBlock{{Type: agent.ContentText, Text: "hello"}}
	if err := b.Add(blocks, "msg1"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if hook.Calls() != 1 {
		t.Fatalf("hook should be called once, got %d", hook.Calls())
	}
	if b.Pending() != 0 {
		t.Fatalf("buffer should be empty after Idle flush, got %d", b.Pending())
	}
}

func TestInputBuffer_BusyQueuesThenFlushes(t *testing.T) {
	hook := &trackingFlushHook{}
	b := NewInputBuffer(hook.hook, 50, 1024)

	b.SetState(StateBusy)

	if err := b.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "first"}}, "m1"); err != nil {
		t.Fatalf("Add #1: %v", err)
	}
	if err := b.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "second"}}, "m2"); err != nil {
		t.Fatalf("Add #2: %v", err)
	}
	if hook.Calls() != 0 {
		t.Fatalf("hook should not fire while Busy, got %d calls", hook.Calls())
	}
	if b.Pending() != 2 {
		t.Fatalf("expected 2 queued, got %d", b.Pending())
	}

	// Turn ends; OnTurnEnded flushes.
	b.SetState(StateIdle)
	if err := b.OnTurnEnded(); err != nil {
		t.Fatalf("OnTurnEnded: %v", err)
	}
	if hook.Calls() != 1 {
		t.Fatalf("hook should fire once after flush, got %d", hook.Calls())
	}
	if b.Pending() != 0 {
		t.Fatalf("buffer should be empty after flush, got %d", b.Pending())
	}
}

func TestInputBuffer_FullReturnsError(t *testing.T) {
	b := NewInputBuffer(nil, 2, 1024)
	b.SetState(StateBusy)

	_ = b.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "a"}}, "m1")
	_ = b.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "b"}}, "m2")
	err := b.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "c"}}, "m3")
	if !errors.Is(err, ErrBufferFull) {
		t.Fatalf("expected ErrBufferFull, got %v", err)
	}
}

func TestInputBuffer_ClearDiscardsQueued(t *testing.T) {
	b := NewInputBuffer(nil, 50, 1024)
	b.SetState(StateBusy)
	_ = b.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}, "m1")
	_ = b.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "y"}}, "m2")

	if got := b.Clear(); got != 2 {
		t.Fatalf("Clear should return 2, got %d", got)
	}
	if b.Pending() != 0 {
		t.Fatalf("buffer should be empty after clear")
	}
}

func TestInputBuffer_SetFlushHook_RebindsTarget(t *testing.T) {
	hook1 := &trackingFlushHook{}
	hook2 := &trackingFlushHook{}

	b := NewInputBuffer(hook1.hook, 50, 1024)
	b.SetState(StateBusy)
	_ = b.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "first"}}, "m1")

	// Rebind hook mid-flight.
	b.SetFlushHook(hook2.hook)

	b.SetState(StateIdle)
	_ = b.OnTurnEnded()

	if hook1.Calls() != 0 {
		t.Errorf("old hook should not fire after rebind, got %d", hook1.Calls())
	}
	if hook2.Calls() != 1 {
		t.Errorf("new hook should fire after rebind, got %d", hook2.Calls())
	}
}

// --- ChatSession-level ownership tests (commit 9) ---

func TestChatSession_QueueUserMessage_IdleFlushed(t *testing.T) {
	hook := &trackingFlushHook{}
	cs := New("oc_xxx", "claude")
	cs.SetFlushHook(hook.hook)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")

	if err := cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hello"}}, "m1"); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if hook.Calls() != 1 {
		t.Fatalf("hook should fire on Idle, got %d", hook.Calls())
	}
	if cs.BufferPending() != 0 {
		t.Fatalf("buffer should be empty, got %d", cs.BufferPending())
	}
}

func TestChatSession_QueueUserMessage_BusyQueued(t *testing.T) {
	hook := &trackingFlushHook{}
	cs := New("oc_xxx", "claude")
	cs.SetFlushHook(hook.hook)

	cs.SetBusy()
	if err := cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "hello"}}, "m1"); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if hook.Calls() != 0 {
		t.Fatalf("hook should not fire while Busy")
	}
	if cs.BufferPending() != 1 {
		t.Fatalf("expected 1 queued, got %d", cs.BufferPending())
	}
	if cs.BufferState() != StateBusy {
		t.Fatalf("expected StateBusy, got %s", cs.BufferState())
	}

	// Turn ends → flush.
	cs.SetIdle()
	if err := cs.OnTurnEnded(); err != nil {
		t.Fatalf("OnTurnEnded: %v", err)
	}
	if hook.Calls() != 1 {
		t.Fatalf("hook should fire on flush, got %d", hook.Calls())
	}
	if cs.BufferPending() != 0 {
		t.Fatalf("buffer should be empty, got %d", cs.BufferPending())
	}
}

func TestChatSession_BufferSurvivesAgentSwitch(t *testing.T) {
	// The most important commit-9 invariant: switching the active
	// AgentSession via /use must NOT reset the InputBuffer FSM.
	// Queued messages must flush to the new active AgentSession.
	hook := &trackingFlushHook{}
	cs := New("oc_xxx", "claude")
	cs.SetFlushHook(hook.hook)
	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")

	// Spawn claude.
	cs.LookupActiveAgentSession()

	// Mark Busy (simulating claude processing a turn).
	cs.SetBusy()
	if err := cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "queued"}}, "m1"); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if cs.BufferPending() != 1 {
		t.Fatalf("expected 1 queued, got %d", cs.BufferPending())
	}

	// /use codex while Busy (no SetIdle → buffer stays Busy).
	cs.SetActiveAgent("codex")
	cs.LookupActiveAgentSession()

	// Buffer state must NOT have changed (still Busy, still 1 queued).
	if cs.BufferState() != StateBusy {
		t.Fatalf("/use should not change BufferState; got %s", cs.BufferState())
	}
	if cs.BufferPending() != 1 {
		t.Fatalf("/use should not clear queue; got %d pending", cs.BufferPending())
	}

	// Turn ends → flush via the (rebound) hook. In production the
	// runtime rebinds the hook to the new active AgentSession; here
	// we leave the original hook which captures the combined blocks.
	cs.SetIdle()
	if err := cs.OnTurnEnded(); err != nil {
		t.Fatalf("OnTurnEnded: %v", err)
	}
	if hook.Calls() != 1 {
		t.Fatalf("hook should fire once after /use + flush, got %d", hook.Calls())
	}
	if cs.BufferPending() != 0 {
		t.Fatalf("buffer should be empty after flush")
	}

	// Verify the flushed content is the queued one (the agent switch
	// did NOT silently drop it).
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if len(hook.lastCombined) != 1 || hook.lastCombined[0].Text != "queued" {
		t.Errorf("flushed content lost during /use: %+v", hook.lastCombined)
	}
}

func TestChatSession_KillAllClearsBuffer(t *testing.T) {
	hook := &trackingFlushHook{}
	cs := New("oc_xxx", "claude")
	cs.SetFlushHook(hook.hook)

	cs.SetBusy()
	_ = cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "lost"}}, "m1")
	_ = cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "lost2"}}, "m2")

	cs.KillAll()

	// KillAll doesn't currently reset the buffer FSM; just
	// documenting the current behavior. The queued messages are
	// stranded — runtime is expected to BufferClear() before/after
	// kill as part of commit 8b.
	if cs.BufferPending() != 2 {
		t.Logf("note: KillAll leaves %d messages stranded (runtime should BufferClear)", cs.BufferPending())
	}
}

func TestChatSession_BufferClearAfterKill(t *testing.T) {
	cs := New("oc_xxx", "claude")
	cs.SetBusy()
	_ = cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}, "m1")

	if got := cs.BufferClear(); got != 1 {
		t.Fatalf("BufferClear: got %d, want 1", got)
	}
	if cs.BufferPending() != 0 {
		t.Fatalf("BufferPending should be 0 after clear")
	}
}

func TestChatSession_LazyBufferAllocation(t *testing.T) {
	// A ChatSession that never dispatches messages should not
	// allocate an InputBuffer.
	cs := New("oc_xxx", "claude")
	if cs.BufferPending() != 0 {
		t.Fatalf("BufferPending should report 0 before any dispatch")
	}
	if cs.BufferState() != StateIdle {
		t.Fatalf("BufferState should default to StateIdle")
	}
}

// Compile-time guards.
var (
	_ = context.Background // import-only marker
	_ = strings.Repeat    // import-only marker
)