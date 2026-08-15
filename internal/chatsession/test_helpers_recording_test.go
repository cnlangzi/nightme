// Package chatsession — extracted test helpers (Phase 1 rename protection).
//
// These helpers were originally in flushhook_test.go, which was
// renamed to .skip for Phase 1 refactor cleanup. The helpers are
// still needed by other test files (message_state_test.go).
package chatsession

import (
	"context"
	"errors"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
)

type sentBlock struct {
	blocks     []agent.ContentBlock
	userMsgIDs []string
}

type recordingAgentSession struct {
	mu     sync.Mutex
	pid    int
	events chan agent.AgentEvent
	sent   []sentBlock
	closed bool
}

func newRecordingAgentSession(pid int) *recordingAgentSession {
	return &recordingAgentSession{pid: pid, events: make(chan agent.AgentEvent, 16)}
}

func (r *recordingAgentSession) Events() <-chan agent.AgentEvent { return r.events }
func (r *recordingAgentSession) PID() int                      { return r.pid }
func (r *recordingAgentSession) Name() string                  { return "fake" }
func (r *recordingAgentSession) Mode() agent.Mode              { return agent.ModePTY }
func (r *recordingAgentSession) Command() string               { return "fake" }
func (r *recordingAgentSession) Args() []string                { return nil }
func (r *recordingAgentSession) Env() []string                 { return nil }
func (r *recordingAgentSession) Info() agent.Info {
	return agent.NewInfo("rec", agent.ModePTY, "rec", nil, nil)
}
func (r *recordingAgentSession) Detect() error { return nil }
func (r *recordingAgentSession) Start(_ context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	return r.buildLive(), nil
}

// buildLive wraps r in a *agent.Agent with a recording driver.
// Used when tests need to install the recording into
// AgentSession.handle.
func (r *recordingAgentSession) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("rec", agent.ModePTY, "rec", nil, nil),
		r.pid, r.events, &recordingDriver{inner: r})
}

// recordingDriver forwards driver calls to a recordingAgentSession.
type recordingDriver struct{ inner *recordingAgentSession }

func (d *recordingDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *recordingDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *recordingDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *recordingDriver) Stop(ctx context.Context) error { return d.inner.Stop(ctx) }
func (d *recordingDriver) Close() error                   { return d.inner.Close() }
func (r *recordingAgentSession) SendBlocks(_ context.Context, blocks []agent.ContentBlock) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("closed")
	}
	cp := make([]agent.ContentBlock, len(blocks))
	copy(cp, blocks)
	r.sent = append(r.sent, sentBlock{blocks: cp})
	return nil
}
func (r *recordingAgentSession) SendPermission(_ string) error { return nil }
func (r *recordingAgentSession) New(_ context.Context) error   { return nil }
func (r *recordingAgentSession) Stop(_ context.Context) error { return agent.ErrNotSupported }
func (r *recordingAgentSession) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.events)
	}
	return nil
}

func (r *recordingAgentSession) SentCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}
