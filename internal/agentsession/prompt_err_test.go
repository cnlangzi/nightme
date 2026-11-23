// Package agentsession — the failure a turn carries to its prompt.
//
// The turn that fails reports the failure the upstream gave it — a
// 429 from the API — and the prompt carries it, so the layer that
// settles the message knows what the upstream said without reading
// the transcript.
//
// The failure belongs to the turn that reported it: a result frame
// that arrives after the prompt is over belongs to no turn, and the
// next prompt must not inherit it.

package agentsession

import (
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestRememberPromptErr_AFailureOfTheRunningTurnIsCarried — the
// failure the turn reported rides on the prompt that ends it.
func TestRememberPromptErr_AFailureOfTheRunningTurnIsCarried(t *testing.T) {
	as := makeBareAgentSession(t, "claude", "/tmp")
	p := installInjectedPrompt(as, "om_review")

	as.rememberPromptErr(errors.New("the turn died"))
	as.EndPrompt(agent.PromptEndClean)

	if p.Err == nil {
		t.Fatal("the prompt carries no error")
	}
	if p.Err.Error() != "the turn died" {
		t.Errorf("the prompt carries %q, want the failure the turn reported", p.Err)
	}
}

// TestRememberPromptErr_AFailureWithoutAPromptIsDropped — a result
// frame that arrives after the prompt is over belongs to no turn.
//
// The next prompt must not inherit it: the message it settles would
// carry the previous turn's failure.
func TestRememberPromptErr_AFailureWithoutAPromptIsDropped(t *testing.T) {
	as := makeBareAgentSession(t, "claude", "/tmp")

	as.rememberPromptErr(errors.New("the turn died"))

	next := &Prompt{ID: "p_next", LastMessageID: "om_next"}
	as.asMu.Lock()
	as.currentPrompt = next
	as.asMu.Unlock()
	as.EndPrompt(agent.PromptEndClean)

	if next.Err != nil {
		t.Fatalf("the prompt carries %v of a turn that was over", next.Err)
	}
}
