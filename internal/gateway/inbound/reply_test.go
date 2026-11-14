package inbound

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway/inbound/teststubs"
	"github.com/cnlangzi/nightme/internal/messages"
)

// claimingStub is a ReplyClaimer that takes every message.
type claimingStub struct {
	claimed bool
	text    string
}

func (c *claimingStub) Claim(_ context.Context, msg *messages.InboundMessage) bool {
	c.claimed = true
	c.text = msg.Text
	return true
}

// TestDispatch_ReplyClaimBeatsTheCommandBranch — the answer to a
// program's question may be a slash command. The claimer takes it
// before the commander does, so the answer reaches the program and
// not the chat.
func TestDispatch_ReplyClaimBeatsTheCommandBranch(t *testing.T) {
	commander := teststubs.NewCommander()
	commander.Recognize("/gtw commit", teststubs.Result{Consumed: true, Reply: "handled"})

	claimer := &claimingStub{}
	mgr := teststubs.NewMessage(chatsession.NewManager())
	r := New(mgr, commander, teststubs.AlwaysFallThroughShell{}, teststubs.NewReaction(true), nil, "primary")
	r = r.WithReplyClaimer(claimer)

	res, err := r.Dispatch(context.Background(), mgr, &messages.InboundMessage{
		ChatID:     "oc_claim",
		Text:       "/gtw commit",
		HasMention: true,
		MessageID:  "om_test",
	})
	r.WaitExec()
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !claimer.claimed {
		t.Fatal("the claimer did not take the message")
	}
	if claimer.text != "/gtw commit" {
		t.Errorf("the claimer saw %q, want the message", claimer.text)
	}
	if commander.Calls() != 0 {
		t.Errorf("the commander was called %d times, want 0 — the answer belongs to the program", commander.Calls())
	}
	if res == nil || !res.Consumed {
		t.Errorf("the result is %+v, want a consumed message", res)
	}
}

// TestDispatch_NoClaimerLeavesTheChainAlone — a daemon without a
// claimer dispatches a message the way it always did.
func TestDispatch_NoClaimerLeavesTheChainAlone(t *testing.T) {
	commander := teststubs.NewCommander()
	commander.Recognize("/known", teststubs.Result{Consumed: true, Reply: "handled"})

	mgr := teststubs.NewMessage(chatsession.NewManager())
	r := New(mgr, commander, teststubs.AlwaysFallThroughShell{}, teststubs.NewReaction(true), nil, "primary")

	res, err := r.Dispatch(context.Background(), mgr, &messages.InboundMessage{
		ChatID:     "oc_noclaim",
		Text:       "/known",
		HasMention: true,
		MessageID:  "om_test",
	})
	r.WaitExec()
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Errorf("the result is %+v, want a consumed message", res)
	}
	if commander.Calls() != 1 {
		t.Errorf("the commander was called %d times, want 1", commander.Calls())
	}
}
