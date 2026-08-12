// Shared test helpers for the F-32 follow-up tests
// (readpump_fsm_test.go, buffer_swap_test.go, etc.). Lives in
// its own file so each test file can use the helpers without
// redeclaring fakeAgentSession (which already exists in
// spawn_test.go for the existing Spawn / Kill test suite).
package chatsession

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/gateway/outbound"
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

// testEmitter is a minimal outbound.Emitter impl for in-package
// tests. Records every Send/SendCard so tests can assert what
// was emitted. PATCH semantics are encoded as Kind=OutCardPatch
// in the message itself; the Send path applies the disabled-flag
// conventions that the runtime would have set.
//
// Replaces the pre-refactor testChannel which implemented the
// chatsession.Channel interface (deleted in Commit 4); the new
// world has no chatsession.Channel — every chat session holds
// an outbound.Emitter, so the test stub is an Emitter.
type testEmitter struct {
	Sent []messages.OutboundMessage
}

func (t *testEmitter) Send(_ context.Context, msg messages.OutboundMessage) error {
	t.Sent = append(t.Sent, msg)
	return nil
}

func (t *testEmitter) SendCard(_ context.Context, msg messages.OutboundMessage) (string, error) {
	t.Sent = append(t.Sent, msg)
	return "bot-msg-test", nil
}

// pushEvent pushes an EnrichedEvent into the AS's dispatch queue
// (via the public InjectEvent helper). Test-only — production
// events come from the bridge's readpump.
func pushEvent(as *AgentSession, ev EnrichedEvent) {
	as.InjectEvent(ev)
}

// makeBareAgentSession creates a fresh AgentSession for tests that
// don't drive the full Spawn lifecycle. Equivalent to the helper of
// the same name in the agentsession test package; duplicated here
// because test files cannot share across packages.
func makeBareAgentSession(t *testing.T, agentName, cwd string) *AgentSession {
	t.Helper()
	return NewAgentSession(newAgentSessionID(), "cs_test", agentName, cwd, nil)
}

// Compile-time guard: testEmitter must satisfy outbound.Emitter so
// any signature drift in outbound is caught at test compile.
var _ outbound.Emitter = (*testEmitter)(nil)