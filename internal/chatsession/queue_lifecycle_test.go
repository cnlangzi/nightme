// Queue-lifecycle invariants that outlived the InputBuffer FSM
// (CS-AS 边界重构 Phase 1).
//
// The FSM this behaviour used to live on — cs.inputBuffer with its
// Idle/Busy states, BufferPending / ClearBuffer / SetBusy / SetIdle
// — was deleted in Phase 1. "Busy" is now the presence of an
// in-flight Prompt on the AgentSession (as.IsReady()), and the
// queue is a plain slice on ChatSession drained by TryFlush.
//
// Two invariants from the old input_buffer_test.go /
// buffer_swap_test.go survive that rewrite and are not covered
// anywhere else, so they are ported here:
//
//  1. DropQueue empties the queue and marks each message Dropped.
//  2. Switching the active agent (/use) must NOT silently discard
//     queued messages — they flush to the NEW active AgentSession.
//
// The rest of those two files tested the FSM itself (SetIdle
// resetting a hung Busy, lazy buffer allocation, InputBuffer.Add
// capacity math) and had no subject left after the refactor.
package chatsession

import (
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestDropQueue_DropsQueuedMessages covers the explicit-clear path:
// DropQueue empties the queue and fires a MessageDropped wire
// event for each removed message.
//
// Ported from TestClearBuffer_DropsQueuedMessages. Post-refactor:
// the test no longer reads msg.Stage directly (the queue's copy
// is what gets the Stage flip; value semantics mean the test's
// local copy is untouched). The MessageStateBus event is the
// canonical signal.
func TestDropQueue_DropsQueuedMessages(t *testing.T) {
	cs, _ := New("test_dropqueue", "pi")
	cs = cs.WithSpawner(nil)
	// Observe drop events.
	var (
		mu             sync.Mutex
		droppedIDs     []string
		haveDropEvent  bool
	)
	cs.MessageStateBus.Subscribe(func(e MessageStateEvent) bool {
		if e.State != agent.MessageDropped {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		droppedIDs = append(droppedIDs, e.UserMsgID)
		haveDropEvent = true
		return false
	})

	// No selectedAS, so the TryFlush inside QueueUserMessage is a
	// no-op and the message stays queued.
	msg := makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "abandoned hi"}}, "um_abandoned")
	if err := cs.QueueUserMessage(msg); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if n := cs.QueueLen(); n != 1 {
		t.Fatalf("pre-drop: QueueLen = %d; want 1", n)
	}

	if n := cs.DropQueue(); n != 1 {
		t.Errorf("DropQueue returned %d; want 1", n)
	}
	if n := cs.QueueLen(); n != 0 {
		t.Errorf("post-drop: QueueLen = %d; want 0", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if !haveDropEvent {
		t.Fatal("MessageStateBus saw no drop event")
	}
	if len(droppedIDs) != 1 || droppedIDs[0] != msg.ID {
		t.Errorf("drop event IDs = %v, want [%s]", droppedIDs, msg.ID)
	}
}

// TestQueueSurvivesAgentSwitch is the most important invariant from
// the old commit-9 suite: switching the active AgentSession (/use)
// must NOT drop queued messages. They flush to the NEW active AS.
//
// Ported from TestChatSession_BufferSurvivesAgentSwitch. The old
// version asserted the FSM stayed Busy across the switch; the new
// equivalent is that the queue is untouched by the swap and the
// pending message lands on the agent the user switched TO.
func TestQueueSurvivesAgentSwitch(t *testing.T) {
	spawner := &spawnerRecording{}
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_use_swap", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(spawner)
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.SetSelectedAgent("claude")

	claudeAS, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession(claude): %v", err)
	}

	// claude is mid-turn, so the next user message queues.
	if err := claudeAS.Submit(&Prompt{
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "in-flight"}},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	claudeSent := spawner.sessions[0].SentCount()

	if err := cs.QueueUserMessage(makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "queued"}}, "m1")); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if n := cs.QueueLen(); n != 1 {
		t.Fatalf("expected 1 queued, got %d", n)
	}

	// /use codex — swap the active agent while the message is
	// still queued.
	cs.SetSelectedAgent("codex")
	codexAS, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession(codex): %v", err)
	}
	if codexAS.ID == claudeAS.ID {
		t.Fatalf("/use did not swap the active AgentSession")
	}

	// The swap must not have touched the queue.
	if n := cs.QueueLen(); n != 1 {
		t.Fatalf("/use dropped queued messages; QueueLen = %d, want 1", n)
	}

	// The fresh AS is ready by construction, so the queued message
	// flushes to it — not to claude.
	if err := cs.TryFlush(); err != nil {
		t.Fatalf("TryFlush after /use: %v", err)
	}
	if n := cs.QueueLen(); n != 0 {
		t.Errorf("queue not drained after /use + flush: %d", n)
	}
	if got := spawner.sessions[0].SentCount(); got != claudeSent {
		t.Errorf("queued message leaked to the OLD agent: SentCount %d -> %d", claudeSent, got)
	}

	// Verify the content landed on codex intact (the agent switch
	// did NOT silently drop it).
	codexRec := spawner.sessions[len(spawner.sessions)-1]
	codexRec.mu.Lock()
	sent := append([]sentBlock(nil), codexRec.sent...)
	codexRec.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("new agent received %d SendBlocks calls; want 1", len(sent))
	}
	if len(sent[0].blocks) != 1 || sent[0].blocks[0].Text != "queued" {
		t.Errorf("flushed content lost during /use: %+v", sent[0].blocks)
	}
}
