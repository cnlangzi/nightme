// Package slack — transient error retry.
//
// Slack failures split three ways:
//
//  1. Rate limit: HTTP 429. slack-go surfaces it as
//     *slack.RateLimitedError carrying the server's Retry-After.
//     Honour that value verbatim — Slack's burst allowances are not
//     published, so the server's number beats any local guess.
//  2. Transient: 5xx, network timeouts, connection resets, EOF.
//     Retry with exponential backoff + jitter.
//  3. Terminal: business errors (channel_not_found, not_in_channel,
//     msg_too_long, invalid_thread_ts, …). Surface to the caller.
//
// Follows internal/channel/feishu/retry.go and the telegram port.
// The notable Slack-specific bit is that Retry-After is re-read on
// every attempt rather than baked into the config, because the value
// changes per response.
package slack

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

	slackgo "github.com/slack-go/slack"
)

// DefaultRetryConfig mirrors the feishu and telegram defaults so all
// three channels behave comparably for the same daemon profile.
var DefaultRetryConfig = RetryConfig{
	MaxAttempts:    3,
	InitialBackoff: 500 * time.Millisecond,
	MaxBackoff:     5 * time.Second,
	JitterPercent:  0.25,
}

// RetryConfig configures withTransientRetry.
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
	return c
}

// isRateLimited reports whether err is Slack's 429 and returns the
// server-supplied wait.
func isRateLimited(err error) (time.Duration, bool) {
	var rl *slackgo.RateLimitedError
	if errors.As(err, &rl) {
		return rl.RetryAfter, true
	}
	return 0, false
}

// isTransient reports whether err is worth retrying.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := isRateLimited(err); ok {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// slack-go returns plain errors for HTTP-level failures; match on
	// the 5xx shape it formats ("slack server error: 503 ...").
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"internal_error", "service_unavailable", "server error",
		"connection reset", "broken pipe", "timeout", "eof",
		"too many requests",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

// withTransientRetry runs fn, retrying transient failures. A 429
// waits the server-dictated Retry-After instead of the exponential
// schedule.
func withTransientRetry(ctx context.Context, cfg RetryConfig, logger *slog.Logger, op string, fn func() error) error {
	c := cfg.normalize()
	if logger == nil {
		logger = slog.Default()
	}
	backoff := c.InitialBackoff
	var lastErr error
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isTransient(err) {
			return err
		}
		if attempt == c.MaxAttempts {
			break
		}

		wait := jitter(backoff, c.JitterPercent)
		if retryAfter, ok := isRateLimited(err); ok && retryAfter > 0 {
			// Server knows better than our schedule.
			wait = retryAfter
			logger.Warn("slack: rate limited, honouring Retry-After",
				"op", op, "attempt", attempt, "retry_after", retryAfter)
		} else {
			logger.Debug("slack: transient failure, retrying",
				"op", op, "attempt", attempt, "wait", wait, "err", err)
		}

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}

		backoff *= 2
		if backoff > c.MaxBackoff {
			backoff = c.MaxBackoff
		}
	}
	return lastErr
}

// jitter spreads retries so concurrent turns don't resynchronise on
// the same backoff boundary.
func jitter(base time.Duration, pct float64) time.Duration {
	if pct <= 0 {
		return base
	}
	delta := float64(base) * pct
	// rand without an explicit seed is fine here: jitter only needs
	// to decorrelate goroutines, not resist prediction.
	offset := (rand.Float64()*2 - 1) * delta
	out := time.Duration(float64(base) + offset)
	if out < 0 {
		return base
	}
	return out
}
