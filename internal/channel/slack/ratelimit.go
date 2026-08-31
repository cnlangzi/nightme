// Package slack — global token-bucket limiter.
//
// Slack rate limits the adapter cares about
// (https://docs.slack.dev/apis/web-api/rate-limits):
//
//   - chat.appendStream — Tier 4, 100+/min (~1.67/s)
//   - chat.startStream / chat.stopStream — Tier 2, 20+/min
//   - chat.postMessage — 1/second/channel
//   - chat.update — Tier 3, 50+/min
//   - reactions.add / remove — Tier 1, pooled
//
// The bucket is GLOBAL (one per Adapter), not per-chat. nightme runs
// many chats in parallel against a single Slack app, so a per-chat
// limiter would let N concurrent turns multiply straight past the app
// quota: at the default 3s per-turn stream throttle, ~5 concurrent
// turns already saturate Tier 4. See docs/channel/slack.md §2.6.
//
// The per-turn throttle in stream.go is a presentation choice; this
// bucket is the protection layer. Both apply.
package slack

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"
)

// DefaultLimiterConfig is deliberately below Tier 4's ~1.67/s floor.
// Slack does not publish burst allowances, and a 429 that forces a
// server-dictated backoff looks worse to the user than a marginally
// slower tick.
var DefaultLimiterConfig = LimiterConfig{
	RatePerSec: 1.5,
	Burst:      3,
}

// LimiterConfig configures the rate limiter.
type LimiterConfig struct {
	RatePerSec float64
	Burst      int
}

// Limiter is a lazy-refill token bucket. No background goroutine, so
// there is nothing to leak when an Adapter is discarded.
type Limiter struct {
	cfg    LimiterConfig
	logger *slog.Logger

	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// NewLimiter constructs a Limiter. cfg == nil or zero fields fall
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
				l.logger.Debug("slack rate limit blocked",
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

// Tokens returns the current bucket level (test hook).
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
