package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHttpREST_GetMe_Headers(t *testing.T) {
	var sawAuth string
	var sawUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawUA = r.Header.Get("User-Agent")
		if r.URL.Path != "/users/@me" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "9999", "username": "nightme-bot", "bot": true,
		})
	}))
	defer srv.Close()

	limiter := NewLimiter(&LimiterConfig{
		PerBucketRatePerSec: 1000,
		PerBucketBurst:      1000,
		GlobalRatePerSec:    1000,
		GlobalBurst:         1000,
	}, nil)
	// Override baseURL to point at the test server. The simplest
	// way without adding a seam is to use a custom httpREST that
	// we construct by hand.
	r := &httpREST{
		token:     "fake.token.x",
		baseURL:   srv.URL,
		limiter:   limiter,
		retry:     RetryConfig{MaxAttempts: 1, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond},
		client:    srv.Client(),
		userAgent: "DiscordBot (test)",
	}
	user, err := r.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if user.ID != "9999" {
		t.Errorf("ID = %q", user.ID)
	}
	if sawAuth != "Bot fake.token.x" {
		t.Errorf("Authorization = %q, want 'Bot fake.token.x'", sawAuth)
	}
	if sawUA != "DiscordBot (test)" {
		t.Errorf("User-Agent = %q", sawUA)
	}
}

func TestHttpREST_401IsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "401: Unauthorized", "code": 0}`))
	}))
	defer srv.Close()

	r := newRESTClient("bad.token", fastLimiter(), RetryConfig{MaxAttempts: 1, InitialBackoff: 1 * time.Millisecond}, "test-ua")
	r.baseURL = srv.URL
	r.client = srv.Client()

	_, err := r.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if IsTransient(err) {
		t.Error("401 should be terminal, not transient")
	}
	var apiErr *apiError
	if !asAPIError(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected apiError 401, got %v", err)
	}
}

func TestHttpREST_429HonorsRetryAfter(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0.05")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message": "rate limited"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1"})
	}))
	defer srv.Close()

	r := newRESTClient("x", fastLimiter(), RetryConfig{MaxAttempts: 3, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}, "test")
	r.baseURL = srv.URL
	r.client = srv.Client()

	if _, err := r.GetMe(context.Background()); err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (retry after 429)", calls)
	}
}

func TestHttpREST_5xxIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	}))
	defer srv.Close()

	r := newRESTClient("x", fastLimiter(), RetryConfig{MaxAttempts: 1, InitialBackoff: 1 * time.Millisecond, MaxBackoff: 1 * time.Millisecond}, "test")
	r.baseURL = srv.URL
	r.client = srv.Client()

	_, err := r.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !IsTransient(err) {
		t.Error("500 should be transient")
	}
}

func TestHttpREST_AddReaction_URLPathEncoding(t *testing.T) {
	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := newRESTClient("x", fastLimiter(), RetryConfig{MaxAttempts: 1}, "test")
	r.baseURL = srv.URL
	r.client = srv.Client()

	if err := r.AddReaction(context.Background(), "chan1", "msg1", "👌"); err != nil {
		t.Fatalf("AddReaction: %v", err)
	}
	if !strings.Contains(sawPath, "channels/chan1/messages/msg1/reactions/") {
		t.Errorf("path = %q", sawPath)
	}
	if !strings.HasSuffix(sawPath, "/@me") {
		t.Errorf("path = %q (missing /@me)", sawPath)
	}
}

func fastLimiter() *Limiter {
	return NewLimiter(&LimiterConfig{
		PerBucketRatePerSec: 1000,
		PerBucketBurst:      1000,
		GlobalRatePerSec:    1000,
		GlobalBurst:         1000,
	}, nil)
}

func asAPIError(err error, target **apiError) bool {
	if err == nil {
		return false
	}
	for cur := err; cur != nil; {
		if a, ok := cur.(*apiError); ok {
			*target = a
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := cur.(unwrapper)
		if !ok {
			return false
		}
		cur = u.Unwrap()
	}
	return false
}
