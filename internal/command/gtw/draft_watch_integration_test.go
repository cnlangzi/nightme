package gtw

// Integration tests for the draft watcher /gtw new arms: the real
// factory drives RunNewDraft against the capture-agent rig
// (agent_rig_test.go), the test writes the drafts the agent would
// have written, ends the turn on the prompt-end bus, and reads the
// render back from the recording emitter.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// renders returns the captured messages that are draft renders
// anchored at msgID: an OutReply whose body leads with the rule that
// introduces the first draft block.
func renders(r *recordingCh, msgID string) []messages.OutboundMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []messages.OutboundMessage
	for _, m := range r.sends {
		if m.Kind != messages.OutReply || m.ReplyTo != msgID {
			continue
		}
		if strings.HasPrefix(m.Text, "---\n") {
			out = append(out, m)
		}
	}
	return out
}

// waitRender polls until one render anchored at msgID arrived.
func waitRender(t *testing.T, r *recordingCh, msgID string) messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := renders(r, msgID); len(got) > 0 {
			return got[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no draft render arrived after the turn ended")
	return messages.OutboundMessage{}
}

// settledMsg returns the settled outcome of the message, or nil
// while it has not settled.
func settledMsg(r *recordingCh, msgID string) *outcome.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.sends {
		if m.Kind == messages.OutMessageSettled && m.ReplyTo == msgID {
			return m.Outcome
		}
	}
	return nil
}

func waitSettledMsg(t *testing.T, r *recordingCh, msgID string) *outcome.Outcome {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := settledMsg(r, msgID); got != nil {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the message did not settle when the turn ended")
	return nil
}

// TestFactory_New_RendersDraftsWhenTheTurnEnds pins the flow the
// watcher exists for: /gtw new dispatches, the agent's turn ends
// cleanly, and nightme renders the drafts from disk into the chat,
// anchored at the turn's message. The message itself settles
// normally — the watcher neither holds its work nor swallows the
// prompt-end event.
func TestFactory_New_RendersDraftsWhenTheTurnEnds(t *testing.T) {
	rig := newMaterialRig(t, "chat-render-1")
	rig.send(t, "/gtw new")
	if turns := rig.cap.sent(); len(turns) != 1 {
		t.Fatalf("the agent received %d turns, want 1", len(turns))
	}

	dir := rig.draftDir(t)
	raw1 := "# feat(a): first\n\nbody one\n"
	raw2 := "# feat(b): second\n\nbody two\n"
	writeDraft(t, dir, "01.md", raw1)
	writeDraft(t, dir, "02.md", raw2)

	publishCleanEnd(rig.cs, "msg-1")

	out := waitRender(t, rig.ch, "msg-1")
	want := "---\n01.md\n" + raw1 + "\n---\n02.md\n" + raw2 + "\n---\n" + newIssueHint + "\n"
	if out.Text != want {
		t.Errorf("render body =\n%q\nwant\n%q", out.Text, want)
	}
	if got := waitSettledMsg(t, rig.ch, "msg-1"); got.Status != http.StatusOK {
		t.Errorf("settled status = %d, want ok — a clean turn settles normally", got.Status)
	}
}

// TestFactory_New_AgentQuestionTurnRendersNothing covers the turn
// that ends without drafts: the agent asked its blocking questions,
// the directory is empty, and the render stays silent.
func TestFactory_New_AgentQuestionTurnRendersNothing(t *testing.T) {
	rig := newMaterialRig(t, "chat-render-2")
	rig.send(t, "/gtw new")

	publishCleanEnd(rig.cs, "msg-1")

	waitSettledMsg(t, rig.ch, "msg-1")
	time.Sleep(200 * time.Millisecond)
	if got := renders(rig.ch, "msg-1"); len(got) != 0 {
		t.Fatalf("an empty draft directory rendered %d messages, want 0", len(got))
	}
}

// TestFactory_New_ReplacesTheWatcherOnTheNextRun pins the watcher
// lifecycle: the next /gtw new disarms the previous watcher, so one
// turn end produces exactly one render.
func TestFactory_New_ReplacesTheWatcherOnTheNextRun(t *testing.T) {
	rig := newMaterialRig(t, "chat-render-3")
	raw := "# feat(a): first\n\nbody\n"

	rig.send(t, "/gtw new")
	writeDraft(t, rig.draftDir(t), "01.md", raw)
	publishCleanEnd(rig.cs, "msg-1")
	waitRender(t, rig.ch, "msg-1")

	// The second run resets the directory and replaces the watcher.
	rig.send(t, "/gtw new")
	writeDraft(t, rig.draftDir(t), "01.md", raw)
	publishCleanEnd(rig.cs, "msg-1")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := renders(rig.ch, "msg-1"); len(got) >= 2 {
			time.Sleep(200 * time.Millisecond)
			if n := len(renders(rig.ch, "msg-1")); n != 2 {
				t.Fatalf("two /gtw new runs rendered %d messages for one turn end each, want 2", n)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the second run did not render: %d renders, want 2", len(renders(rig.ch, "msg-1")))
}

// TestFactory_New_FailedRunArmsNoWatcher covers the failure paths:
// a /gtw new that never reaches the dispatch leaves no watcher
// behind, so a later turn end renders nothing.
func TestFactory_New_FailedRunArmsNoWatcher(t *testing.T) {
	rig := newMaterialRig(t, "chat-render-4")
	// Point the chat outside any git repository: RunNewDraft fails
	// its first precheck and returns before arming.
	if err := rig.cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	rig.send(t, "/gtw new")
	if turns := rig.cap.sent(); len(turns) != 0 {
		t.Fatalf("the failed run still dispatched %d turns, want 0", len(turns))
	}

	publishCleanEnd(rig.cs, "msg-1")
	time.Sleep(200 * time.Millisecond)
	if got := renders(rig.ch, "msg-1"); len(got) != 0 {
		t.Fatalf("a failed /gtw new rendered %d messages, want 0", len(got))
	}
}

// TestFactory_New_LaterTurnsRender covers the watcher's residence:
// the agent asked questions in its first turn (no drafts), the user
// answered, and the follow-up turn that wrote the drafts renders
// them without a second /gtw new.
func TestFactory_New_LaterTurnsRender(t *testing.T) {
	rig := newMaterialRig(t, "chat-render-5")
	rig.send(t, "/gtw new")

	// The question turn: no drafts, nothing renders.
	publishCleanEnd(rig.cs, "msg-1")
	waitSettledMsg(t, rig.ch, "msg-1")
	time.Sleep(200 * time.Millisecond)
	if got := renders(rig.ch, "msg-1"); len(got) != 0 {
		t.Fatalf("the question turn rendered %d messages, want 0", len(got))
	}

	// The answer turn writes the drafts; the same watcher renders
	// them.
	raw := "# feat(a): first\n\nbody\n"
	writeDraft(t, rig.draftDir(t), "01.md", raw)
	publishCleanEnd(rig.cs, "msg-1")

	out := waitRender(t, rig.ch, "msg-1")
	want := "---\n01.md\n" + raw + "\n---\n" + newIssueHint + "\n"
	if out.Text != want {
		t.Errorf("render body =\n%q\nwant\n%q", out.Text, want)
	}
}
