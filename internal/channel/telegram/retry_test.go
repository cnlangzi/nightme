package telegram

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestIsTransient_Nil(t *testing.T) {
	if IsTransient(nil) {
		t.Fatal("nil must not be transient")
	}
}

func TestIsTransient_ContextErrors(t *testing.T) {
	if IsTransient(context.Canceled) {
		t.Fatal("context.Canceled must not be transient")
	}
	if IsTransient(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must not be transient")
	}
}

func TestIsTransient_NetworkTimeouts(t *testing.T) {
	if !IsTransient(&net.OpError{Op: "read", Err: errors.New("i/o timeout")}) {
		t.Fatal("i/o timeout should be transient")
	}
	if !IsTransient(io.EOF) {
		t.Fatal("io.EOF should be transient")
	}
	if !IsTransient(syscall.ECONNRESET) {
		t.Fatal("ECONNRESET should be transient")
	}
	if !IsTransient(syscall.EPIPE) {
		t.Fatal("EPIPE should be transient")
	}
	if !IsTransient(errors.New("connection reset by peer")) {
		t.Fatal("'connection reset' substring should be transient")
	}
	if !IsTransient(errors.New("TLS handshake timeout")) {
		t.Fatal("'TLS handshake timeout' should be transient")
	}
	if !IsTransient(errors.New("no such host")) {
		t.Fatal("'no such host' should be transient")
	}
}

func TestIsTransient_Terminal(t *testing.T) {
	// 4xx business error: chat not found, message not modified, etc.
	if IsTransient(&apiError{StatusCode: 400, Message: "bad request"}) {
		t.Fatal("400 must not be transient")
	}
	if IsTransient(&apiError{StatusCode: 403, Message: "forbidden"}) {
		t.Fatal("403 must not be transient")
	}
	if IsTransient(&apiError{StatusCode: 404, Message: "chat not found"}) {
		t.Fatal("404 must not be transient")
	}
	// Random business text
	if IsTransient(errors.New("chat not found")) {
		t.Fatal("plain 'chat not found' must not be transient")
	}
}

func TestIsTransient_5xxRetry(t *testing.T) {
	if !IsTransient(&apiError{StatusCode: 500, Message: "internal server error"}) {
		t.Fatal("500 must be transient")
	}
	if !IsTransient(&apiError{StatusCode: 502, Message: "bad gateway"}) {
		t.Fatal("502 must be transient")
	}
	if !IsTransient(&apiError{StatusCode: 503, Message: "service unavailable"}) {
		t.Fatal("503 must be transient")
	}
}

func TestIsTransient_429Retry(t *testing.T) {
	if !IsTransient(&apiError{StatusCode: 429, RetryAfter: 30, Message: "too many requests"}) {
		t.Fatal("429 must be transient")
	}
}

func TestWithTransientRetry_Success(t *testing.T) {
	calls := 0
	err := WithTransientRetry(context.Background(), RetryOpts{Op: "test"}, func() error {
		calls++
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestWithTransientRetry_TerminalNoRetry(t *testing.T) {
	calls := 0
	terminal := &apiError{StatusCode: 404, Message: "chat not found"}
	err := WithTransientRetry(context.Background(), RetryOpts{Op: "test"}, func() error {
		calls++
		return terminal
	})
	if !errors.Is(err, terminal) {
		t.Fatalf("expected terminal err, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("terminal must not retry: calls=%d", calls)
	}
}

func TestWithTransientRetry_ExhaustsThenReturns(t *testing.T) {
	calls := 0
	transient := &apiError{StatusCode: 503, Message: "try again"}
	err := WithTransientRetry(context.Background(),
		RetryOpts{Op: "test", Cfg: RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}},
		func() error {
			calls++
			return transient
		})
	if !errors.Is(err, transient) {
		t.Fatalf("expected transient err, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestWithTransientRetry_RecoversOnRetry(t *testing.T) {
	calls := 0
	transient := &apiError{StatusCode: 503, Message: "try again"}
	err := WithTransientRetry(context.Background(),
		RetryOpts{Op: "test", Cfg: RetryConfig{MaxAttempts: 5, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}},
		func() error {
			calls++
			if calls < 3 {
				return transient
			}
			return nil
		})
	if err != nil {
		t.Fatalf("expected nil after recovery, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts (2 fails + 1 success), got %d", calls)
	}
}

func TestWithTransientRetry_429RetryAfter(t *testing.T) {
	calls := 0
	err429 := &apiError{StatusCode: 429, RetryAfter: 1, Message: "rate limited"}
	started := time.Now()
	_ = WithTransientRetry(context.Background(),
		RetryOpts{Op: "test", Cfg: RetryConfig{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}},
		func() error {
			calls++
			return err429
		})
	elapsed := time.Since(started)
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
	// RetryAfter is 1 second; backoff would be ~1ms. The retry wait
	// should be at least 1s.
	if elapsed < 900*time.Millisecond {
		t.Fatalf("429 retry did not honour retry_after: elapsed=%v", elapsed)
	}
}

func TestWithTransientRetry_ContextCancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	calls := 0
	transient := &apiError{StatusCode: 503}
	err := WithTransientRetry(ctx,
		RetryOpts{Op: "test", Cfg: RetryConfig{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}},
		func() error {
			calls++
			return transient
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ctx.Canceled, got %v", err)
	}
}

func TestWithTransientRetryResult_Generic(t *testing.T) {
	result, err := WithTransientRetryResult(context.Background(),
		RetryOpts{Op: "test"},
		func() (int, error) { return 42, nil })
	if err != nil || result != 42 {
		t.Fatalf("result=%d err=%v", result, err)
	}
}

func TestRetryAfter_ZeroOnNonAPIError(t *testing.T) {
	if retryAfter(errors.New("plain error")) != 0 {
		t.Fatal("plain error should have 0 retryAfter")
	}
	if retryAfter(&apiError{StatusCode: 429, RetryAfter: 0}) != 0 {
		t.Fatal("apiError with RetryAfter=0 should be 0")
	}
	if retryAfter(&apiError{StatusCode: 429, RetryAfter: 5}) != 5*time.Second {
		t.Fatal("apiError with RetryAfter=5 should be 5s")
	}
}

func TestJitter_Bounds(t *testing.T) {
	base := time.Second
	for i := 0; i < 100; i++ {
		got := jitter(base, 0.25)
		if got < 750*time.Millisecond || got > 1250*time.Millisecond {
			t.Fatalf("jitter out of bounds: %v", got)
		}
	}
}

func TestJitter_ZeroPct(t *testing.T) {
	base := time.Second
	if got := jitter(base, 0); got != base {
		t.Fatalf("zero-pct jitter should be exact: %v", got)
	}
}

