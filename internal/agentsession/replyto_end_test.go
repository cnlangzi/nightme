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
	"testing"

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
