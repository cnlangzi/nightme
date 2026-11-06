package gtw

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// --- manager-level registry ---

// TestManager_AbortRun_CancelsRegisteredRun pins the registry
// contract: beginRun registers the chain holder, abortRun cancels
// it with ErrAborted and reports the subcommand name.
func TestManager_AbortRun_CancelsRegisteredRun(t *testing.T) {
	m := newTestManager()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	release := m.beginRun("chat-A", "pr", cancel)
	defer release()

	sub, elapsed, ok := m.abortRun("chat-A")
	if !ok {
		t.Fatal("abortRun reported no run for chat-A")
	}
	if sub != "pr" {
		t.Errorf("sub = %q, want %q", sub, "pr")
	}
	if elapsed < 0 {
		t.Errorf("elapsed = %v, want >= 0", elapsed)
	}
	if !errors.Is(context.Cause(ctx), ErrAborted) {
		t.Errorf("cause = %v, want %v", context.Cause(ctx), ErrAborted)
	}
}

// TestManager_AbortRun_SecondCallFindsNothing covers the two
// aborts in a row: the record is removed at abort time, so the
// second abort has nothing to report.
func TestManager_AbortRun_SecondCallFindsNothing(t *testing.T) {
	m := newTestManager()

	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	release := m.beginRun("chat-A", "commit", cancel)
	defer release()

	if _, _, ok := m.abortRun("chat-A"); !ok {
		t.Fatal("first abortRun found no run")
	}
	if _, _, ok := m.abortRun("chat-A"); ok {
		t.Error("second abortRun found a run; the record must be removed at abort time")
	}
}

// TestManager_AbortRun_EmptyChatIDIsNoOp mirrors runLockFor: an
// empty chatID has no chain to abort.
func TestManager_AbortRun_EmptyChatIDIsNoOp(t *testing.T) {
	m := newTestManager()

	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	release := m.beginRun("", "pr", cancel)
	defer release()

	if _, _, ok := m.abortRun(""); ok {
		t.Error("abortRun(\"\") found a run")
	}
}

// TestManager_BeginRun_ReleaseKeepsNewerRun guards the release
// function's pointer compare: a late release from an aborted run
// must not clear the record of the run that took the chain after
// it.
func TestManager_BeginRun_ReleaseKeepsNewerRun(t *testing.T) {
	m := newTestManager()

	_, cancelOld := context.WithCancelCause(context.Background())
	defer cancelOld(nil)
	oldRelease := m.beginRun("chat-A", "pr", cancelOld)

	_, cancelNew := context.WithCancelCause(context.Background())
	defer cancelNew(nil)
	newRelease := m.beginRun("chat-A", "push", cancelNew)
	defer newRelease()

	oldRelease()

	sub, _, ok := m.abortRun("chat-A")
	if !ok {
		t.Fatal("abortRun found no run; the stale release cleared the newer record")
	}
	if sub != "push" {
		t.Errorf("sub = %q, want %q", sub, "push")
	}
}

// TestManager_AbortRun_OtherChatUnaffected covers the per-chat
// boundary of the registry.
func TestManager_AbortRun_OtherChatUnaffected(t *testing.T) {
	m := newTestManager()

	ctxA, cancelA := context.WithCancelCause(context.Background())
	defer cancelA(nil)
	releaseA := m.beginRun("chat-A", "pr", cancelA)
	defer releaseA()

	if _, _, ok := m.abortRun("chat-B"); ok {
		t.Error("abortRun(chat-B) found chat-A's run")
	}
	if aborted(ctxA) {
		t.Error("chat-A's run was aborted by an abort aimed at chat-B")
	}
}

// --- elapsed formatting ---

// TestFormatElapsed pins the /gtw abort card's running-time
// rendering.
func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{42 * time.Second, "42s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m00s"},
		{5*time.Minute + 12*time.Second, "5m12s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour, "1h00m"},
		{time.Hour + 3*time.Minute, "1h03m"},
		{2*time.Hour + 30*time.Minute, "2h30m"},
	}
	for _, c := range cases {
		if got := formatElapsed(c.d); got != c.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// --- command surface ---

// TestAbort_SpecAdvertises pins the /gtw abort entry in the
// command surface /help renders.
func TestAbort_SpecAdvertises(t *testing.T) {
	spec := NewFactory(NewManager()).Spec()
	if !strings.Contains(spec.Usage, "/gtw abort") {
		t.Errorf("Usage does not mention /gtw abort:\n%s", spec.Usage)
	}
	found := false
	for _, sub := range spec.Subcommands {
		if sub.Name == "abort" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Subcommands has no abort entry")
	}
}

// --- abort command ---

// TestAbort_NothingRunning covers /gtw abort in a chat with no
// /gtw command in flight: the reply is exactly "✨ nothing to
// abort" and nothing is cancelled.
func TestAbort_NothingRunning(t *testing.T) {
	rig := newAbortRig(t, "chat-abort-none")

	got, err := rig.handle(t, "chat-abort-none", "abort")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.Reply != "✨ nothing to abort" {
		t.Errorf("reply = %q, want %q", got.Reply, "✨ nothing to abort")
	}
	if !got.Consumed {
		t.Error("expected Consumed=true")
	}
}

// TestAbort_RejectsTail covers the argv gate: /gtw abort takes no
// positional arg and no flag.
func TestAbort_RejectsTail(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"positional", []string{"abort", "foo"}, "unexpected positional argument"},
		{"long flag", []string{"abort", "--force"}, "unknown flag"},
		{"short flag", []string{"abort", "-f"}, "unknown flag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig := newAbortRig(t, "chat-abort-tail")
			got, err := rig.handle(t, "chat-abort-tail", c.argv...)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !strings.Contains(got.Reply, c.want) {
				t.Errorf("reply = %q, want it to mention %q", got.Reply, c.want)
			}
		})
	}
}

// TestAbort_CancelsStuckCommand is the end-to-end case: a /gtw
// push is stuck in a git call, /gtw abort cancels it, the abort
// card names the command and its running time, and the cancelled
// command produces no card of its own.
func TestAbort_CancelsStuckCommand(t *testing.T) {
	rig := newAbortRig(t, "chat-abort-1")

	pushDone := make(chan *command.SlashOutput, 1)
	go func() {
		out, _ := rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs,
			command.SlashInput{
				Text:      "/gtw push",
				Args:      []string{"gtw", "push"},
				ChatID:    rig.cs.ChatID,
				MessageID: "msg-push",
			})
		pushDone <- out
	}()

	select {
	case <-rig.git.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the /gtw push never reached the git runner")
	}

	// The abort must not queue behind the run lock the stuck
	// command holds.
	abortOut := rig.abort(t, rig.cs.ChatID)

	lines := strings.Split(abortOut.Reply, "\n")
	if len(lines) != 2 {
		t.Fatalf("abort reply = %q, want two lines", abortOut.Reply)
	}
	if lines[0] != "✅ aborted `/gtw push`" {
		t.Errorf("first line = %q, want %q", lines[0], "✅ aborted `/gtw push`")
	}
	if !strings.HasPrefix(lines[1], "→ was running: ") {
		t.Errorf("second line = %q, want it to start with %q", lines[1], "→ was running: ")
	}

	// The stuck command unwinds: its context is cancelled, so the
	// git call returns.
	select {
	case out := <-pushDone:
		if out.Reply != "" {
			t.Errorf("the aborted command replied %q; it must stay silent", out.Reply)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the aborted command did not return")
	}
	if n := rig.ch.count(); n != 0 {
		t.Errorf("the aborted command sent %d message(s); the abort card is the only one", n)
	}
}

// TestAbort_CancelsStuckSyncCommand covers the other reply path:
// /gtw sync answers through the SlashOutput reply rather than the
// emitter, so the cancelled command's own card must be dropped
// there too.
func TestAbort_CancelsStuckSyncCommand(t *testing.T) {
	rig := newAbortRig(t, "chat-abort-sync")

	syncDone := make(chan *command.SlashOutput, 1)
	go func() {
		out, _ := rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs,
			command.SlashInput{
				Text:      "/gtw sync",
				Args:      []string{"gtw", "sync"},
				ChatID:    rig.cs.ChatID,
				MessageID: "msg-sync",
			})
		syncDone <- out
	}()

	select {
	case <-rig.git.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the /gtw sync never reached the git runner")
	}

	abortOut := rig.abort(t, rig.cs.ChatID)
	if !strings.HasPrefix(abortOut.Reply, "✅ aborted `/gtw sync`") {
		t.Errorf("abort reply = %q", abortOut.Reply)
	}

	select {
	case out := <-syncDone:
		if out.Reply != "" {
			t.Errorf("the aborted command replied %q; it must stay silent", out.Reply)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the aborted command did not return")
	}
}

// TestAbort_CancelledRunRunsNoHooks pins the "no further work"
// half of the abort: the cancelled command's after-hooks never
// start. The hook's context is already cancelled when the hook
// would spawn, and os/exec refuses to start a command whose
// context is done — so the hook process is never created.
func TestAbort_CancelledRunRunsNoHooks(t *testing.T) {
	rig := newAbortRig(t, "chat-abort-hooks")
	writeHomeYml(t, rig.home, `
sync:
  hooks:
    after:
      - echo hi > marker.txt
`)

	syncDone := make(chan *command.SlashOutput, 1)
	go func() {
		out, _ := rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs,
			command.SlashInput{
				Text:      "/gtw sync",
				Args:      []string{"gtw", "sync"},
				ChatID:    rig.cs.ChatID,
				MessageID: "msg-sync",
			})
		syncDone <- out
	}()

	select {
	case <-rig.git.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the /gtw sync never reached the git runner")
	}

	rig.abort(t, rig.cs.ChatID)

	select {
	case <-syncDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the aborted command did not return")
	}

	marker := filepath.Join(rig.cs.SelectedCwd(), "marker.txt")
	if _, err := os.Stat(marker); err == nil {
		t.Error("the after-hook ran after the abort; a cancelled command must do no further work")
	}
	if n := rig.ch.count(); n != 0 {
		t.Errorf("the aborted command sent %d message(s); the abort card is the only one", n)
	}
}

// TestAbort_QueuedCommandProceeds covers the point of the abort:
// the commands queued behind the cancelled one are not dropped.
func TestAbort_QueuedCommandProceeds(t *testing.T) {
	rig := newAbortRig(t, "chat-abort-queue")

	pushDone := make(chan struct{})
	go func() {
		defer close(pushDone)
		_, _ = rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs,
			command.SlashInput{
				Text:      "/gtw push",
				Args:      []string{"gtw", "push"},
				ChatID:    rig.cs.ChatID,
				MessageID: "msg-push",
			})
	}()

	select {
	case <-rig.git.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the /gtw push never reached the git runner")
	}

	// Queues behind the stuck command on the run lock.
	queuedDone := make(chan struct{})
	go func() {
		defer close(queuedDone)
		_, _ = rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs,
			command.SlashInput{
				Text:      "/gtw fix",
				Args:      []string{"gtw", "fix"},
				ChatID:    rig.cs.ChatID,
				MessageID: "msg-queued",
			})
	}()

	select {
	case <-queuedDone:
		t.Fatal("the queued /gtw fix ran while the stuck command still held the lock")
	case <-time.After(200 * time.Millisecond):
	}

	abortOut := rig.abort(t, rig.cs.ChatID)
	if !strings.HasPrefix(abortOut.Reply, "✅ aborted `/gtw push`") {
		t.Errorf("abort reply = %q", abortOut.Reply)
	}

	select {
	case <-queuedDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the queued /gtw fix did not proceed after the abort")
	}
	select {
	case <-pushDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the aborted /gtw push did not return")
	}
}

// TestAbort_OtherChatUnaffected covers the per-chat boundary: an
// abort issued in chat B leaves chat A's command running.
func TestAbort_OtherChatUnaffected(t *testing.T) {
	rig := newAbortRig(t, "chat-abort-a")
	csB := newAbortSession(t, "chat-abort-b", rig.ch)

	pushDone := make(chan struct{})
	go func() {
		defer close(pushDone)
		_, _ = rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs,
			command.SlashInput{
				Text:      "/gtw push",
				Args:      []string{"gtw", "push"},
				ChatID:    rig.cs.ChatID,
				MessageID: "msg-push",
			})
	}()

	select {
	case <-rig.git.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the /gtw push never reached the git runner")
	}

	out, err := rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, csB,
		command.SlashInput{
			Text:      "/gtw abort",
			Args:      []string{"gtw", "abort"},
			ChatID:    csB.ChatID,
			MessageID: "msg-abort",
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.Reply != "✨ nothing to abort" {
		t.Errorf("chat-B abort reply = %q, want %q", out.Reply, "✨ nothing to abort")
	}

	select {
	case <-pushDone:
		t.Fatal("chat-B's abort cancelled chat-A's command")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAbort_WorksWithoutCwd covers a chat that never picked a
// workspace: the abort still answers instead of refusing.
func TestAbort_WorksWithoutCwd(t *testing.T) {
	rig := newAbortRig(t, "chat-abort-nocwd")
	rig.cs.ClearSelectedCwd()

	got, err := rig.f.Handle(context.Background(), command.RuntimeServices{}, nil, rig.cs,
		command.SlashInput{
			Text:      "/gtw abort",
			Args:      []string{"gtw", "abort"},
			ChatID:    rig.cs.ChatID,
			MessageID: "msg-abort",
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.Reply != "✨ nothing to abort" {
		t.Errorf("reply = %q, want %q", got.Reply, "✨ nothing to abort")
	}
}

// --- rig ---

// blockingGit is a GitRunner that never returns on its own: it
// blocks until the caller's context is cancelled, then reports the
// cancellation. It stands in for the subprocess that hangs — the
// reason /gtw abort exists.
type blockingGit struct {
	entered chan struct{}
	once    sync.Once
}

func (g *blockingGit) Run(ctx context.Context, _ string, _ ...string) (string, string, error) {
	g.once.Do(func() { close(g.entered) })
	<-ctx.Done()
	return "", "", ctx.Err()
}

// abortRig is the minimal /gtw rig the abort tests need: a chat
// session with a workspace, a recording emitter, and a factory
// whose git runner blocks until cancelled.
type abortRig struct {
	f    *Factory
	cs   *chatsession.ChatSession
	ch   *recordingCh
	git  *blockingGit
	home string
}

func newAbortRig(t *testing.T, chatID string) *abortRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	ch := &recordingCh{}
	git := &blockingGit{entered: make(chan struct{})}
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{
		Git: git,
		Now: time.Now,
	})
	return &abortRig{
		f:    f,
		cs:   newAbortSession(t, chatID, ch),
		ch:   ch,
		git:  git,
		home: home,
	}
}

// newAbortSession builds a chat session bound to the recording
// emitter and a workspace.
func newAbortSession(t *testing.T, chatID string, ch *recordingCh) *chatsession.ChatSession {
	t.Helper()
	cs, err := chatsession.New(chatID, "test-agent")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	cs.WithEmitter(ch)
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	return cs
}

// handle drives /gtw <args...> in the rig's chat.
func (r *abortRig) handle(t *testing.T, chatID string, args ...string) (*command.SlashOutput, error) {
	t.Helper()
	return r.f.Handle(context.Background(), command.RuntimeServices{}, nil, r.cs,
		command.SlashInput{
			Text:      "/gtw " + strings.Join(args, " "),
			Args:      append([]string{"gtw"}, args...),
			ChatID:    chatID,
			MessageID: "msg-1",
		})
}

// abort drives /gtw abort in the rig's chat. It runs on its own
// goroutine so a regression that makes the abort wait on the run
// lock fails the test instead of hanging it.
func (r *abortRig) abort(t *testing.T, chatID string) *command.SlashOutput {
	t.Helper()
	done := make(chan *command.SlashOutput, 1)
	go func() {
		out, _ := r.f.Handle(context.Background(), command.RuntimeServices{}, nil, r.cs,
			command.SlashInput{
				Text:      "/gtw abort",
				Args:      []string{"gtw", "abort"},
				ChatID:    chatID,
				MessageID: "msg-abort",
			})
		done <- out
	}()
	select {
	case out := <-done:
		return out
	case <-time.After(10 * time.Second):
		t.Fatal("/gtw abort blocked on the run lock")
		return nil
	}
}
