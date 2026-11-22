package review

// The injection refusal.
//
// When the review's findings cannot be injected — a user turn is in
// flight — the reason has to reach the user. The structured outcome
// is not a candidate: messages.OutMessageSettled never reaches a
// human. The reason rides the review's own chat line.
//
// A turn that stalled names the commands that end it (/stop,
// /close). A turn that is merely busy names nothing: it is worth
// waiting out.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/agentsession/agentstest"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// stalledDriver refuses the injection the way a stalled session
// does: the refusal names the way out.
type stalledDriver struct{}

func (d *stalledDriver) SendBlocks(context.Context, []agent.ContentBlock) error {
	return &agentsession.InjectionRefused{
		Reason: "the previous turn has made no progress for 47 minutes and the agent is not responding. " +
			"Run /stop to end it, or /close and send your message again",
		Stalled: true,
	}
}
func (d *stalledDriver) SendPermission(string) error { return nil }
func (d *stalledDriver) Reset(context.Context) error { return nil }
func (d *stalledDriver) Close() error                { return nil }
func (d *stalledDriver) Stop(context.Context) error  { return agent.ErrNotSupported }
func (d *stalledDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}
func (d *stalledDriver) Ping(context.Context) error { return nil }

// busyDriver refuses the injection the way a busy session does: the
// turn is worth waiting out.
type busyDriver struct{}

func (d *busyDriver) SendBlocks(context.Context, []agent.ContentBlock) error {
	return &agentsession.InjectionRefused{Reason: "a turn is still in flight"}
}
func (d *busyDriver) SendPermission(string) error { return nil }
func (d *busyDriver) Reset(context.Context) error { return nil }
func (d *busyDriver) Close() error                { return nil }
func (d *busyDriver) Stop(context.Context) error  { return agent.ErrNotSupported }
func (d *busyDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}
func (d *busyDriver) Ping(context.Context) error { return nil }

// runReviewWithDriver runs a review whose injection is refused by
// the given driver, and returns the chat's recording emitter.
func runReviewWithDriver(t *testing.T, drv agentDriver, chatID string) *recordingEmitter {
	t.Helper()
	agents := newTestAgents(t)
	starter := &reviewFanoutStarter{
		name:   "review-stall-agent",
		groups: []agent.ReviewPlanGroup{{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}}},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate(chatID, starter.name)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	// The chat's selected cwd and the agent session's cwd are the
	// same directory: that is the key the chat resolves the agent
	// session by.
	dir := t.TempDir()
	if err := cs.SetSelectedCwd(dir); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithEmitter(em)
	as := chatsession.NewAgentSession("as-"+chatID, cs.ID, starter.name, dir, nil)
	agentstest.SetHandle(as, liveAgent(starter.name, drv))
	agentstest.SetStatus(as, agentsession.StatusRunning)
	cs.AttachAgentSessionForTest(as)
	cs.SelectedAgentSessionForTest(as)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")
	waitOutcome(t, em, 5*time.Second)
	return em
}

// TestHandle_StalledInjectionNamesTheWayOut — the refusal reaches
// the chat: the user learns that /stop or /close ends the turn that
// blocked the delivery.
func TestHandle_StalledInjectionNamesTheWayOut(t *testing.T) {
	em := runReviewWithDriver(t, &stalledDriver{}, "review-stall-chat")

	found := false
	for _, msg := range em.messages() {
		if msg.Kind != messages.OutReply {
			continue
		}
		if strings.Contains(msg.Text, "/stop") && strings.Contains(msg.Text, "/close") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no chat line named /stop and /close: %+v", em.messages())
	}
}

// TestHandle_BusyInjectionNamesNoCommand — a busy turn is worth
// waiting out, so the review does not send the user chasing a
// command. It still says the findings did not reach the agent: the
// verdict line reports the review, and the delivery line reports the
// delivery.
func TestHandle_BusyInjectionNamesNoCommand(t *testing.T) {
	em := runReviewWithDriver(t, &busyDriver{}, "review-busy-chat")

	said := false
	for _, msg := range em.messages() {
		if strings.Contains(msg.Text, "/stop") {
			t.Fatalf("a busy turn produced a line naming /stop: %q", msg.Text)
		}
		if msg.Kind == messages.OutReply && strings.Contains(msg.Text, "not delivered") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the busy refusal told the user nothing: %+v", em.messages())
	}
}
