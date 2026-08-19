// Package telegram — F-TG-RL global token-bucket limiter.
//
// Telegram Bot API rate limits (per https://core.telegram.org/bots/api#rate-limits):
//
//   - ~30 messages per second to different chats (global).
//   - ~20 messages per minute to the same group.
//
// We default to a conservative per-process bucket that comfortably
// fits both bounds. RatePerSec=8 (well under 30/s global) and
// Burst=4 (small allowance for parallel sub-routines, e.g. sending
// thinking + tool_start in the same turn). Per-chat constraints are
// enforced by the runtime's natural serialization (Send is called
// serially per chat turn), not by this limiter.
//
// The limiter is process-wide (one bucket per Adapter). It is
// advisory: callers should Wait() before every API call. api.call()
// owns this discipline.
package telegram

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"
)

// DefaultLimiterConfig is the conservative default. Slightly more
// generous than feishu.StrictDefault because Telegram's per-second
// cap is higher (30 vs 5).
var DefaultLimiterConfig = LimiterConfig{
	RatePerSec: 8,
	Burst:      4,
}

// LimiterConfig configures the rate limiter.
type LimiterConfig struct {
	RatePerSec float64
	Burst      int
}

// Limiter is a lazy-refill token bucket. Single-process, single
// bucket; covers all outbound Telegram API calls.
type Limiter struct {
	cfg    LimiterConfig
	logger *slog.Logger

	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// NewLimiter constructs a Limiter. cfg==nil or zero fields fall
// back to DefaultLimiterConfig.
func NewLimiter(cfg *LimiterConfig, logger *slog.Logger) *Limiter {
	c := DefaultLimiterConfig
	if cfg != nil {
		if cfg.RatePerSec > 0 {
			c.RatePerSec = cfg.RatePerSec
		}
		if cfg.Burst > 0 {
			c.Burst = cfg.Burst
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Limiter{
		cfg:        c,
		logger:     logger,
		tokens:     float64(c.Burst),
		lastRefill: time.Now(),
	}
}

// Wait blocks until a token is available or ctx is cancelled.
// Returns nil on success, ctx.Err() on cancel.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	started := time.Now()
	for {
		l.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(l.lastRefill).Seconds()
		l.tokens = math.Min(float64(l.cfg.Burst), l.tokens+elapsed*l.cfg.RatePerSec)
		l.lastRefill = now

		if l.tokens >= 1.0 {
			l.tokens -= 1.0
			snapshot := l.tokens
			l.mu.Unlock()
			if d := time.Since(started); d > 100*time.Millisecond {
				l.logger.Debug("telegram rate limit blocked",
					"wait_ms", d.Milliseconds(),
					"tokens", snapshot,
					"rate_per_sec", l.cfg.RatePerSec,
					"burst", l.cfg.Burst,
				)
			}
			return nil
		}

		deficit := 1.0 - l.tokens
		waitSec := deficit / l.cfg.RatePerSec
		l.mu.Unlock()

		timer := time.NewTimer(time.Duration(waitSec * float64(time.Second)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// Tokens returns the current bucket level (test-only hook).
func (l *Limiter) Tokens() float64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(l.lastRefill).Seconds()
	l.tokens = math.Min(float64(l.cfg.Burst), l.tokens+elapsed*l.cfg.RatePerSec)
	l.lastRefill = now
	return l.tokens
}
