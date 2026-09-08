// restart_resume_test.go — R1.5: verify RestartFromDeath forks a
// fresh bridge and re-arms the readpump using the captured
// SessionID, without resubmitting any in-flight blocks (the
// in-flight replay was removed; the new bridge is expected to
// resume the prior session via --resume and pick up conversation
// history from the bridge's own store, but the specific turn that
// was running when the old bridge died is not replayed).
package agentsession

import (
	"context"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// recordingSendBlocksAS wraps a fakeAgentSession with a
// SendBlocks-recording driver. Pattern mirrors callRecordingAS
// from test_helpers_test.go so the test setup matches the rest
// of the package's fakes.
type recordingSendBlocksAS struct {
	*fakeAgentSession
	mu    sync.Mutex
	sent  [][]agent.ContentBlock
	inner *recordingSendBlocksDriver
}

type recordingSendBlocksDriver struct {
	inner *recordingSendBlocksAS
}

func (d *recordingSendBlocksDriver) SendBlocks(_ context.Context, b []agent.ContentBlock) error {
	d.inner.mu.Lock()
	cp := make([]agent.ContentBlock, len(b))
	copy(cp, b)
	d.inner.sent = append(d.inner.sent, cp)
	d.inner.mu.Unlock()
	return d.inner.fakeAgentSession.SendBlocks(context.Background(), b)
}
func (d *recordingSendBlocksDriver) SendPermission(string) error { return nil }
func (d *recordingSendBlocksDriver) Reset(context.Context) error {
	return d.inner.New(context.Background())
}
func (d *recordingSendBlocksDriver) Close() error               { return d.inner.Close() }
func (d *recordingSendBlocksDriver) Stop(context.Context) error { return nil }
func (d *recordingSendBlocksDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

func (r *recordingSendBlocksAS) buildLive() *agent.Agent {
	r.inner = &recordingSendBlocksDriver{inner: r}
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		r.pid, r.events,
		r.inner,
	)
}

func (r *recordingSendBlocksAS) Sent() [][]agent.ContentBlock {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]agent.ContentBlock, len(r.sent))
	copy(out, r.sent)
	return out
}

// TestRestartFromDeath_DoesNotResubmit asserts the post-removal
// contract: RestartFromDeath must NOT call SendBlocks on the
// freshly-spawned bridge. The user's in-flight prompt is lost on
// a bridge-side crash; the chat layer surfaces the next user
// message as a fresh attempt.
func TestRestartFromDeath_DoesNotResubmit(t *testing.T) {
	as := NewAgentSession(newAgentSessionID(), "cs_test", "dsh", "/code", nil)
	as.SetSessionID("old-session-id")
	as.SetPersist(func(_ *registry.AgentSessionEntry) error { return nil })
	as.asMu.Lock()
	as.stat = StatusExited
	as.pid = 9999 // pretend the old bridge died with this pid
	as.asMu.Unlock()

	newBridge := &recordingSendBlocksAS{fakeAgentSession: newFakeAgentSession(4242)}
	spawner := &fakeRestartSpawner{handle: newBridge.buildLive()}

	if err := as.RestartFromDeath(context.Background(), spawner); err != nil {
		t.Fatalf("RestartFromDeath: %v", err)
	}

	if got := as.Handle(); got == nil {
		t.Fatalf("Handle should be non-nil after RestartFromDeath")
	}
	if got := as.PID(); got != 4242 {
		t.Fatalf("PID = %d, want 4242 (new bridge)", got)
	}
	if got := as.Status(); got != StatusRunning {
		t.Fatalf("Status = %s, want StatusRunning", got)
	}

	if sent := newBridge.Sent(); len(sent) != 0 {
		t.Fatalf("SendBlocks call count = %d, want 0 (no resubmit after removal)", len(sent))
	}
	if !as.IsReady() {
		t.Fatalf("IsReady = false post-respawn, want true (idle bridge ready for next message)")
	}
}
