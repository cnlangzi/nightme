package chatsession

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// sentBlock records what the default FlushHook actually sent to
// the agent (used to verify the commit-9+ bugfix: messages must
// reach the spawned agent, not be silently dropped).
type sentBlock struct {
	blocks    []agent.ContentBlock
	userMsgIDs []string
}

type recordingAgentSession struct {
	mu       sync.Mutex
	pid      int
	events   chan agent.AgentEvent
	sent     []sentBlock
	closed   bool
}

func newRecordingAgentSession(pid int) *recordingAgentSession {
	return &recordingAgentSession{pid: pid, events: make(chan agent.AgentEvent, 16)}
}

func (r *recordingAgentSession) Events() <-chan agent.AgentEvent { return r.events }
func (r *recordingAgentSession) PID() int                      { return r.pid }
func (r *recordingAgentSession) SendText(_ string) error       { return nil }
func (r *recordingAgentSession) SendBlocks(_ context.Context, blocks []agent.ContentBlock) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("closed")
	}
	// record (a copy for race safety)
	cp := make([]agent.ContentBlock, len(blocks))
	copy(cp, blocks)
	r.sent = append(r.sent, sentBlock{blocks: cp})
	return nil
}
func (r *recordingAgentSession) SendPermission(_ string) error  { return nil }
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

// spawnerRecording wraps a single recordingAgentSession so we can
// substitute it for the real spawner and assert that the hook
// actually sends to the bridge.
type spawnerRecording struct {
	sessions []*recordingAgentSession
	idx      int
}

func (s *spawnerRecording) Spawn(_ context.Context, _, _ string, _ []string, _ string) (agent.AgentSession, error) {
	as := newRecordingAgentSession(42000 + s.idx)
	s.sessions = append(s.sessions, as)
	s.idx++
	return as, nil
}

// TestFlushHook_DefaultDeliversToAgent: regression test for the
// commit-9+ bug where InputBuffer had no default FlushHook. With
// /cwd then "hi", the agent must see "hi" (not silently dropped).
func TestFlushHook_DefaultDeliversToAgent(t *testing.T) {
	spawner := &spawnerRecording{}
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")

	// Simulate the daemon path: GetOrCreate → LookupActiveAgentSession
	// (spawns via spawner) → QueueUserMessage (Idle flush).
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}

	blocks := []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}}
	if err := cs.QueueUserMessage(blocks, "msg_1"); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	// The default FlushHook should have forwarded to the spawned
	// agent's SendBlocks. Verify.
	if got := spawner.sessions[0].SentCount(); got != 1 {
		t.Fatalf("agent received %d SendBlocks calls; want 1 (the bug: 0 means silent drop)", got)
	}
	if as.Handle() == nil {
		t.Fatalf("active AS has no handle after spawn")
	}
}

// TestFlushHook_BusyQueues: same as above but Busy state queues
// instead of flushing immediately; flush happens via SetIdle +
// OnTurnEnded (which the readPump drives in production).
func TestFlushHook_BusyQueues(t *testing.T) {
	spawner := &spawnerRecording{}
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()

	cs.SetBusy() // simulate "agent is processing a turn"
	if err := cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "queued"}}, "m_queued"); err != nil {
		t.Fatalf("QueueUserMessage Busy: %v", err)
	}
	if got := spawner.sessions[0].SentCount(); got != 0 {
		t.Fatalf("Busy should queue, not flush; agent got %d, want 0", got)
	}

	cs.SetIdle()
	if err := cs.OnTurnEnded(); err != nil {
		t.Fatalf("OnTurnEnded: %v", err)
	}
	if got := spawner.sessions[0].SentCount(); got != 1 {
		t.Fatalf("after flush agent got %d, want 1", got)
	}
}

// TestFlushHook_NoActiveAgentSession: when /kill has cleared the
// pool (activeAS == nil), the default hook returns ErrNotRunning
// instead of panicking.
func TestFlushHook_NoActiveAgentSession(t *testing.T) {
	spawner := &spawnerRecording{}
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()

	cs.KillAll()
	// activeAS is nil. Queue a message BEFORE OnTurnEnded; this
	// produces a buffer entry to flush. With no active AS, the
	// default hook must return ErrNotRunning.
	cs.SetBusy()
	if err := cs.QueueUserMessage(
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "lost"}}, "m_lost"); err != nil {
		t.Fatalf("QueueUserMessage Busy: %v", err)
	}
	cs.SetIdle()
	if err := cs.OnTurnEnded(); err == nil {
		t.Fatalf("OnTurnEnded with no active AS should return ErrNotRunning, got nil")
	}
}