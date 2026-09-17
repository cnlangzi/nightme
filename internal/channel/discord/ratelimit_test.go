package discord

import (
	"context"
	"testing"
	"time"
)

func TestLimiter_WaitReturnsImmediatelyWhenTokensAvailable(t *testing.T) {
	l := NewLimiter(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err != nil {
		t.Errorf("Wait on fresh limiter: %v", err)
	}
}

func TestLimiter_UpdateFromHeaders_RecordsBucket(t *testing.T) {
	l := NewLimiter(nil, nil)
	reset := float64(time.Now().Add(time.Hour).Unix())
	l.UpdateFromHeaders("test-bucket", 3, 5, reset, false, 0)

	// Wait should still succeed because the global bucket has
	// tokens independent of the per-bucket key.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err != nil {
		t.Errorf("Wait after header update: %v", err)
	}
}

func TestLimiter_GlobalRetryAfterHonored(t *testing.T) {
	l := NewLimiter(nil, nil)
	l.UpdateFromHeaders("", 0, 0, 0, true, 200*time.Millisecond)

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Errorf("Wait did not honor Retry-After: elapsed=%v", elapsed)
	}
}

func TestParseRateLimitHeaders(t *testing.T) {
	headers := map[string][]string{
		"X-RateLimit-Bucket":    {"abcd1234"},
		"X-RateLimit-Limit":     {"5"},
		"X-RateLimit-Remaining": {"3"},
		"X-RateLimit-Reset":     {"1700000000.5"},
		"X-RateLimit-Global":    {"true"},
		"Retry-After":           {"1.5"},
	}
	bucket, remaining, limit, _, global, retryAfter := parseRateLimitHeaders(headers)
	if bucket != "abcd1234" {
		t.Errorf("bucket = %q", bucket)
	}
	if remaining != 3 {
		t.Errorf("remaining = %d", remaining)
	}
	if limit != 5 {
		t.Errorf("limit = %d", limit)
	}
	if !global {
		t.Error("global = false, want true")
	}
	if retryAfter != 1500*time.Millisecond {
		t.Errorf("retryAfter = %v", retryAfter)
	}
}

func TestParseRateLimitHeaders_Missing(t *testing.T) {
	bucket, remaining, limit, _, global, retryAfter := parseRateLimitHeaders(map[string][]string{})
	if bucket != "" || remaining != 0 || limit != 0 || global || retryAfter != 0 {
		t.Errorf("expected zero values for missing headers, got bucket=%q remaining=%d limit=%d global=%v retryAfter=%v",
			bucket, remaining, limit, global, retryAfter)
	}
}

func TestParseRateLimitHeaders_ResetAfterFallback(t *testing.T) {
	headers := map[string][]string{
		"X-RateLimit-Reset-After": {"2.0"},
	}
	_, _, _, _, _, retryAfter := parseRateLimitHeaders(headers)
	if retryAfter != 2*time.Second {
		t.Errorf("retryAfter = %v, want 2s", retryAfter)
	}
}

func TestEqualFoldASCII(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"X-RateLimit-Bucket", "x-ratelimit-bucket", true},
		{"X-Ratelimit-Bucket", "X-Ratelimit-Bucket", true},
		{"X-Ratelimit-Bucket", "Y-Ratelimit-Bucket", false},
		{"", "", true},
		{"a", "ab", false},
	}
	for _, tc := range cases {
		if got := equalFoldASCII(tc.a, tc.b); got != tc.want {
			t.Errorf("equalFoldASCII(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
