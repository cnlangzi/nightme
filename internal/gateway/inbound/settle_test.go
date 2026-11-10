package inbound

import (
	"context"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/gateway/inbound/teststubs"
	"github.com/cnlangzi/nightme/internal/messages"
)

// settleEmitter records the outbound messages the router emits.
type settleEmitter struct {
	mu  sync.Mutex
	out []messages.OutboundMessage
}

func (e *settleEmitter) Send(_ context.Context, msg messages.OutboundMessage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.out = append(e.out, msg)
	return nil
}

func (e *settleEmitter) record() []messages.OutboundMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]messages.OutboundMessage(nil), e.out...)
}

// settleCommander is a command that reports a canned outcome.
type settleCommander struct {
	outcome *messages.Outcome
	reply   string
}

func (c *settleCommander) Dispatch(ctx context.Context, _ command.RuntimeServices, _ *chatsession.Manager, _ *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, bool, error) {
	if c.outcome != nil {
		chatsession.ReportOutcome(ctx, *c.outcome)
	}
	return &command.SlashOutput{Consumed: true, Reply: c.reply}, true, nil
}

func (c *settleCommander) Match(_ string) (string, bool) { return "", true }

// runSettleRouter drives one inbound through the router and waits
// for the async command dispatch to finish.
func runSettleRouter(t *testing.T, cmd CommandDispatcher, em *settleEmitter) []messages.OutboundMessage {
	t.Helper()
	mgr := chatsession.NewManager()
	mgr.WithEmitter(em)
	r := New(mgr, cmd, teststubs.AlwaysFallThroughShell{}, teststubs.NewReaction(false), em, "primary")

	if _, err := r.Dispatch(context.Background(), mgr, &messages.InboundMessage{
		ChatID:    "oc_settle",
		MessageID: "om_settle_1",
		UserID:    "u_test",
		Text:      "/settle",
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	r.WaitExec()
	return em.record()
}

// TestRunCommand_SettledArrivesAfterReply checks that the settled
// message follows the command's reply.
func TestRunCommand_SettledArrivesAfterReply(t *testing.T) {
	em := &settleEmitter{}
	out := runSettleRouter(t, &settleCommander{reply: "done"}, em)

	replyAt, settledAt := -1, -1
	for i, m := range out {
		if m.Kind == messages.OutCommandReply && m.Text == "done" {
			replyAt = i
		}
		if m.Kind == messages.OutMessageSettled {
			settledAt = i
		}
	}
	if replyAt < 0 {
		t.Fatalf("no command reply in %d messages", len(out))
	}
	if settledAt < 0 {
		t.Fatalf("no settled message in %d messages", len(out))
	}
	if settledAt < replyAt {
		t.Errorf("settled arrived before the reply (reply at %d, settled at %d)", replyAt, settledAt)
	}
	for _, m := range out {
		if m.Kind == messages.OutMessageSettled && m.ReplyTo != "om_settle_1" {
			t.Errorf("settled ReplyTo = %q, want om_settle_1", m.ReplyTo)
		}
	}
}

// TestRunCommand_SettledUnknownWhenNothingReported checks that a
// command that reports nothing settles as unknown.
func TestRunCommand_SettledUnknownWhenNothingReported(t *testing.T) {
	em := &settleEmitter{}
	out := runSettleRouter(t, &settleCommander{reply: "done"}, em)

	var settled *messages.OutboundMessage
	for i, m := range out {
		if m.Kind == messages.OutMessageSettled {
			settled = &out[i]
		}
	}
	if settled == nil {
		t.Fatal("no settled message")
	}
	if settled.Outcome == nil || settled.Outcome.Status != messages.OutcomeUnknown {
		t.Errorf("settled outcome = %+v, want unknown", settled.Outcome)
	}
}

// TestRunCommand_SettledCarriesReportedOutcome checks that the
// outcome a command reported reaches the settled message.
func TestRunCommand_SettledCarriesReportedOutcome(t *testing.T) {
	em := &settleEmitter{}
	out := runSettleRouter(t, &settleCommander{
		reply: "pushed",
		outcome: &messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "pushed 3 commits",
			Data:    map[string]string{"branch": "feat/x"},
		},
	}, em)

	var settled *messages.OutboundMessage
	for i, m := range out {
		if m.Kind == messages.OutMessageSettled {
			settled = &out[i]
		}
	}
	if settled == nil {
		t.Fatal("no settled message")
	}
	if settled.Outcome == nil {
		t.Fatal("settled message has no outcome")
	}
	if settled.Outcome.Status != messages.OutcomeOK {
		t.Errorf("settled status = %s, want ok", settled.Outcome.Status)
	}
	if settled.Outcome.Data["branch"] != "feat/x" {
		t.Errorf("settled data = %v, want branch=feat/x", settled.Outcome.Data)
	}
}
