// Package discord — token-bucket rate limiter keyed by Discord's
// X-RateLimit-Bucket header.
//
// Discord rate-limit shape (per the developer docs §Rate Limits):
//
//   - Per-route bucket identified by X-RateLimit-Bucket. Per-route
//     limits vary; the bucket key is opaque on our side.
//   - Global cap: 50 req/s/token.
//   - 429 responses carry X-RateLimit-Global (when the cap applies)
//     and X-RateLimit-Scope ("user" / "global" / "shared"); retry
//     after the smaller of X-RateLimit-Reset-After and Retry-After.
//   - Headers also carry X-RateLimit-Limit / -Remaining / -Reset
//     (epoch seconds) so the client can pre-emptively Wait before
//     sending the next call.
//
// The limiter is advisory: callers should Wait() before every API
// call. api.go owns this discipline.
package discord

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"
)

// DefaultLimiterConfig is the conservative default. Per-route
// buckets default to 5 req/s with burst 5 (Discord's most common
// per-bucket cap), and the global bucket runs at 50 req/s with
// burst 1 to match Discord's documented cap.
var DefaultLimiterConfig = LimiterConfig{
	PerBucketRatePerSec: 5,
	PerBucketBurst:      5,
	GlobalRatePerSec:    50,
	GlobalBurst:         1,
}

// LimiterConfig configures the rate limiter.
type LimiterConfig struct {
	PerBucketRatePerSec float64
	PerBucketBurst      int
	GlobalRatePerSec    float64
	GlobalBurst         int
}

// Limiter is a token bucket per Discord X-RateLimit-Bucket plus a
// shared global bucket covering the 50 req/s/token cap.
type Limiter struct {
	cfg    LimiterConfig
	logger *slog.Logger

	mu         sync.Mutex
	buckets    map[string]*bucket
	retryAfter time.Time // X-RateLimit-Global backoff
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	resetAt    time.Time
}

// NewLimiter constructs a Limiter. nil cfg falls back to defaults.
func NewLimiter(cfg *LimiterConfig, logger *slog.Logger) *Limiter {
	c := DefaultLimiterConfig
	if cfg != nil {
		if cfg.PerBucketRatePerSec > 0 {
			c.PerBucketRatePerSec = cfg.PerBucketRatePerSec
		}
		if cfg.PerBucketBurst > 0 {
			c.PerBucketBurst = cfg.PerBucketBurst
		}
		if cfg.GlobalRatePerSec > 0 {
			c.GlobalRatePerSec = cfg.GlobalRatePerSec
		}
		if cfg.GlobalBurst > 0 {
			c.GlobalBurst = cfg.GlobalBurst
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Limiter{
		cfg:     c,
		logger:  logger,
		buckets: make(map[string]*bucket),
	}
}

// Wait blocks until a token is available on the global bucket (and
// implicitly on the per-route bucket — but per-bucket wait is
// driven by UpdateFromHeaders rather than by Wait, because
// per-route limits vary and the bucket key only emerges from the
// first response). Returns ctx.Err() on cancel.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	started := time.Now()
	for {
		l.mu.Lock()
		// Respect a server-supplied global backoff window.
		if !l.retryAfter.IsZero() {
			wait := time.Until(l.retryAfter)
			if wait > 0 {
				l.mu.Unlock()
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
				continue
			}
			l.retryAfter = time.Time{}
		}

		now := time.Now()
		refillGlobal(l, now)
		if l.buckets["__global__"].tokens >= 1.0 {
			l.buckets["__global__"].tokens -= 1.0
			l.mu.Unlock()
			if d := time.Since(started); d > 100*time.Millisecond {
				l.logger.Debug("discord rate limit blocked",
					"wait_ms", d.Milliseconds(),
					"global_rate_per_sec", l.cfg.GlobalRatePerSec,
				)
			}
			return nil
		}

		deficit := 1.0 - l.buckets["__global__"].tokens
		waitSec := deficit / l.cfg.GlobalRatePerSec
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

// UpdateFromHeaders applies Discord's response headers to the
// limiter state. Called after every API response.
//
// bucketKey is the value of X-RateLimit-Bucket (empty for endpoints
// that don't set one — fall through to the global bucket only).
// remaining / limit drive the per-bucket token count; reset drives
// when the bucket refills. retryAfter triggers a global backoff
// when X-RateLimit-Global was true on a 429.
func (l *Limiter) UpdateFromHeaders(bucketKey string, remaining, limit int, resetEpoch float64, global bool, retryAfter time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if global && retryAfter > 0 {
		l.retryAfter = time.Now().Add(retryAfter)
	}
	if bucketKey == "" {
		return
	}
	b, ok := l.buckets[bucketKey]
	if !ok {
		b = &bucket{tokens: float64(l.cfg.PerBucketBurst), lastRefill: time.Now()}
		l.buckets[bucketKey] = b
	}
	if remaining >= 0 && limit > 0 {
		b.tokens = math.Min(float64(limit), float64(remaining))
	}
	if resetEpoch > 0 {
		b.resetAt = time.Unix(int64(resetEpoch), 0)
	}
}

func refillGlobal(l *Limiter, now time.Time) {
	b, ok := l.buckets["__global__"]
	if !ok {
		b = &bucket{tokens: float64(l.cfg.GlobalBurst), lastRefill: now}
		l.buckets["__global__"] = b
	}
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = math.Min(float64(l.cfg.GlobalBurst), b.tokens+elapsed*l.cfg.GlobalRatePerSec)
	b.lastRefill = now
}

// parseRateLimitHeaders reads the standard Discord headers from
// an http.Response and returns the structured values used by
// UpdateFromHeaders. All values are optional; the parser returns
// zero when the header is missing or unparseable.
func parseRateLimitHeaders(headers map[string][]string) (bucket string, remaining, limit int, reset float64, global bool, retryAfter time.Duration) {
	if v := firstHeader(headers, "X-Ratelimit-Bucket"); v != "" {
		bucket = v
	}
	if v := firstHeader(headers, "X-Ratelimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			remaining = n
		}
	}
	if v := firstHeader(headers, "X-Ratelimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if v := firstHeader(headers, "X-Ratelimit-Reset"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			reset = f
		}
	}
	if v := firstHeader(headers, "X-Ratelimit-Global"); v == "true" {
		global = true
	}
	if v := firstHeader(headers, "Retry-After"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			retryAfter = time.Duration(f * float64(time.Second))
		}
	}
	if v := firstHeader(headers, "X-Ratelimit-Reset-After"); v != "" && retryAfter == 0 {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			retryAfter = time.Duration(f * float64(time.Second))
		}
	}
	return bucket, remaining, limit, reset, global, retryAfter
}

func firstHeader(headers map[string][]string, name string) string {
	if values := headers[name]; len(values) > 0 {
		return values[0]
	}
	// http.Header canonicalises names; callers iterating a non-canonical
	// map should still get a hit.
	for k, values := range headers {
		if len(values) > 0 && equalFoldASCII(k, name) {
			return values[0]
		}
	}
	return ""
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
