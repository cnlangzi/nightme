// Read-pump regression for the F-32 long-lived agent contract:
//
// EventAgentDone marks the end of one turn; the events channel MUST
// stay open across many turns. Only process exit (channel close)
// transitions AgentSession to Exited.
//
// This file lives in package chatsession (not a black-box _test
// import) so it can poke the unexported readPump path. It
// constructs a fake AgentSession whose Events channel can be
// hand-fed by the test, then exercises the same code path
// ChatSession.runReadPump runs in production.
package chatsession

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// longLivedFakeAS is an agent.Agent whose Events channel
// delivers whatever the test writes to it. The "process" is
// long-lived: the events channel does not close until the test
// calls Close.
type longLivedFakeAS struct {
	events  chan agent.AgentEvent
	pid     int
	closeMu sync.Mutex
	closed  bool
}

func newLongLivedFakeAS() *longLivedFakeAS {
	return &longLivedFakeAS{
		events: make(chan agent.AgentEvent, 16),
		pid:    4242,
	}
}

func (a *longLivedFakeAS) Events() <-chan agent.AgentEvent { return a.events }
func (a *longLivedFakeAS) PID() int                       { return a.pid }
func (a *longLivedFakeAS) Name() string                   { return "fake" }
func (a *longLivedFakeAS) Mode() agent.Mode               { return agent.ModePTY }
func (a *longLivedFakeAS) Command() string                { return "fake" }
func (a *longLivedFakeAS) Args() []string                 { return nil }
func (a *longLivedFakeAS) Env() []string                  { return nil }
func (a *longLivedFakeAS) Detect() error                  { return nil }
func (a *longLivedFakeAS) Start(context.Context, agent.StartConfig) (agent.Agent, error) {
	return a, nil
}
func (a *longLivedFakeAS) SendText(string) error          { return nil }
func (a *longLivedFakeAS) SendBlocks(context.Context, []agent.ContentBlock) error {
	return nil
}
func (a *longLivedFakeAS) SendPermission(string) error { return nil }
func (a *longLivedFakeAS) New(context.Context) error   { return nil }
func (a *longLivedFakeAS) Close() error {
	a.closeMu.Lock()
	defer a.closeMu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	close(a.events)
	return nil
}

// push enqueues one event; non-blocking thanks to the buffered
// channel.
func (a *longLivedFakeAS) push(ev agent.AgentEvent) {
	a.events <- ev
}

// fakeSpawnerLS is a Spawner that returns our longLivedFakeAS.
type fakeSpawnerLS struct{ as agent.Agent }

func (f fakeSpawnerLS) Spawn(_ context.Context, _, _ string, _ []string, _ string) (agent.Agent, error) {
	return f.as, nil
}

// TestReadPump_ContinuesAfterEventDone verifies the F-32 contract:
// after EventAgentDone, the read pump continues to consume events from
// the same session; it does NOT transition the session to Exited.
// The session only flips to Exited when the events channel closes.
func TestReadPump_ContinuesAfterEventDone(t *testing.T) {
	cs, _ := New("oc_pi", "pi", newTestChannel())
	cs.SetActiveCwd("/tmp")
	cs.SetActiveAgent("pi")
	fake := newLongLivedFakeAS()
	cs.spawner = fakeSpawnerLS{as: fake}

	if _, err := cs.LookupActiveAgentSession(); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// CS-AS 边界重构 Phase 1: the readpump is per-AgentSession and
	// starts automatically once the handle is wired (see
	// AgentSession.startReadPump). The CS side consumes the
	// enriched stream via PumpEvents — that is what routes
	// KindLifecycle{StatusExited} into AS.SetExited, which this
	// test asserts on.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cs.PumpEvents(ctx)

	// First turn: a few events + EventAgentDone.
	fake.push(agent.AgentEvent{Kind: agent.EventAgentText, Text: "hello"})
	fake.push(agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done: &agent.AgentDoneEvent{ExitCode: 0, Reason: "settled"},
	})
	// Give the pump a chance to consume both events.
	time.Sleep(50 * time.Millisecond)

	// Second turn: more events + EventAgentDone. The read pump must
	// still be alive (it would have exited only on channel
	// close).
	fake.push(agent.AgentEvent{Kind: agent.EventAgentText, Text: "again"})
	fake.push(agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done: &agent.AgentDoneEvent{ExitCode: 0, Reason: "settled"},
	})
	time.Sleep(50 * time.Millisecond)

	// Now the "process" dies: close the channel. The pump
	// observes the close and transitions to Exited.
	fake.Close()

	// Wait for the pump to observe the close and flip the
	// AgentSession status to Exited.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cs.activeAS != nil {
			if status := cs.activeAS.Status(); status == StatusExited {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cs.activeAS == nil {
		t.Fatalf("activeAS is nil after close")
	}
	t.Fatalf("AgentSession status = %s, want Exited after channel close", cs.activeAS.Status())
}
