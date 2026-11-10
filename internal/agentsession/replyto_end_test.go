// Package agentsession — the prompt-end anchor of an injected turn.
//
// /review hands its findings to the chat's agent as a turn of its
// own (SendBlocks(WithReplyTo(replyTo))). The settled signal of the
// /review message waits for that turn to end, and it recognises the
// turn's prompt-end by its UserMsgID. That UserMsgID comes from the
// WithReplyTo override, so the override has to survive all the way
// into the prompt-end event.
//
// Nothing else guards that: replyto_override_test.go covers the
// ENRICHMENT side (the UserMsgID stamped on events), this file
// covers the PROMPT-END side (the UserMsgID the prompt-end event
// carries).

package agentsession

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// installInjectedPrompt puts the agent session into the state
// SendBlocks(WithReplyTo) leaves it in: an in-flight prompt plus the
// replyTo override.
func installInjectedPrompt(as *AgentSession, replyTo string) *Prompt {
	p := &Prompt{ID: "p_injected", LastMessageID: "prior_user_msg"}
	as.asMu.Lock()
	as.currentPromptOverrideUserMsgID = replyTo
	as.currentPrompt = p
	as.asMu.Unlock()
	return p
}

// TestEndPrompt_AppliesReplyToOverrideToThePrompt checks that the
// prompt-end carries the injected turn's anchor — the replyTo the
// caller asked for, not the prompt's previous LastMessageID.
func TestEndPrompt_AppliesReplyToOverrideToThePrompt(t *testing.T) {
	as := makeBareAgentSession(t, "claude", "/tmp")
	p := installInjectedPrompt(as, "om_review")

	as.EndPrompt(agent.PromptEndClean)

	if p.LastMessageID != "om_review" {
		t.Fatalf("prompt LastMessageID = %q, want om_review (the replyTo override)", p.LastMessageID)
	}
}

// TestEndPrompt_AppliesReplyToOverrideOnEveryReason checks the
// override survives a non-clean end too: a turn that was stopped or
// killed still has to be recognisable by the replyTo, or the
// settled signal would wait for a prompt-end that never comes.
func TestEndPrompt_AppliesReplyToOverrideOnEveryReason(t *testing.T) {
	for _, reason := range []agent.PromptEndReason{
		agent.PromptEndClean,
		agent.PromptEndError,
		agent.PromptEndUserStopped,
		agent.PromptEndUserKilled,
		agent.PromptEndProcessDied,
	} {
		as := makeBareAgentSession(t, "claude", "/tmp")
		p := installInjectedPrompt(as, "om_review")

		as.EndPrompt(reason)

		if p.LastMessageID != "om_review" {
			t.Errorf("reason %s: prompt LastMessageID = %q, want om_review", reason, p.LastMessageID)
		}
	}
}

// TestEndPrompt_ClearsTheOverrideAfterwards checks the override does
// not leak into the next prompt: the second prompt-end must not
// carry the stale replyTo, or one injected turn would settle two
// messages.
func TestEndPrompt_ClearsTheOverrideAfterwards(t *testing.T) {
	as := makeBareAgentSession(t, "claude", "/tmp")
	installInjectedPrompt(as, "om_review")

	as.EndPrompt(agent.PromptEndClean)

	if got := as.replyToOverride(); got != "" {
		t.Fatalf("override after the prompt ended = %q, want empty", got)
	}
}

// TestClose_DeliversTheKilledPromptEnd checks that closing a
// session still delivers the prompt-end of the turn it killed.
//
// Close() stops the readpump BEFORE ending the prompt, so the
// prompt-end push races a closed stop channel. When the event was
// dropped, the message that carried the turn never settled — the
// chat layer waits on that prompt-end to close the message out.
func TestClose_DeliversTheKilledPromptEnd(t *testing.T) {
	as := NewAgentSession("as_kill", "cs_kill", "pi", "/tmp", nil)

	var got atomic.Int32
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		if ev.Kind == KindPromptEnded {
			got.Store(1)
		}
		return false
	})

	// A spawned session has a live readpump stop channel — Close()
	// closes it before ending the prompt.
	as.asMu.Lock()
	as.readpumpStop = make(chan struct{})
	as.asMu.Unlock()

	as.SetCurrentPrompt(&Prompt{ID: "p_inflight", LastMessageID: "om_inflight"})
	if err := as.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && got.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got.Load() == 0 {
		t.Fatal("the killed turn's prompt-end never reached the event bus")
	}
}
