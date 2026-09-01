package slack

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
)

func testStream(t *testing.T, api *fakeAPI, throttle time.Duration) *turnStream {
	t.Helper()
	state, err := newStateStore("")
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	return newTurnStream("sl_T1:C1", "C1", "1000.1", "1000.1", "T_TEST", "U_TEST", streamDeps{
		api:      api,
		limiter:  NewLimiter(&LimiterConfig{RatePerSec: 1000, Burst: 1000}, nil),
		retry:    RetryConfig{MaxAttempts: 1},
		logger:   nil,
		state:    state,
		throttle: throttle,
	})
}

// The first flush opens the stream; every later one appends. This is
// the whole reason the Slack adapter has no overflow machinery: the
// body is never re-sent.
func TestStream_FirstFlushStartsThenAppends(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)
	ctx := context.Background()

	if err := s.appendMarkdown(ctx, "one", false); err != nil {
		t.Fatalf("append one: %v", err)
	}
	if err := s.appendMarkdown(ctx, "two", false); err != nil {
		t.Fatalf("append two: %v", err)
	}

	got := api.methods()
	want := []string{"StartStream", "AppendStream"}
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, got[i], want[i])
		}
	}

	calls := api.snapshot()
	if texts := chunkTexts(calls[0].Chunks); len(texts) != 1 || texts[0] != "one" {
		t.Fatalf("start chunks = %v", texts)
	}
	if texts := chunkTexts(calls[1].Chunks); len(texts) != 1 || texts[0] != "two" {
		t.Fatalf("append chunks = %v", texts)
	}
	if calls[1].TS != calls[0].TS {
		t.Fatalf("append targeted %q but the stream is %q", calls[1].TS, calls[0].TS)
	}
}

// Events arriving inside the throttle window coalesce into one API
// call — the mechanism that keeps a chatty turn inside Slack's quota.
func TestStream_ThrottleCoalescesWithinWindow(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, time.Hour) // window never reopens on its own
	ctx := context.Background()

	for _, text := range []string{"a", "b", "c"} {
		if err := s.appendMarkdown(ctx, text, false); err != nil {
			t.Fatalf("append %s: %v", text, err)
		}
	}

	// Only the first append flushed; b and c are still buffered.
	if n := api.countOf("StartStream"); n != 1 {
		t.Fatalf("StartStream count = %d, want 1", n)
	}
	if n := api.countOf("AppendStream"); n != 0 {
		t.Fatalf("AppendStream count = %d, want 0 (throttled)", n)
	}

	// finish drains what the window held back.
	if err := s.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}
	calls := api.snapshot()
	var appended []string
	for _, c := range calls {
		if c.Method == "AppendStream" {
			appended = append(appended, chunkTexts(c.Chunks)...)
		}
	}
	if len(appended) != 2 || appended[0] != "b" || appended[1] != "c" {
		t.Fatalf("buffered content = %v, want [b c] in order", appended)
	}
}

// A blocking prompt must not wait for the window.
func TestStream_UrgentBypassesThrottle(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, time.Hour)
	ctx := context.Background()

	if err := s.appendMarkdown(ctx, "first", false); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.appendMarkdown(ctx, "urgent", true); err != nil {
		t.Fatalf("urgent append: %v", err)
	}

	if n := api.countOf("AppendStream"); n != 1 {
		t.Fatalf("urgent append should have flushed immediately, got %d appends", n)
	}
}

// The throttle timer must eventually drain the buffer even if no
// further events arrive.
func TestStream_TimerDrainsBufferAfterWindow(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 40*time.Millisecond)
	ctx := context.Background()

	if err := s.appendMarkdown(ctx, "first", false); err != nil {
		t.Fatalf("append first: %v", err)
	}
	if err := s.appendMarkdown(ctx, "second", false); err != nil {
		t.Fatalf("append second: %v", err)
	}
	if n := api.countOf("AppendStream"); n != 0 {
		t.Fatalf("second append should be buffered, got %d", n)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if api.countOf("AppendStream") > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := api.countOf("AppendStream"); n != 1 {
		t.Fatalf("timer did not drain the buffer (appends=%d)", n)
	}
}

// finish is the only thing that closes a Slack stream. Unlike a
// Feishu card, an unclosed stream keeps rendering as in-progress.
func TestStream_FinishStopsAndCarriesFooter(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)
	ctx := context.Background()

	if err := s.appendMarkdown(ctx, "body", false); err != nil {
		t.Fatalf("append: %v", err)
	}
	s.stampFooter([]string{"🤖: claude", "📁: repo"})
	if err := s.finish(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}

	calls := api.snapshot()
	last := calls[len(calls)-1]
	if last.Method != "StopStream" {
		t.Fatalf("last call = %q, want StopStream", last.Method)
	}
	if len(last.Blocks) != 2 {
		t.Fatalf("footer should ride along as finalization blocks, got %d", len(last.Blocks))
	}
}

func TestStream_FinishIsIdempotent(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)
	ctx := context.Background()

	_ = s.appendMarkdown(ctx, "body", false)
	if err := s.finish(ctx); err != nil {
		t.Fatalf("first finish: %v", err)
	}
	if err := s.finish(ctx); err != nil {
		t.Fatalf("second finish: %v", err)
	}
	if n := api.countOf("StopStream"); n != 1 {
		t.Fatalf("StopStream called %d times, want 1", n)
	}
}

// A turn that produced nothing never opened a stream, so there is
// nothing to stop.
func TestStream_FinishWithoutContentSendsNothing(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)

	if err := s.finish(context.Background()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(api.methods()) != 0 {
		t.Fatalf("empty turn should not call Slack, got %v", api.methods())
	}
}

func TestStream_AppendAfterFinishIsRejected(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)
	ctx := context.Background()

	_ = s.appendMarkdown(ctx, "body", false)
	_ = s.finish(ctx)

	err := s.appendMarkdown(ctx, "late", false)
	if err != errStreamClosed {
		t.Fatalf("append after finish = %v, want errStreamClosed", err)
	}
}

// A failed flush must not silently drop the content, and must not
// reorder it relative to what comes next.
func TestStream_FailedAppendRequeuesInOrder(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)
	ctx := context.Background()

	if err := s.appendMarkdown(ctx, "first", false); err != nil {
		t.Fatalf("open stream: %v", err)
	}
	api.failOnce("AppendStream", errBoom)
	if err := s.appendMarkdown(ctx, "lost", false); err == nil {
		t.Fatal("expected the scripted failure to surface")
	}

	// The next append should carry the failed chunk first.
	if err := s.appendMarkdown(ctx, "next", false); err != nil {
		t.Fatalf("retry append: %v", err)
	}
	calls := api.snapshot()
	last := calls[len(calls)-1]
	texts := chunkTexts(last.Chunks)
	if len(texts) != 2 || texts[0] != "lost" || texts[1] != "next" {
		t.Fatalf("requeued content = %v, want [lost next]", texts)
	}
}

// Tool start and end share one card id so Slack merges them.
func TestStream_ToolPairingIsFIFO(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)

	firstID := s.beginTool("Bash(a)")
	secondID := s.beginTool("Read(b)")
	if firstID == secondID {
		t.Fatal("concurrent tools must get distinct card ids")
	}

	gotID, gotTitle, ok := s.endTool()
	if !ok || gotID != firstID || gotTitle != "Bash(a)" {
		t.Fatalf("first end popped (%q,%q,%v), want the oldest start", gotID, gotTitle, ok)
	}
	gotID, _, ok = s.endTool()
	if !ok || gotID != secondID {
		t.Fatalf("second end popped %q, want %q", gotID, secondID)
	}
	if _, _, ok := s.endTool(); ok {
		t.Fatal("a third end has no start to pair with")
	}
}

func TestStream_TaskChunkFieldsAreClamped(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)

	long := strings.Repeat("x", 400)
	if err := s.appendTask(context.Background(), "tool-1", long,
		slackgo.TaskCardStatusInProgress, long, false); err != nil {
		t.Fatalf("appendTask: %v", err)
	}

	calls := api.snapshot()
	tasks := taskChunks(calls[0].Chunks)
	if len(tasks) != 1 {
		t.Fatalf("expected one task chunk, got %d", len(tasks))
	}
	// Slack rejects task_update fields over 256 chars.
	if n := len([]rune(tasks[0].Title)); n > taskFieldMaxRunes {
		t.Fatalf("title is %d runes, over the %d limit", n, taskFieldMaxRunes)
	}
	if n := len([]rune(tasks[0].Details)); n > taskFieldMaxRunes {
		t.Fatalf("details is %d runes, over the %d limit", n, taskFieldMaxRunes)
	}
}

// The open-stream record has to exist before anything else can fail,
// otherwise a crash leaves a stream nobody knows how to close.
func TestStream_RecordsOpenStreamForRecovery(t *testing.T) {
	api := newFakeAPI()
	state, err := newStateStore("")
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	s := newTurnStream("sl_T1:C1", "C1", "1000.1", "1000.1", "T_TEST", "U_TEST", streamDeps{
		api:      api,
		limiter:  NewLimiter(&LimiterConfig{RatePerSec: 1000, Burst: 1000}, nil),
		retry:    RetryConfig{MaxAttempts: 1},
		state:    state,
		throttle: 0,
	})

	if err := s.appendMarkdown(context.Background(), "body", false); err != nil {
		t.Fatalf("append: %v", err)
	}
	orphans := state.orphanStreams(time.Now().UTC())
	if len(orphans) != 1 {
		t.Fatalf("expected the open stream to be recorded, got %d", len(orphans))
	}
	if orphans[0].TS != s.messageTS() {
		t.Fatalf("recorded ts %q != stream ts %q", orphans[0].TS, s.messageTS())
	}

	// After a clean close the record must be gone, or the next
	// start would try to stop an already-stopped stream.
	if err := s.finish(context.Background()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if n := len(state.orphanStreams(time.Now().UTC())); n != 0 {
		t.Fatalf("record should be dropped after a clean stop, got %d", n)
	}
}

func TestStream_LongTextSplitsIntoMultipleChunks(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)

	long := strings.Repeat("a", maxMarkdownChunkRunes+500)
	if err := s.appendMarkdown(context.Background(), long, false); err != nil {
		t.Fatalf("append: %v", err)
	}
	calls := api.snapshot()
	texts := chunkTexts(calls[0].Chunks)
	if len(texts) < 2 {
		t.Fatalf("oversized text should split, got %d chunks", len(texts))
	}
	for i, txt := range texts {
		if n := len([]rune(txt)); n > maxMarkdownChunkRunes {
			t.Fatalf("chunk %d has %d runes, over the ceiling", i, n)
		}
	}
}

// Two concurrent first-flushes must NOT mint two parallel streams on
// Slack. startMu serializes startStream so whichever goroutine wins
// the race opens the stream; the other falls through to appendStream.
func TestStream_ConcurrentFirstFlushOpensOneStream(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, time.Hour) // never coalesce via timer
	ctx := context.Background()

	// Stage two flushes by hand so they race for the start path.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = s.appendMarkdown(ctx, "first", true)
	}()
	go func() {
		defer wg.Done()
		_ = s.appendMarkdown(ctx, "second", true)
	}()
	wg.Wait()

	if n := api.countOf("StartStream"); n != 1 {
		t.Fatalf("StartStream count = %d, want 1 (startMu should serialize)", n)
	}
	if n := api.countOf("AppendStream"); n < 1 {
		t.Fatalf("AppendStream count = %d, want at least 1 (racing goroutine should fall through)", n)
	}
}

// finish() must surface a startStream failure with enough context
// that the operator can see what was lost — silent drops used to
// make this look like "nothing happened".
func TestStream_FinishLogsLostContentWhenStartFails(t *testing.T) {
	api := newFakeAPI()
	api.failAlways("StartStream", errBoom)

	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, nil))

	state, _ := newStateStore("")
	s := newTurnStream("sl_T1:C1", "C1", "1000.1", "1000.1", "T_TEST", "U_TEST", streamDeps{
		api:      api,
		limiter:  NewLimiter(&LimiterConfig{RatePerSec: 1000, Burst: 1000}, nil),
		retry:    RetryConfig{MaxAttempts: 1},
		logger:   logger,
		state:    state,
		throttle: 0,
	})

	// The append itself surfaces the start failure (the batch is
	// requeued so finish can still try). The point of the test is
	// what finish does with the failed start.
	if err := s.appendMarkdown(context.Background(), "this would have been the reply", false); err == nil {
		t.Fatal("append should surface the startStream failure while StartStream is broken")
	}
	if err := s.finish(context.Background()); err == nil {
		t.Fatal("finish should propagate startStream failure")
	}
	if !strings.Contains(logbuf.String(), "agent reply lost") {
		t.Fatalf("finish should log the lost content; log was: %q", logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "this would have been the reply") {
		t.Fatalf("log should include the lost text preview, got: %q", logbuf.String())
	}
}

// chat.startStream requires recipient_team_id + recipient_user_id
// when streaming to channels, otherwise Slack returns
// missing_recipient_team_id (docs/channel/slack.md §5.2). The
// turnStream must forward the adapter-stashed team/user ids.
func TestStream_StartStreamCarriesRecipientInfo(t *testing.T) {
	api := newFakeAPI()
	s := testStream(t, api, 0)
	ctx := context.Background()

	if err := s.appendMarkdown(ctx, "hello", false); err != nil {
		t.Fatalf("append: %v", err)
	}
	calls := api.snapshot()
	if len(calls) == 0 || calls[0].Method != "StartStream" {
		t.Fatalf("expected StartStream call first; got %v", calls)
	}
	if calls[0].TeamID != "T_TEST" {
		t.Errorf("StartStream teamID = %q, want T_TEST (must propagate so Slack accepts the call)", calls[0].TeamID)
	}
	if calls[0].UserID != "U_TEST" {
		t.Errorf("StartStream userID = %q, want U_TEST (must propagate so Slack accepts the call)", calls[0].UserID)
	}
}
