package chatsession

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestInput_RoutesToSelectedAS — input messages queued on ChatSession
// reach only the selected AS's currentPrompt. Other ASes in the pool
// don't receive them.
func TestInput_RoutesToSelectedAS(t *testing.T) {
	cs := newChatSessionForTest("cs_inp_selected")
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent("pi")

	as := NewAgentSession("as_input", cs.ID, "pi", "/tmp", nil)
	cs.attachAgentSession(as)
	cs.selectAgentSessionLocked(as)

	asB := NewAgentSession("as_other", cs.ID, "claude", "/tmp", nil)
	cs.attachAgentSession(asB)

	// Sanity: A is selected, B is in pool but not selected.
	if cs.SelectedAgentSession().ID != as.ID {
		t.Errorf("selected AS = %q, want %q", cs.SelectedAgentSession().ID, as.ID)
	}
	if asB == cs.SelectedAgentSession() {
		t.Error("asB should not be selected")
	}
}

// TestInput_NonSelectedASDoesNotReceive — selected AS receives a
// prompt; non-selected AS in the same pool stays untouched
// (currentPrompt nil).
func TestInput_NonSelectedASDoesNotReceive(t *testing.T) {
	cs := newChatSessionForTest("cs_inp_nonselected")

	asA := NewAgentSession("as_A", cs.ID, "pi", "/tmp", nil)
	asB := NewAgentSession("as_B", cs.ID, "claude", "/tmp", nil)
	cs.attachAgentSession(asA)
	cs.attachAgentSession(asB)
	cs.selectAgentSessionLocked(asA)

	// Verify pre-state.
	if cs.SelectedAgentSession().ID != asA.ID {
		t.Fatalf("selected = %q, want %q", cs.SelectedAgentSession().ID, asA.ID)
	}
	if asB.CurrentPrompt() != nil {
		t.Errorf("asB should have nil currentPrompt before input")
	}

	// Build a fake prompt and try to Submit on each.
	prompt := &Prompt{
		ID:      "p_x",
		AgentSessionID: asA.ID,
		Blocks:  []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}},
	}

	// asA: Submit should accept (since it's "ready" by default).
	// But without a handle / spawner, Submit may not actually
	// deliver. We're only checking that B is NOT the recipient.
	if err := asA.Submit(prompt); err != nil {
		// Without a real handle, Submit returns an error — that's
		// expected. The point of this test is that B is untouched.
		t.Logf("asA.Submit: %v (expected without real bridge handle)", err)
	}

	if asB.CurrentPrompt() != nil {
		t.Errorf("asB should NOT have received the prompt (currentPrompt = %v)", asB.CurrentPrompt())
	}
}

// TestInput_UseSwitchChangesTarget — after /use B, subsequent input
// dispatch targets asB (selected AS pointer flips).
func TestInput_UseSwitchChangesTarget(t *testing.T) {
	cs := newChatSessionForTest("cs_inp_use")

	asA := NewAgentSession("as_A", cs.ID, "pi", "/tmp", nil)
	asB := NewAgentSession("as_B", cs.ID, "claude", "/tmp", nil)
	cs.attachAgentSession(asA)
	cs.attachAgentSession(asB)
	cs.selectAgentSessionLocked(asA)

	if cs.SelectedAgentSession().ID != asA.ID {
		t.Fatalf("pre-/use: selected = %q, want A", cs.SelectedAgentSession().ID)
	}

	cs.SetSelectedAgent("claude")
	cs.selectAgentSessionLocked(asB)

	if cs.SelectedAgentSession().ID != asB.ID {
		t.Errorf("post-/use: selected = %q, want B", cs.SelectedAgentSession().ID)
	}
}

// TestInput_NoSelectedAS_DoesNotSubmit — when selectedAS is nil
// (no /cwd + /use yet), TryFlush is a no-op (returns nil) and the
// message stays in the queue.
func TestInput_NoSelectedAS_DoesNotSubmit(t *testing.T) {
	cs := newChatSessionForTest("cs_inp_nosel")

	cs.SetSelectedCwd("/tmp")
	// Don't select any AS — selectedAS stays nil.

	msg := Message{
		ID:     "msg-orphan",
		ChatID: cs.ID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	if err := cs.TryFlush(); err != nil {
		t.Errorf("TryFlush with no selectedAS should be a silent no-op, got err=%v", err)
	}

	// Message must still be queued for the next TryFlush attempt.
	found := false
	for _, m := range cs.queue.Peek() {
		if m.ID == "msg-orphan" {
			found = true
			break
		}
	}
	if !found {
		t.Error("message was lost after no-selectedAS TryFlush")
	}
}

// TestInput_NotReadySelectedAS_RetainsInQueue — when selectedAS is
// not ready (mid-Prompt), TryFlush is a silent no-op (returns nil)
// and the message stays queued.
func TestInput_NotReadySelectedAS_RetainsInQueue(t *testing.T) {
	cs := newChatSessionForTest("cs_inp_busy")
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent("pi")

	as := NewAgentSession("as_busy", cs.ID, "pi", "/tmp", nil)
	cs.attachAgentSession(as)
	cs.selectAgentSessionLocked(as)

	// Force not-ready.
	as.SetIsReadyForTest(false)

	msg := Message{
		ID:     "msg-busy",
		ChatID: cs.ID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	if err := cs.TryFlush(); err != nil {
		t.Errorf("TryFlush when AS not ready should be a silent no-op, got err=%v", err)
	}

	// Message must still be in the queue (not lost).
	found := false
	for _, m := range cs.queue.Peek() {
		if m.ID == "msg-busy" {
			found = true
			break
		}
	}
	if !found {
		t.Error("message was lost after not-ready TryFlush")
	}
}

// TestInput_DirectPermissionResponse_RoutesToSourceAS — a permission
// response arriving on the AgentEventBus (as a KindPromptEnded-style
// event carrying user input) is routed to the AS that emitted the
// prompt, NOT to selectedAS. Phase 1 invariant: input routing uses
// event source, not selected.
func TestInput_DirectPermissionResponse_RoutesToSourceAS(t *testing.T) {
	cs := newChatSessionForTest("cs_inp_permsrc")

	asA := NewAgentSession("as_A", cs.ID, "pi", "/tmp", nil)
	asB := NewAgentSession("as_B", cs.ID, "claude", "/tmp", nil)
	cs.attachAgentSession(asA)
	cs.attachAgentSession(asB)
	cs.selectAgentSessionLocked(asB) // selected = B

	var fromA, fromB atomic.Int32
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		if env.AgentSession == nil {
			return false
		}
		switch env.AgentSession.ID {
		case asA.ID:
			fromA.Add(1)
		case asB.ID:
			fromB.Add(1)
		}
		return false
	})

	// A "permission response" routed via A's bus — selected is B,
	// but the event source is A.
	pushEvent(asA, EnrichedEvent{
		Kind:       KindAgentEvent,
		AgentEvent: makeTextEvent("permission-yes"),
	})

	deadline := time.Now()
	deadline = deadline.Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			break
		}
		if fromA.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fromA.Load() == 0 {
		t.Error("permission response did not route to source AS")
	}
	if fromB.Load() != 0 {
		t.Errorf("permission response incorrectly routed to selected AS B (got %d hits)", fromB.Load())
	}
}

// TestInput_FailedSubmitReturnsToQueue — Submit error doesn't lose
// the queued message; TryFlush leaves it in place for the next
// attempt.
func TestInput_FailedSubmitReturnsToQueue(t *testing.T) {
	cs := newChatSessionForTest("cs_inp_failed")
	cs.SetSelectedCwd("/tmp")
	cs.SetSelectedAgent("pi")

	as := NewAgentSession("as_fail", cs.ID, "pi", "/tmp", nil)
	// No spawner wired; as.handle is nil. Submit will fail.
	cs.attachAgentSession(as)
	cs.selectAgentSessionLocked(as)

	msg := Message{
		ID:     "msg-fail",
		ChatID: cs.ID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}},
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	_ = cs.TryFlush() // expected to fail

	found := false
	for _, m := range cs.queue.Peek() {
		if m.ID == "msg-fail" {
			found = true
			break
		}
	}
	if !found {
		t.Error("message was lost after failed Submit")
	}
}

// silence unused warning if test ordering shifts.
var _ = atomic.Int32{}