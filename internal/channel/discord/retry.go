// Package discord — Layer 1 transient error retry.
//
// Discord API errors fall into three buckets:
//
//  1. Transient: 5xx server errors, network I/O timeouts, connection
//     resets, DNS hiccups, HTTP 429 with Retry-After. Retry with
//     exponential backoff.
//  2. Rate limit: 429 Too Many Requests. Honour the server-supplied
//     Retry-After / X-RateLimit-Reset-After, then fall back to the
//     exponential backoff used for other transients.
//  3. Terminal: 4xx business errors. 400 (bad request), 401 (token
//     invalid — stop reconnect loop), 403 (missing scope /
//     privilege), 404 (channel gone). Surface to caller; no retry.
package discord

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"syscall"
	"time"
)

// DefaultRetryConfig is the conservative production default.
// Mirrors telegram.DefaultRetryConfig so both channels have
// comparable retry behaviour for the same daemon profile.
var DefaultRetryConfig = RetryConfig{
	MaxAttempts:    3,
	InitialBackoff: 500 * time.Millisecond,
	MaxBackoff:     5 * time.Second,
	JitterPercent:  0.25,
}

// RetryConfig configures WithTransientRetry. Production code
// uses DefaultRetryConfig; tests inject shorter backoffs.
type RetryConfig struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	JitterPercent  float64
}

func (c RetryConfig) normalize() RetryConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = DefaultRetryConfig.MaxAttempts
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = DefaultRetryConfig.InitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultRetryConfig.MaxBackoff
	}
	if c.JitterPercent < 0 {
		c.JitterPercent = DefaultRetryConfig.JitterPercent
	}
	if c.JitterPercent > 1 {
		c.JitterPercent = 1
	}
	return c
}

// RetryOpts is the per-call context for retry.
type RetryOpts struct {
	Op     string
	Cfg    RetryConfig
	Logger *slog.Logger
	Attrs  []any
}

func (o RetryOpts) cfg() RetryConfig { return o.Cfg.normalize() }
func (o RetryOpts) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// IsTransient classifies err as retryable.
//
//	Transient:
//	- net.Error.Timeout() == true
//	- io.EOF / io.ErrUnexpectedEOF
//	- syscall.ECONNRESET / EPIPE
//	- HTTP 5xx (StatusCode >= 500)
//	- HTTP 429
//	- Substring matches: "connection reset", "broken pipe",
//	  "i/o timeout", "tls handshake timeout", "connection refused",
//	  "no such host"
//
//	Terminal:
//	- context.Canceled / DeadlineExceeded
//	- HTTP 4xx (StatusCode 400..499, except 429) — including 401
//	  (token revoked → caller must stop the reconnect loop).
//	- nil
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 429:
			return true
		case apiErr.StatusCode >= 500:
			return true
		case apiErr.StatusCode >= 400:
			return false
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"tls handshake timeout",
		"connection refused",
		"no such host",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func retryAfter(err error) time.Duration {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return 0
	}
	return apiErr.RetryAfter
}

// WithTransientRetry calls fn, retrying transient errors with
// exponential backoff and jitter. Behaviour matches
// telegram.WithTransientRetry.
func WithTransientRetry(ctx context.Context, opts RetryOpts, fn func() error) error {
	logger := opts.logger()
	cfg := opts.cfg()
	var lastErr error
	backoff := cfg.InitialBackoff
	totalWait := time.Duration(0)

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			if attempt > 1 {
				logger.Debug("discord transient retry succeeded",
					append([]any{"op", opts.Op, "attempt", attempt}, opts.Attrs...)...)
			}
			return nil
		}
		lastErr = err
		if !IsTransient(err) {
			return err
		}
		if attempt == cfg.MaxAttempts {
			logger.Warn("discord retry exhausted",
				append([]any{
					"op", opts.Op,
					"attempts", attempt,
					"total_wait_ms", totalWait.Milliseconds(),
					"final_err", err.Error(),
				}, opts.Attrs...)...)
			return err
		}

		wait := jitter(backoff, cfg.JitterPercent)
		if server := retryAfter(err); server > wait {
			wait = server
		}
		logger.Debug("discord transient retry scheduled",
			append([]any{
				"op", opts.Op,
				"attempt", attempt,
				"wait_ms", wait.Milliseconds(),
				"err", err.Error(),
			}, opts.Attrs...)...)

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			totalWait += wait
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}

		backoff *= 2
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}
	return lastErr
}

// WithTransientRetryResult is the generic-result variant.
func WithTransientRetryResult[T any](ctx context.Context, opts RetryOpts, fn func() (T, error)) (T, error) {
	var zero T
	logger := opts.logger()
	cfg := opts.cfg()
	var lastErr error
	var lastResult T
	backoff := cfg.InitialBackoff
	totalWait := time.Duration(0)

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		result, err := fn()
		if err == nil {
			if attempt > 1 {
				logger.Debug("discord transient retry succeeded",
					append([]any{"op", opts.Op, "attempt", attempt}, opts.Attrs...)...)
			}
			return result, nil
		}
		lastErr = err
		lastResult = result
		if !IsTransient(err) {
			return result, err
		}
		if attempt == cfg.MaxAttempts {
			logger.Warn("discord retry exhausted",
				append([]any{
					"op", opts.Op,
					"attempts", attempt,
					"total_wait_ms", totalWait.Milliseconds(),
					"final_err", err.Error(),
				}, opts.Attrs...)...)
			return lastResult, err
		}

		wait := jitter(backoff, cfg.JitterPercent)
		if server := retryAfter(err); server > wait {
			wait = server
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			totalWait += wait
		case <-ctx.Done():
			timer.Stop()
			return lastResult, ctx.Err()
		}

		backoff *= 2
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}
	return lastResult, lastErr
}

func jitter(base time.Duration, pct float64) time.Duration {
	if pct <= 0 {
		return base
	}
	delta := float64(base) * pct
	return base + time.Duration((rand.Float64()*2-1)*delta)
}
