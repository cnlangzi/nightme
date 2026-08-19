// Package telegram — Layer 1 transient error retry.
//
// Telegram Bot API errors fall into three buckets:
//
//  1. Transient: 5xx server errors, network I/O timeouts, connection
//     resets, DNS hiccups, HTTP 429 with retry_after. Retry with
//     exponential backoff.
//  2. Rate limit: 429 Too Many Requests. Honour the server-supplied
//     retry_after (apiError.RetryAfter), then fall back to the
//     exponential backoff used for other transients.
//  3. Terminal: 4xx business errors (chat not found, message is not
//     modified, bot was kicked, …). Surface to caller; no retry.
//
// Design follows internal/channel/feishu/retry.go. Notable differences:
//   - We re-read apiError.RetryAfter between attempts (the value can
//     change per response), so we don't bake it into RetryOpts.
//   - We don't try to extract a Feishu-style "business code" — Telegram
//     uses human-readable descriptions and is_finite error classes
//     (4xx vs 5xx), not numeric codes.
package telegram

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

// DefaultRetryConfig is the conservative production default. Mirrors
// feishu.DefaultRetryConfig so both channels have comparable retry
// behaviour for the same daemon profile.
var DefaultRetryConfig = RetryConfig{
	MaxAttempts:    3,             // initial + 2 retries
	InitialBackoff: 500 * time.Millisecond,
	MaxBackoff:     5 * time.Second,
	JitterPercent:  0.25,          // ±25%
}

// RetryConfig configures WithTransientRetry. Production code uses
// DefaultRetryConfig; tests may inject shorter backoffs.
type RetryConfig struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	JitterPercent  float64
}

// normalize fills zero fields with DefaultRetryConfig values.
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

// RetryOpts is the per-call context for retry. Op is required (e.g.
// "send_message", "edit_message", "set_reaction"). Cfg and Logger
// fall back to defaults.
type RetryOpts struct {
	Op     string
	Cfg    RetryConfig
	Logger *slog.Logger
	// Attrs are appended to every retry log line.
	Attrs []any
}

func (o RetryOpts) cfg() RetryConfig  { return o.Cfg.normalize() }
func (o RetryOpts) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// IsTransient classifies err as retryable. Mirrors
// feishu.IsTransient but for Telegram-shaped errors.
//
//	Transient:
//	- net.Error.Timeout() == true
//	- io.EOF / io.ErrUnexpectedEOF
//	- syscall.ECONNRESET / EPIPE
//	- HTTP 5xx envelope (apiError.StatusCode >= 500)
//	- HTTP 429 (Telegram returns this via the standard apiError; the
//	  RetryAfter field is honoured separately by the caller)
//	- Substring matches: "connection reset", "broken pipe",
//	  "i/o timeout", "tls handshake timeout", "connection refused",
//	  "no such host"
//
//	Terminal (not retryable):
//	- context.Canceled / DeadlineExceeded
//	- HTTP 4xx envelope (apiError.StatusCode 400..499, except 429)
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

// retryAfter returns Telegram's suggested wait when the API
// returned 429. Returns 0 when err is not an apiError or has no
// retry_after. Callers should sleep for at least this long before
// the next attempt.
func retryAfter(err error) time.Duration {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return 0
	}
	if apiErr.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(apiErr.RetryAfter) * time.Second
}

// WithTransientRetry calls fn, retrying transient errors with
// exponential backoff and jitter. Behaviour matches
// feishu.WithTransientRetry:
//
//   - At most opts.Cfg.MaxAttempts attempts.
//   - Backoff doubles each retry, capped at MaxBackoff.
//   - Real wait = backoff * (1 + jitter * (2r - 1)), r uniform.
//   - On 429, the wait is max(exponential backoff, retryAfter(err)).
//   - ctx.Done() at any wait point returns ctx.Err() immediately.
//   - First non-transient error returns immediately, no retry.
//   - All attempts exhausted: returns the last transient error.
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
				logger.Debug("telegram transient retry succeeded",
					append([]any{"op", opts.Op, "attempt", attempt}, opts.Attrs...)...)
			}
			return nil
		}
		lastErr = err
		if !IsTransient(err) {
			return err
		}
		if attempt == cfg.MaxAttempts {
			logger.Warn("telegram retry exhausted",
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
		logger.Debug("telegram transient retry scheduled",
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

// WithTransientRetryResult is the generic-result variant of
// WithTransientRetry. fn returns (T, error); on success T is
// returned to the caller (matching fn's last success).
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
				logger.Debug("telegram transient retry succeeded",
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
			logger.Warn("telegram retry exhausted",
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

// jitter applies ±pct*100% randomness around base. r is uniform in
// [0,1). Kept private — tests assert via total wall-time bounds,
// not the exact value.
func jitter(base time.Duration, pct float64) time.Duration {
	if pct <= 0 {
		return base
	}
	delta := float64(base) * pct
	return base + time.Duration((rand.Float64()*2-1)*delta)
}
