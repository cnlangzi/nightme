package telegram

import (
	"context"
	"testing"
	"time"
)

func TestLimiter_ImmediateWhenBucketFull(t *testing.T) {
	l := NewLimiter(&LimiterConfig{RatePerSec: 10, Burst: 4}, nil)
	started := time.Now()
	for i := 0; i < 4; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("bucket should be full, elapsed=%v", elapsed)
	}
}

func TestLimiter_BlocksWhenBucketEmpty(t *testing.T) {
	// RatePerSec=10, Burst=1 → after the first Wait, we should
	// wait ~100ms before the next token is available. Upper bound
	// is generous to accommodate timer coalescing on macOS and
	// system load during CI.
	l := NewLimiter(&LimiterConfig{RatePerSec: 10, Burst: 1}, nil)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	started := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("second Wait: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed < 50*time.Millisecond {
		t.Fatalf("second Wait should block ~100ms, elapsed=%v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("second Wait blocked too long, elapsed=%v", elapsed)
	}
}

func TestLimiter_ContextCancelReturns(t *testing.T) {
	l := NewLimiter(&LimiterConfig{RatePerSec: 1, Burst: 1}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	_ = l.Wait(ctx)
	cancel()
	started := time.Now()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("expected ctx error")
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("cancel should return immediately, elapsed=%v", elapsed)
	}
}

func TestLimiter_NilSafe(t *testing.T) {
	var l *Limiter
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("nil limiter should be safe: %v", err)
	}
	if l.Tokens() != 0 {
		t.Fatal("nil limiter Tokens() should be 0")
	}
}

func TestLimiter_DefaultConfig(t *testing.T) {
	l := NewLimiter(nil, nil)
	if l.cfg.RatePerSec != DefaultLimiterConfig.RatePerSec {
		t.Fatalf("default rate: %v", l.cfg.RatePerSec)
	}
	if l.cfg.Burst != DefaultLimiterConfig.Burst {
		t.Fatalf("default burst: %v", l.cfg.Burst)
	}
}

func TestLimiter_ZeroFieldsFallBackToDefault(t *testing.T) {
	l := NewLimiter(&LimiterConfig{RatePerSec: 0, Burst: 0}, nil)
	if l.cfg.RatePerSec != DefaultLimiterConfig.RatePerSec {
		t.Fatalf("zero rate should fall back: %v", l.cfg.RatePerSec)
	}
	if l.cfg.Burst != DefaultLimiterConfig.Burst {
		t.Fatalf("zero burst should fall back: %v", l.cfg.Burst)
	}
}

func TestLimiter_TokensRefills(t *testing.T) {
	// 100 tokens/s, burst 10. After 50ms of refill, the bucket
	// should be at the burst cap (10) — we don't wait long enough
	// to see the gradient, but we can assert the cap.
	l := NewLimiter(&LimiterConfig{RatePerSec: 100, Burst: 10}, nil)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := l.Tokens(); got < 9 {
		t.Fatalf("expected ~10 tokens after 50ms @ 100/s, got %v", got)
	}
	// Drain via real Waits: 5 should be immediate, then the 6th
	// should be near-instant since the bucket still has ~5.
	for i := 0; i < 10; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
}
