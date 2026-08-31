package slack

import (
	"context"
	"errors"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
)

func TestLimiter_SpacesCallsApart(t *testing.T) {
	// Burst 1 with 5/s means the second acquire must wait ~200ms.
	// This is the protection layer that keeps N parallel chats from
	// multiplying past the app-wide quota.
	l := NewLimiter(&LimiterConfig{RatePerSec: 5, Burst: 1}, nil)
	ctx := context.Background()

	start := time.Now()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("first acquire should be immediate, took %v", d)
	}

	start = time.Now()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("second acquire returned after %v, expected ~200ms of throttling", d)
	}
}

func TestLimiter_RespectsContextCancellation(t *testing.T) {
	l := NewLimiter(&LimiterConfig{RatePerSec: 0.5, Burst: 1}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := l.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	if err := l.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked wait should honour ctx, got %v", err)
	}
}

func TestLimiter_NilIsANoOp(t *testing.T) {
	var l *Limiter
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("a nil limiter should not block or error, got %v", err)
	}
}

func TestLimiter_DefaultStaysUnderTier4(t *testing.T) {
	// chat.appendStream is Tier 4 (~1.67 req/s). The default has to
	// sit below that, not at it — Slack's burst allowance is not
	// published and a 429 costs more than a slower tick.
	if DefaultLimiterConfig.RatePerSec >= 1.67 {
		t.Fatalf("default rate %v is at or above the Tier 4 floor",
			DefaultLimiterConfig.RatePerSec)
	}
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limited", &slackgo.RateLimitedError{RetryAfter: time.Second}, true},
		{"server error", errors.New("slack server error: 503"), true},
		{"timeout", errors.New("net/http: request timeout"), true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"business error", errors.New("channel_not_found"), false},
		{"msg too long", errors.New("msg_too_long"), false},
		{"invalid thread", errors.New("invalid_thread_ts"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransient(tc.err); got != tc.want {
				t.Fatalf("isTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithTransientRetry_RetriesThenSucceeds(t *testing.T) {
	attempts := 0
	err := withTransientRetry(context.Background(),
		RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond},
		nil, "test", func() error {
			attempts++
			if attempts < 3 {
				return errors.New("slack server error: 503")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

// A business error is the caller's problem, not something to hammer.
func TestWithTransientRetry_TerminalErrorIsNotRetried(t *testing.T) {
	attempts := 0
	err := withTransientRetry(context.Background(),
		RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond},
		nil, "test", func() error {
			attempts++
			return errors.New("channel_not_found")
		})
	if err == nil {
		t.Fatal("terminal error should surface")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 — terminal errors must not be retried", attempts)
	}
}

// Slack's Retry-After beats any local backoff schedule.
func TestWithTransientRetry_HonoursRetryAfter(t *testing.T) {
	attempts := 0
	start := time.Now()
	err := withTransientRetry(context.Background(),
		RetryConfig{MaxAttempts: 2, InitialBackoff: time.Microsecond, MaxBackoff: time.Microsecond},
		nil, "test", func() error {
			attempts++
			if attempts == 1 {
				return &slackgo.RateLimitedError{RetryAfter: 120 * time.Millisecond}
			}
			return nil
		})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("waited %v, expected the server-supplied ~120ms", d)
	}
}

func TestWithTransientRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	attempts := 0
	err := withTransientRetry(context.Background(),
		RetryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond},
		nil, "test", func() error {
			attempts++
			return errors.New("slack server error: 500")
		})
	if err == nil {
		t.Fatal("expected the final error to surface")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestDedupIndex_FirstSightPassesRestDrop(t *testing.T) {
	d := newDedupIndex(8, time.Minute)

	if d.seen("C1", "1000.1") {
		t.Fatal("the first sighting must pass")
	}
	if !d.seen("C1", "1000.1") {
		t.Fatal("the second sighting must be suppressed")
	}
	if d.seen("C1", "1000.2") {
		t.Fatal("a different ts is a different message")
	}
	if d.seen("C2", "1000.1") {
		t.Fatal("the same ts in another channel is a different message")
	}
}

func TestDedupIndex_ExpiresAfterTTL(t *testing.T) {
	d := newDedupIndex(8, 50*time.Millisecond)
	base := time.Now()
	d.now = func() time.Time { return base }

	if d.seen("C1", "1000.1") {
		t.Fatal("first sighting should pass")
	}
	d.now = func() time.Time { return base.Add(time.Second) }
	if d.seen("C1", "1000.1") {
		t.Fatal("after the TTL the key should have been forgotten")
	}
}

func TestDedupIndex_EvictsBeyondCapacity(t *testing.T) {
	d := newDedupIndex(2, time.Minute)
	d.seen("C1", "1")
	d.seen("C1", "2")
	d.seen("C1", "3")

	if got := d.len(); got != 2 {
		t.Fatalf("index holds %d entries, want the cap of 2", got)
	}
	// The oldest was evicted, so it reads as unseen again.
	if d.seen("C1", "1") {
		t.Fatal("the evicted key should look unseen")
	}
}

func TestDedupIndex_IgnoresEmptyKeys(t *testing.T) {
	d := newDedupIndex(8, time.Minute)
	if d.seen("", "1000.1") || d.seen("C1", "") {
		t.Fatal("incomplete keys must not be treated as duplicates")
	}
}

func TestStateStore_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/slack_state.json"

	store, err := newStateStore(path)
	if err != nil {
		t.Fatalf("newStateStore: %v", err)
	}
	store.putStream(&OpenStream{
		ChatID: "sl_T1:C1", ChannelID: "C1", TS: "ts-1",
		StartedAt: time.Now().UTC(),
	})
	store.putChoice(&ChoiceState{RequestID: "req-1", ChannelID: "C1", TS: "ts-2"})

	// A fresh process must see both records — this is what makes
	// orphan-stream recovery possible at all.
	reopened, err := newStateStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n := len(reopened.orphanStreams(time.Now().UTC())); n != 1 {
		t.Fatalf("open streams after reopen = %d, want 1", n)
	}
	if _, ok := reopened.choice("req-1"); !ok {
		t.Fatal("choice state did not survive the reopen")
	}
}

func TestStateStore_MissingFileIsNotAnError(t *testing.T) {
	store, err := newStateStore(t.TempDir() + "/does-not-exist.json")
	if err != nil {
		t.Fatalf("a missing state file should start empty, got %v", err)
	}
	if n := len(store.orphanStreams(time.Now().UTC())); n != 0 {
		t.Fatalf("expected an empty store, got %d records", n)
	}
}

func TestStateStore_DropStreamRemovesRecord(t *testing.T) {
	store, _ := newStateStore("")
	store.putStream(&OpenStream{ChannelID: "C1", TS: "ts-1", StartedAt: time.Now().UTC()})
	store.dropStream("C1", "ts-1")

	if n := len(store.orphanStreams(time.Now().UTC())); n != 0 {
		t.Fatalf("record should be gone, got %d", n)
	}
}

func TestStreamIndex_EvictsAndPurges(t *testing.T) {
	idx := newStreamIndex(2)
	api := newFakeAPI()
	build := func() *turnStream {
		state, _ := newStateStore("")
		return newTurnStream("sl_T1:C1", "C1", "", "u", streamDeps{
			api: api, limiter: NewLimiter(nil, nil), state: state,
		})
	}

	idx.getOrCreate("sl_T1:C1", "u1", build)
	idx.getOrCreate("sl_T1:C1", "u2", build)
	if _, created := idx.getOrCreate("sl_T1:C1", "u1", build); created {
		t.Fatal("an existing turn should be returned, not rebuilt")
	}
	idx.getOrCreate("sl_T1:C1", "u3", build)

	if idx.len() != 2 {
		t.Fatalf("index holds %d, want the cap of 2", idx.len())
	}
	// u2 was least recently used once u1 was touched again.
	if _, ok := idx.lookup("sl_T1:C1", "u2"); ok {
		t.Fatal("the least recently used turn should have been evicted")
	}

	idx.purge("sl_T1:C1", "u1")
	if _, ok := idx.lookup("sl_T1:C1", "u1"); ok {
		t.Fatal("purge should remove the turn")
	}
}

func TestIsHTML(t *testing.T) {
	// Slack answers with a 200 + login page when files:read is
	// missing, so the status code alone cannot catch it.
	if !isHTML([]byte("<!DOCTYPE html><html>")) {
		t.Fatal("doctype should be detected")
	}
	if !isHTML([]byte("  <html lang=\"en\">")) {
		t.Fatal("leading whitespace should not defeat detection")
	}
	if isHTML([]byte("\x89PNG\r\n")) {
		t.Fatal("PNG bytes must not be mistaken for HTML")
	}
}
