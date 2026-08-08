// Shared test helpers for the F-32 follow-up tests
// (readpump_fsm_test.go, buffer_swap_test.go, etc.). Lives in
// its own file so each test file can use the helpers without
// redeclaring fakeAgentSession (which already exists in
// spawn_test.go for the existing Spawn / Kill test suite).
package chatsession

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// makeTestMessage constructs a `Message` value with the minimal
// fields a test typically needs: an ID, the owning ChatSession's
// ChatID, the supplied blocks, and a fresh ReceivedAt. Use this
// in tests that previously called
// `cs.QueueUserMessage(blocks, userMsgID)` — the signature takes
// a `Message` value and the construction is noisy enough to
// warrant a helper.
func makeTestMessage(cs *ChatSession, blocks []agent.ContentBlock, userMsgID string) Message {
	return Message{
		ID:         userMsgID,
		ChatID:     cs.ChatID,
		Blocks:     blocks,
		ReceivedAt: time.Now(),
	}
}

// newTestASWithFakeHandle wires a fresh AgentSession to a fake
// bridge handle WITHOUT going through Spawn / Spawner. Used by
// the FSM-transition tests that only care about the readPump /
// InputBuffer path, not bridge bring-up.
//
// Returns the AgentSession (already inserted into cs.pool and
// set as cs.activeAS, with handle wired and stat=Running) and
// the underlying fakeAgentSession the test can drive via
// PushEvent / FinishEvent.
func newTestASWithFakeHandle(cs *ChatSession) (*AgentSession, *fakeAgentSession) {
	as := NewAgentSession("as_test", cs.ID, "fake", "/tmp", nil)
	sess := newFakeAgentSession(1)
	as.handle = sess
	as.stat = StatusRunning
	as.pid = 1

	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: as.Agent, Cwd: as.Cwd}] = as
	cs.activeAS = as
	cs.mu.Unlock()

	return as, sess
}