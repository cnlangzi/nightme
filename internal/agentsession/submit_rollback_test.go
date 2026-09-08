// submit_rollback_test.go — Submit failure-rollback contract.
//
// Submit installs currentPrompt and flips isReady=false BEFORE
// SendBlocks, so a fast-failing bridge can return an error while
// the commit is already in place. The wrapper must roll the
// commit back so the chat layer's next TryFlush sees a clean AS
// (no currentPrompt, IsReady=true) instead of getting stuck
// behind a phantom "busy" flag.
package agentsession

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// failingSendBlocksAS wraps fakeAgentSession and overrides
// SendBlocks to always return the configured error.
type failingSendBlocksAS struct {
	*fakeAgentSession
	err error
}

func (f *failingSendBlocksAS) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		f.pid, f.events,
		&failingSendBlocksDriver{inner: f})
}

func (f *failingSendBlocksAS) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	return f.err
}

type failingSendBlocksDriver struct{ inner *failingSendBlocksAS }

func (d *failingSendBlocksDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *failingSendBlocksDriver) SendPermission(string) error { return nil }
func (d *failingSendBlocksDriver) Reset(context.Context) error { return nil }
func (d *failingSendBlocksDriver) Close() error                { return nil }
func (d *failingSendBlocksDriver) Stop(context.Context) error  { return nil }
func (d *failingSendBlocksDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// TestSubmit_FailureLeavesCleanState asserts the rollback contract:
// when SendBlocks fails after the commit, currentPrompt must be
// nil and IsReady must be true so the chat layer can retry on the
// next TryFlush without wedging the AS in a phantom busy state.
func TestSubmit_FailureLeavesCleanState(t *testing.T) {
	as := newTestAgentSession()
	want := errors.New("bridge refused")
	bridge := &failingSendBlocksAS{fakeAgentSession: newFakeAgentSession(7777), err: want}
	handle := bridge.buildLive()
	as.asMu.Lock()
	as.handle = handle
	as.stat = StatusRunning
	as.asMu.Unlock()
	as.isReady.Store(true)

	p := &Prompt{
		ID:       "as_test-p1",
		Blocks:   []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}},
		Messages: []Message{{ID: "m_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}}},
	}
	if err := as.Submit(p); !errors.Is(err, want) {
		t.Fatalf("Submit error = %v, want %v", err, want)
	}
	if as.CurrentPrompt() != nil {
		t.Errorf("CurrentPrompt = %v, want nil after rollback", as.CurrentPrompt())
	}
	if !as.IsReady() {
		t.Error("IsReady = false after rollback, want true (next TryFlush must land)")
	}
}
