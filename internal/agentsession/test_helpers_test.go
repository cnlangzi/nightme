package agentsession

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fakeAgentSession is a minimal agent.Agent implementation used by
// tests. It does not spawn anything; the caller injects events into
// the channel via PushEvent / FinishEvent.
type fakeAgentSession struct {
	mu     sync.Mutex
	pid    int
	events chan agent.AgentEvent
	closed bool
}

func newFakeAgentSession(pid int) *fakeAgentSession {
	return &fakeAgentSession{
		pid:    pid,
		events: make(chan agent.AgentEvent, 32),
	}
}

func (f *fakeAgentSession) Events() <-chan agent.AgentEvent { return f.events }
func (f *fakeAgentSession) PID() int                      { return f.pid }
func (f *fakeAgentSession) Info() agent.Info {
	return agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil)
}
func (f *fakeAgentSession) Detect() error { return nil }
func (f *fakeAgentSession) Start(_ context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	return f.buildLive(), nil
}

// buildLive wraps f in a *agent.Agent with a fake driver that
// forwards Send*/Reset/Close back to f. Used by fakeSpawners to
// return a *Agent from Spawn().
func (f *fakeAgentSession) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		f.pid, f.events, &fakeDriver{inner: f})
}

// fakeDriver forwards driver calls back to a fakeAgentSession.
type fakeDriver struct{ inner *fakeAgentSession }

func (d *fakeDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *fakeDriver) SendPermission(resp string) error { return d.inner.SendPermission(resp) }
func (d *fakeDriver) Reset(ctx context.Context) error    { return d.inner.New(ctx) }
func (d *fakeDriver) Close() error                      { return d.inner.Close() }
func (d *fakeDriver) Stop(_ context.Context) error       { return nil }

func (f *fakeAgentSession) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("fake: closed")
	}
	return nil
}

func (f *fakeAgentSession) SendPermission(resp string) error { return nil }
func (f *fakeAgentSession) New(_ context.Context) error      { return nil }

func (f *fakeAgentSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}

// PushEvent delivers an event to the channel (used by tests to
// simulate agent output).
func (f *fakeAgentSession) PushEvent(ev agent.AgentEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- ev
}

// FinishEvent delivers EventAgentDone and closes the channel.
func (f *fakeAgentSession) FinishEvent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- agent.AgentEvent{Kind: agent.EventAgentDone}
	f.closed = true
	close(f.events)
}

// --- callRecordingAS --------------------------------------------------

// callRecordingAS records every New() invocation. err is returned
// from New; tests that want a failing bridge set err to errInjected.
type callRecordingAS struct {
	*fakeAgentSession
	calls atomic.Int32
	err   error
}

func (c *callRecordingAS) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		c.pid, c.events,
		&callRecordingASDriver{inner: c, calls: &c.calls})
}

func (c *callRecordingAS) New(_ context.Context) error {
	c.calls.Add(1)
	return c.err
}

// callRecordingASDriver wraps a callRecordingAS for the agent.driver
// interface. Tests use Handle().Driver().(*callRecordingASDriver)
// to read c.calls.
type callRecordingASDriver struct {
	inner *callRecordingAS
	calls *atomic.Int32
}

func (d *callRecordingASDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *callRecordingASDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *callRecordingASDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *callRecordingASDriver) Close() error                   { return d.inner.Close() }
func (d *callRecordingASDriver) Stop(_ context.Context) error   { return nil }

// --- restartErrAS -----------------------------------------------------

// restartErrAS is a bridge fake whose New always returns
// agent.ErrRestartRequired. It records whether Close() was called so
// the wrapper's kill+respawn path can be verified.
type restartErrAS struct {
	*fakeAgentSession
	closed bool
}

func (r *restartErrAS) New(_ context.Context) error { return agent.ErrRestartRequired }
func (r *restartErrAS) Close() error {
	r.closed = true
	return r.fakeAgentSession.Close()
}

func (r *restartErrAS) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		r.pid, r.events,
		&restartErrASDriver{inner: r})
}

type restartErrASDriver struct{ inner *restartErrAS }

func (d *restartErrASDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *restartErrASDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *restartErrASDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *restartErrASDriver) Close() error                   { return d.inner.Close() }
func (d *restartErrASDriver) Stop(_ context.Context) error   { return nil }

// --- Spawner fakes ---------------------------------------------------

// fakeSpawner is a Spawner that returns fakeAgentSession instances
// without forking. The test can PushEvent / FinishEvent to drive
// lifecycle transitions.
type fakeSpawner struct {
	mu           sync.Mutex
	fakes        map[spawnKey]*fakeAgentSession
	calls        int
	lastResumeID string
	spawnFn      func(name, cwd string) (*agent.Agent, error) // optional override
}

type spawnKey struct {
	name string
	cwd  string
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{fakes: make(map[spawnKey]*fakeAgentSession)}
}

func (s *fakeSpawner) Spawn(ctx context.Context, name, cwd string, args []string, sessionID string) (*agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastResumeID = sessionID
	if s.spawnFn != nil {
		return s.spawnFn(name, cwd)
	}
	key := spawnKey{name, cwd}
	if f, ok := s.fakes[key]; ok {
		return f.buildLive(), nil
	}
	f := newFakeAgentSession(10000 + s.calls)
	s.fakes[key] = f
	return f.buildLive(), nil
}

func (s *fakeSpawner) Get(name, cwd string) *fakeAgentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakes[spawnKey{name, cwd}]
}

// fakeRestartSpawner is a minimal Spawner that returns a pre-built
// handle. Records the sessionID it was called with so tests can
// assert "no --resume on the fresh spawn".
type fakeRestartSpawner struct {
	handle             *agent.Agent
	calledWithResumeID string
}

func (f *fakeRestartSpawner) Spawn(_ context.Context, _, _ string, _ []string, sessionID string) (*agent.Agent, error) {
	f.calledWithResumeID = sessionID
	return f.handle, nil
}

// fakeFailingSpawner returns a non-nil error from Spawn, used to
// exercise the wrapper's spawn-failure cleanup path.
type fakeFailingSpawner struct{ err error }

func (f *fakeFailingSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	return nil, f.err
}
