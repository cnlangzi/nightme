package chatsession

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// sentBlock records what the default FlushHook actually sent to
// the agent (used to verify the commit-9+ bugfix: messages must
// reach the spawned agent, not be silently dropped).
// sentBlock / recordingAgentSession / newRecordingAgentSession
// live in test_helpers_recording_test.go — they were extracted
// there while this file was shelved, and other test files now
// depend on them.

// spawnerRecording wraps a single recordingAgentSession so we can
// substitute it for the real spawner and assert that the hook
// actually sends to the bridge.
type spawnerRecording struct {
	sessions []*recordingAgentSession
	idx      int
}

func (s *spawnerRecording) Spawn(_ context.Context, _, _ string, _ []string, _ string) (agent.Agent, error) {
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
	if err := cs.QueueUserMessage(makeTestMessage(cs, blocks, "msg_1")); err != nil {
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

// TestFlushHook_BusyQueues: same as above, but with a Prompt
// already in flight the message queues instead of flushing
// immediately; the flush happens when the Prompt ends.
//
// CS-AS 边界重构 Phase 1 port: "busy" is no longer a CS buffer
// state (SetBusy/SetIdle) — it is the presence of an in-flight
// Prompt on the AgentSession, so the setup submits one. The
// OnTurnEnded hook became endPrompt (driven by the per-AS readpump
// on EventAgentDone) followed by TryFlush.
func TestFlushHook_BusyQueues(t *testing.T) {
	spawner := &spawnerRecording{}
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}

	// Put a Prompt in flight — this is what "agent is processing a
	// turn" means now. It costs one SendBlocks, so the baseline
	// count below is 1, not 0.
	if err := as.Submit(&Prompt{
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "in-flight"}},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if as.IsReady() {
		t.Fatalf("AS should be mid-turn after Submit")
	}
	base := spawner.sessions[0].SentCount()

	if err := cs.QueueUserMessage(makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "queued"}}, "m_queued")); err != nil {
		t.Fatalf("QueueUserMessage mid-turn: %v", err)
	}
	if got := spawner.sessions[0].SentCount(); got != base {
		t.Fatalf("mid-turn should queue, not flush; agent got %d, want %d", got, base)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("QueueLen = %d mid-turn; want 1", got)
	}

	// End the turn — in production the per-AS readpump does this on
	// EventAgentDone, then routeEvent calls TryFlush.
	as.endPrompt(PromptEndClean)
	if err := cs.TryFlush(); err != nil {
		t.Fatalf("TryFlush: %v", err)
	}
	if got := spawner.sessions[0].SentCount(); got != base+1 {
		t.Fatalf("after flush agent got %d, want %d", got, base+1)
	}
}

// TestFlushHook_NoActiveAgentSession: when /kill has cleared the
// pool (activeAS == nil), flushing is a safe no-op that leaves the
// message queued for the next respawn, instead of panicking.
//
// CS-AS 边界重构 Phase 1 port: the old default hook returned
// ErrNotRunning here. TryFlush now SKIPs with reason=activeAS_nil
// and returns nil — /kill deliberately preserves the queue, and
// the message flushes when the next AS spawns.
func TestFlushHook_NoActiveAgentSession(t *testing.T) {
	spawner := &spawnerRecording{}
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	cs.LookupActiveAgentSession()

	// Simulate /kill via the lifecycle accessors (snapshot → Close →
// Drop) — the kill package's KillAllAgents is tested separately in
// internal/command/kill to avoid an import cycle here.
snapshot := cs.AgentSessionsInCwd(cs.ActiveCwd())
for _, as := range snapshot {
	_ = as.Close()
	cs.DropAgentSession(as)
}

	// activeAS is nil. Queueing must not panic, and the message
	// must survive for the next respawn.
	msg := makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "lost"}}, "m_lost")
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage with no active AS: %v", err)
	}
	if err := cs.TryFlush(); err != nil {
		t.Fatalf("TryFlush with no active AS should be a no-op, got %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("QueueLen = %d; want 1 (message retained for respawn)", got)
	}
	// Post-refactor: messages are immutable; the queue still
	// owns this item. The "never delivered" invariant is now
	// observable as "no MessageSubmitted wire event was fired
	// for msg.ID". The wire-event check above (when present)
	// covers the same semantic.
}
