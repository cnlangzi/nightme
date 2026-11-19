package gtw

// Unit tests for the draft watcher /gtw new arms: the render
// trigger (clean turn end + non-empty + changed fingerprint), the
// render body, the ⚠️ annotation of invalid drafts, the retry after
// a failed send, the lenient directory read, and the bus etiquette
// (never consume the event, never block the publish pump).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// watchEmitter records sent messages and can be told to fail or to
// block inside Send (to prove the publish pump is not held).
type watchEmitter struct {
	mu    sync.Mutex
	sends []messages.OutboundMessage
	err   error
	block chan struct{}
}

func (e *watchEmitter) Send(_ context.Context, m messages.OutboundMessage) error {
	if e.block != nil {
		<-e.block
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.sends = append(e.sends, m)
	return nil
}

func (e *watchEmitter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sends)
}

// newWatchTestRig is a chat session with a recording emitter and a
// temp draft directory; the watcher under test is armed on it by the
// caller.
func newWatchTestRig(t *testing.T) (*chatsession.ChatSession, *watchEmitter, string) {
	t.Helper()
	cs, err := chatsession.New("chat-watch-"+t.Name(), "test-agent")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	em := &watchEmitter{}
	cs.WithEmitter(em)
	return cs, em, t.TempDir()
}

func writeDraft(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func publishCleanEnd(cs *chatsession.ChatSession, userMsgID string) bool {
	return cs.PromptEndBus.Publish(chatsession.PromptEndedEvent{
		UserMsgID: userMsgID,
		Reason:    agent.PromptEndClean,
	})
}

// waitSends polls until the emitter captured n messages and returns
// them. Renders run on their own goroutine, so a plain read after
// Publish would race the render.
func waitSends(t *testing.T, em *watchEmitter, n int) []messages.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		em.mu.Lock()
		if len(em.sends) >= n {
			out := append([]messages.OutboundMessage(nil), em.sends...)
			em.mu.Unlock()
			return out
		}
		em.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the emitter captured %d messages, want %d", em.count(), n)
	return nil
}

// settleNoSend proves a publish produced no render: give the render
// goroutine a window to run, then assert the count.
func settleNoSend(t *testing.T, em *watchEmitter, want int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := em.count(); got != want {
		t.Fatalf("the emitter captured %d messages, want %d", got, want)
	}
}

func TestDraftWatcher_RendersPendingDrafts(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	raw1 := "# feat(a): first\n\nbody one\n"
	raw2 := "# feat(b): second\n\nbody two\n"
	writeDraft(t, dir, "01.md", raw1)
	writeDraft(t, dir, "02.md", raw2)
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	publishCleanEnd(cs, "u1")

	sends := waitSends(t, em, 1)
	out := sends[0]
	if out.Kind != messages.OutReply {
		t.Errorf("kind = %v, want OutReply", out.Kind)
	}
	if out.ChatID != cs.ChatID {
		t.Errorf("chatID = %q, want %q", out.ChatID, cs.ChatID)
	}
	if out.ReplyTo != "u1" {
		t.Errorf("ReplyTo = %q, want the ended turn's message id u1", out.ReplyTo)
	}
	want := "01.md\n" + raw1 + "\n02.md\n" + raw2
	if out.Text != want {
		t.Errorf("render body =\n%q\nwant\n%q", out.Text, want)
	}
	if strings.Contains(out.Text, "Reply with changes") {
		t.Error("the render carries the closing hint; the agent says it, nightme does not")
	}
}

func TestDraftWatcher_SkipsUnchangedDigest(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	writeDraft(t, dir, "01.md", "# feat(a): first\n\nbody\n")
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	publishCleanEnd(cs, "u1")
	waitSends(t, em, 1)

	// Same content again: the fingerprint is unchanged, nothing
	// goes out.
	publishCleanEnd(cs, "u2")
	settleNoSend(t, em, 1)

	// A changed draft set renders again.
	writeDraft(t, dir, "02.md", "# feat(b): second\n\nbody\n")
	publishCleanEnd(cs, "u3")
	sends := waitSends(t, em, 2)
	if !strings.Contains(sends[1].Text, "02.md") {
		t.Errorf("the re-render missed the new draft:\n%s", sends[1].Text)
	}
}

func TestDraftWatcher_EmptyDirectoryRendersNothing(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	publishCleanEnd(cs, "u1")
	settleNoSend(t, em, 0)
}

func TestDraftWatcher_UncleanTurnRendersNothing(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	writeDraft(t, dir, "01.md", "# feat(a): first\n\nbody\n")
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	cs.PromptEndBus.Publish(chatsession.PromptEndedEvent{
		UserMsgID: "u1",
		Reason:    agent.PromptEndUserStopped,
	})
	settleNoSend(t, em, 0)

	// The fingerprint was not advanced: the next clean turn renders
	// the same content.
	publishCleanEnd(cs, "u2")
	waitSends(t, em, 1)
}

func TestDraftWatcher_InvalidDraftIsAnnotated(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	raw := "no heading here\n\nbody\n"
	writeDraft(t, dir, "01.md", raw)
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	publishCleanEnd(cs, "u1")

	sends := waitSends(t, em, 1)
	want := "⚠️ 01.md: first line must be `# <issue title>`\n01.md\n" + raw
	if sends[0].Text != want {
		t.Errorf("render body =\n%q\nwant\n%q", sends[0].Text, want)
	}
}

func TestDraftWatcher_FailedSendRetriesNextTurn(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	writeDraft(t, dir, "01.md", "# feat(a): first\n\nbody\n")
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	em.err = errors.New("channel down")
	publishCleanEnd(cs, "u1")
	settleNoSend(t, em, 0)

	// The fingerprint was not advanced: with the channel back, the
	// next clean turn sends the same drafts.
	em.err = nil
	publishCleanEnd(cs, "u2")
	waitSends(t, em, 1)
}

func TestDraftWatcher_IgnoresNonDraftEntries(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	raw := "# feat(a): first\n\nbody\n"
	writeDraft(t, dir, "01.md", raw)
	writeDraft(t, dir, "notes.txt", "stray\n")
	writeDraft(t, dir, "02.created", "#41 done\n")
	if err := os.Mkdir(filepath.Join(dir, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	publishCleanEnd(cs, "u1")

	sends := waitSends(t, em, 1)
	want := "01.md\n" + raw
	if sends[0].Text != want {
		t.Errorf("render body =\n%q\nwant\n%q", sends[0].Text, want)
	}

	// The creation path stays strict: the same directory still
	// fails /gtw new -y's read.
	if _, err := loadIssueDrafts(dir); err == nil || !strings.Contains(err.Error(), "unexpected entry") {
		t.Errorf("loadIssueDrafts err = %v, want an unexpected-entry error", err)
	}
}

func TestDraftWatcher_HandlerNeverConsumesTheEvent(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	// A later subscriber still sees the event: the watcher must not
	// swallow what the message's own settle waits for.
	later := make(chan struct{}, 1)
	cs.PromptEndBus.Subscribe(func(chatsession.PromptEndedEvent) bool {
		later <- struct{}{}
		return false
	})

	if consumed := publishCleanEnd(cs, "u1"); consumed {
		t.Error("the event was consumed; the message's hold-until-prompt-end would starve")
	}
	select {
	case <-later:
	case <-time.After(time.Second):
		t.Fatal("a subscriber registered after the watcher did not see the event")
	}
	settleNoSend(t, em, 0)
}

func TestDraftWatcher_PublishIsNotBlockedByTheRender(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	writeDraft(t, dir, "01.md", "# feat(a): first\n\nbody\n")
	em.block = make(chan struct{})
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	done := make(chan struct{})
	go func() {
		publishCleanEnd(cs, "u1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on the render; the pump must not wait for the channel")
	}
	close(em.block)
	waitSends(t, em, 1)
}

func TestDraftWatcher_SurvivesCommandContextCancel(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	writeDraft(t, dir, "01.md", "# feat(a): first\n\nbody\n")
	ctx, cancel := context.WithCancel(context.Background())
	newDraftWatcher(ctx, cs, cs.ChatID, dir)
	cancel()

	publishCleanEnd(cs, "u1")
	waitSends(t, em, 1)
}

func TestDraftWatcher_NilEmitterSkips(t *testing.T) {
	cs, err := chatsession.New("chat-watch-no-em-"+t.Name(), "test-agent")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	dir := t.TempDir()
	writeDraft(t, dir, "01.md", "# feat(a): first\n\nbody\n")
	newDraftWatcher(context.Background(), cs, cs.ChatID, dir)

	// No emitter: the render is skipped silently, no panic.
	publishCleanEnd(cs, "u1")
	time.Sleep(100 * time.Millisecond)
}

func TestManager_ArmDraftsReplacesTheWatcher(t *testing.T) {
	cs, em, dir := newWatchTestRig(t)
	writeDraft(t, dir, "01.md", "# feat(a): first\n\nbody\n")
	mgr := NewManager()
	mgr.armDrafts(context.Background(), cs, cs.ChatID, dir)
	mgr.armDrafts(context.Background(), cs, cs.ChatID, dir)

	publishCleanEnd(cs, "u1")

	// The old watcher is gone: one event, one render.
	sends := waitSends(t, em, 1)
	if !strings.Contains(sends[0].Text, "01.md") {
		t.Errorf("render body =\n%s", sends[0].Text)
	}
	settleNoSend(t, em, 1)
}
