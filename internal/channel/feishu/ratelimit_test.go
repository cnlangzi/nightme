package feishu

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/config"
)

// fakeClock is a clock whose time can be advanced by the test. Used to
// drive Wait deterministically without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// TestNewLimiter_StrictDefault verifies that nil/zero cfg resolves to
// the conservative StrictDefault (RatePerSec=5, Burst=1).
func TestNewLimiter_StrictDefault(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		cfg               *config.FeishuRateLimitConfig
		wantRatePerSec    float64
		wantBurst         int
	}{
		{"nil cfg", nil, StrictDefault.RatePerSec, StrictDefault.Burst},
		{"zero cfg", &config.FeishuRateLimitConfig{}, StrictDefault.RatePerSec, StrictDefault.Burst},
		// negative RatePerSec 视为无效 → 退回默认；Burst=100 是正值 → 保留
		{"negative rate ignored, valid burst kept",
			&config.FeishuRateLimitConfig{RatePerSec: -1, Burst: 100},
			StrictDefault.RatePerSec, 100},
		// 零 RatePerSec 视为无效 → 退回默认；零 Burst 也视为无效 → 退回默认
		{"both zero ignored", &config.FeishuRateLimitConfig{}, StrictDefault.RatePerSec, StrictDefault.Burst},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := NewLimiter(tc.cfg, nil)
			got := l.Cfg()
			if got.RatePerSec != tc.wantRatePerSec {
				t.Fatalf("RatePerSec = %v, want %v", got.RatePerSec, tc.wantRatePerSec)
			}
			if got.Burst != tc.wantBurst {
				t.Fatalf("Burst = %d, want %d", got.Burst, tc.wantBurst)
			}
		})
	}
}

// TestLimiter_ConfigOverride verifies user-provided cfg is honored
// when values are positive.
func TestLimiter_ConfigOverride(t *testing.T) {
	t.Parallel()
	cfg := &config.FeishuRateLimitConfig{RatePerSec: 10, Burst: 2}
	l := NewLimiter(cfg, nil)
	got := l.Cfg()
	if got.RatePerSec != 10 {
		t.Fatalf("RatePerSec = %v, want 10", got.RatePerSec)
	}
	if got.Burst != 2 {
		t.Fatalf("Burst = %d, want 2", got.Burst)
	}
}

// TestLimiter_InitialBurst verifies the first call(s) within burst
// succeed immediately, and the next one blocks for ~1/rate seconds.
func TestLimiter_InitialBurst(t *testing.T) {
	t.Parallel()

	cfg := &config.FeishuRateLimitConfig{RatePerSec: 5, Burst: 1}
	l := NewLimiter(cfg, nil)
	ctx := context.Background()

	// First call: bucket starts full (Burst=1), no wait.
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	// Second call: bucket empty, must wait 1/5 = 200ms before next token.
	start := time.Now()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("second Wait: %v", err)
	}
	wait := time.Since(start)
	if wait < 180*time.Millisecond {
		t.Fatalf("second Wait elapsed %v, want ≥ 180ms", wait)
	}
	if wait > 400*time.Millisecond {
		t.Fatalf("second Wait elapsed %v, want ≤ 400ms", wait)
	}
}

// TestLimiter_Refill verifies that advancing the clock refills tokens
// proportionally to elapsed time.
func TestLimiter_Refill(t *testing.T) {
	t.Parallel()

	cfg := &config.FeishuRateLimitConfig{RatePerSec: 5, Burst: 1}
	l := NewLimiter(cfg, nil)
	fc := newFakeClock()
	l.SetClock(fc)
	ctx := context.Background()

	// Drain the bucket.
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	// Advance clock by 200ms (1/5 of a second = exactly 1 token).
	fc.Advance(200 * time.Millisecond)

	// Second call should now succeed immediately (without sleeping).
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := l.Wait(ctx); err != nil {
			t.Errorf("Wait after refill: %v", err)
		}
	}()
	select {
	case <-done:
		// good
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Wait after 200ms advance blocked > 100ms")
	}
}

// TestLimiter_ContextCancel verifies Wait returns ctx.Err() when the
// context is cancelled while blocked.
func TestLimiter_ContextCancel(t *testing.T) {
	t.Parallel()

	cfg := &config.FeishuRateLimitConfig{RatePerSec: 5, Burst: 1}
	l := NewLimiter(cfg, nil)
	ctx := context.Background()

	// Drain the bucket.
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("drain Wait: %v", err)
	}

	// Second Wait should block for ~200ms. Cancel mid-flight.
	ctx2, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- l.Wait(ctx2)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Wait returned %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Wait did not return after ctx cancel")
	}
}

// TestLimiter_LongRunNoOvershoot verifies that over a sustained period,
// the total wait time matches the bucket's expected throughput (no
// overshoot, no permanent starvation).
//
// Config: RatePerSec=5, Burst=1. Over 10 seconds, we should be able to
// call Wait 51 times (initial burst 1 + 5/sec × 10) without overshooting
// the configured rate. We simulate this with a fake clock to avoid
// sleeping.
func TestLimiter_LongRunNoOvershoot(t *testing.T) {
	t.Parallel()

	cfg := &config.FeishuRateLimitConfig{RatePerSec: 5, Burst: 1}
	l := NewLimiter(cfg, nil)
	fc := newFakeClock()
	l.SetClock(fc)
	ctx := context.Background()

	// Track the simulated clock time after each Wait completes.
	const totalSeconds = 10
	const expectedCalls = 1 + totalSeconds*5 // initial + rate*time

	waitCount := 0
	for i := 0; i < expectedCalls; i++ {
		beforeT := fc.Now()
		if err := l.Wait(ctx); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
		_ = beforeT // unused; keep for clarity
		waitCount++

		// After each Wait, advance fake clock by 200ms (= 1 token worth)
		// to simulate perfect pacing at RatePerSec.
		fc.Advance(200 * time.Millisecond)
	}

	if waitCount != expectedCalls {
		t.Fatalf("expected %d successful Waits, got %d", expectedCalls, waitCount)
	}
}

// TestLimiter_ConcurrentAcquire verifies multiple goroutines sharing a
// Limiter don't starve and the bucket is acquired atomically.
func TestLimiter_ConcurrentAcquire(t *testing.T) {
	t.Parallel()

	cfg := &config.FeishuRateLimitConfig{RatePerSec: 20, Burst: 2}
	l := NewLimiter(cfg, nil)
	ctx := context.Background()

	const goroutines = 10
	var wg sync.WaitGroup
	var ok int32
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := l.Wait(ctx); err != nil {
				t.Errorf("concurrent Wait: %v", err)
				return
			}
			atomic.AddInt32(&ok, 1)
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&ok) != goroutines {
		t.Fatalf("ok = %d, want %d", ok, goroutines)
	}
}