package agentsession

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// fakePersist captures the most recent entry passed to the persist
// callback so we can assert what Submit / endPrompt write to disk.
type fakePersist struct {
	got   *registry.AgentSessionEntry
	err   error
	calls int
}

func (f *fakePersist) cb(e *registry.AgentSessionEntry) error {
	f.calls++
	f.got = e
	return f.err
}

// makeReadyAS sets up an AgentSession whose handle.SendBlocks can
// be configured to succeed or fail. Uses fakeAgentSession directly
// since callRecordingAS's calls counter isn't wired into SendBlocks.
func makeReadyAS(t *testing.T, sendErr error) (*AgentSession, *fakePersist) {
	t.Helper()
	as := newTestAgentSession()
	fp := &fakePersist{}
	as.SetPersist(fp.cb)
	// Wrap a fakeAgentSession whose SendBlocks returns the requested
	// error (or nil on success).
	fake := &errFakeAgentSession{
		fakeAgentSession: newFakeAgentSession(4242),
		sendErr:          sendErr,
	}
	handle := fake.buildLive()
	as.asMu.Lock()
	as.handle = handle
	as.stat = StatusRunning
	as.asMu.Unlock()
	as.isReady.Store(true)
	return as, fp
}

// errFakeAgentSession is fakeAgentSession + a configurable SendBlocks error.
type errFakeAgentSession struct {
	*fakeAgentSession
	sendErr error
}

func (e *errFakeAgentSession) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		e.pid, e.events,
		&errDriver{inner: e})
}

func (e *errFakeAgentSession) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("fake: closed")
	}
	return e.sendErr
}

// errDriver implements the driver interface by delegating to the
// inner errFakeAgentSession so the configurable SendBlocks error
// surfaces. All other methods are pass-throughs.
type errDriver struct {
	inner *errFakeAgentSession
}

func (d *errDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}

func (d *errDriver) SendPermission(resp string) error                              { return nil }
func (d *errDriver) Reset(ctx context.Context) error                              { return nil }
func (d *errDriver) Close() error                                                  { return nil }
func (d *errDriver) Stop(ctx context.Context) error                                { return nil }
func (d *errDriver) SetModel(ctx context.Context, providerID, modelID string) error { return nil }

func TestSubmit_PersistsInFlightMessages(t *testing.T) {
	as, fp := makeReadyAS(t, nil)

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	p := &Prompt{
		ID: "as_test-p1",
		Messages: []Message{
			{ID: "m_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "first"}}, ReceivedAt: now},
			{ID: "m_2", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "second"}}, ReceivedAt: now.Add(time.Second)},
		},
		Blocks:        []agent.ContentBlock{{Type: agent.ContentText, Text: "merged"}},
		LastMessageID: "m_2",
	}
	if err := as.Submit(p); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if fp.calls != 1 {
		t.Errorf("persist calls = %d, want 1", fp.calls)
	}
	if fp.got == nil || len(fp.got.InFlightMessages) != 2 {
		t.Fatalf("persisted InFlightMessages = %v, want 2 entries", fp.got)
	}
	if fp.got.InFlightMessages[0].ID != "m_1" || fp.got.InFlightMessages[1].ID != "m_2" {
		t.Errorf("persisted ids = [%s, %s], want [m_1, m_2]",
			fp.got.InFlightMessages[0].ID, fp.got.InFlightMessages[1].ID)
	}
	if as.CurrentPrompt() == nil {
		t.Error("CurrentPrompt nil after successful Submit")
	}
}

func TestSubmit_FailureLeavesInFlightEmpty(t *testing.T) {
	as, fp := makeReadyAS(t, errors.New("bridge refused"))

	p := &Prompt{
		ID: "as_test-p1",
		Messages: []Message{
			{ID: "m_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}},
		},
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}},
	}
	if err := as.Submit(p); err == nil {
		t.Fatal("Submit should fail when SendBlocks fails")
	}
	if fp.calls != 0 {
		t.Errorf("persist called %d times on failure, want 0", fp.calls)
	}
	entry := as.Entry()
	if len(entry.InFlightMessages) != 0 {
		t.Errorf("entry.InFlightMessages = %v, want empty after SendBlocks failure",
			entry.InFlightMessages)
	}
	if as.CurrentPrompt() != nil {
		t.Errorf("CurrentPrompt = %v, want nil after SendBlocks failure", as.CurrentPrompt())
	}
}

func TestEndPrompt_ClearsInFlightMessages(t *testing.T) {
	as, fp := makeReadyAS(t, nil)

	now := time.Now()
	p := &Prompt{
		ID:            "as_test-p1",
		Messages:      []Message{{ID: "m_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}, ReceivedAt: now}},
		Blocks:        []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}},
		CreatedAt:     now,
		LastMessageID: "m_1",
	}
	if err := as.Submit(p); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	preCalls := fp.calls

	as.EndPromptForTest(PromptEndClean)

	if fp.calls != preCalls+1 {
		t.Errorf("persist calls = %d, want %d (1 more after endPrompt)", fp.calls, preCalls+1)
	}
	entry := as.Entry()
	if len(entry.InFlightMessages) != 0 {
		t.Errorf("entry.InFlightMessages = %v, want empty after endPrompt",
			entry.InFlightMessages)
	}
	if as.CurrentPrompt() != nil {
		t.Errorf("CurrentPrompt = %v, want nil after endPrompt", as.CurrentPrompt())
	}
}

func TestEndPrompt_IdempotentWhenNoPrompt(t *testing.T) {
	as, fp := makeReadyAS(t, nil)
	preCalls := fp.calls

	// Calling endPrompt with no in-flight prompt must not persist
	// (the early-return path skips persist).
	as.EndPromptForTest(PromptEndClean)

	if fp.calls != preCalls {
		t.Errorf("persist calls = %d, want %d (no change for early-return)",
			fp.calls, preCalls)
	}
}

func TestSubmit_NilPersistIsSafe(t *testing.T) {
	// When persist is unwired (no CS attached yet), Submit must
	// still commit currentPrompt + inFlightMessages without panicking.
	as := newTestAgentSession()
	fake := &fakeAgentSession{}
	handle := fake.buildLive()
	as.asMu.Lock()
	as.handle = handle
	as.stat = StatusRunning
	as.asMu.Unlock()
	as.isReady.Store(true)
	// Note: SetPersist NOT called.

	p := &Prompt{
		ID: "as_test-p1",
		Messages: []Message{{ID: "m_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}}},
		Blocks:   []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}},
	}
	if err := as.Submit(p); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if as.CurrentPrompt() == nil {
		t.Error("CurrentPrompt nil after Submit despite nil persist")
	}
	if len(as.Entry().InFlightMessages) != 1 {
		t.Error("InFlightMessages not populated despite nil persist")
	}
}
