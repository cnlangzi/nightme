package version

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubVersionHandler answers the nightme.dev /api/version
// payload. fetchLatest reads the latest_cli field first, with
// current as a fallback, so we include both in the shape so
// the tests double as a smoke for the field-priority logic.
func stubVersionHandler(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current":    "dev", // intentionally "dev" so we can
			// catch a regression where the decoder
			// picks `current` over `latest_cli`.
			"latest_cli": version,
			"commit":     "unknown",
			"updated_at": "2026-08-17T06:56:53.154834639+08:00",
		})
	})
}

func TestIsOutdated(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{"older patch", "0.1.0", "0.1.1", true},
		{"older minor", "0.1.0", "0.2.0", true},
		{"older major", "0.1.0", "1.0.0", true},
		{"equal", "0.2.0", "0.2.0", false},
		{"newer", "0.3.0", "0.2.0", false},
		{"strip v on current", "v0.1.0", "v0.2.0", true},
		{"strip v on latest", "0.1.0", "v0.2.0", true},
		{"dev build empty current stays comparable", "dev", "0.2.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOutdated(tt.current, tt.latest); got != tt.want {
				t.Errorf("isOutdated(%q, %q) = %v, want %v",
					tt.current, tt.latest, got, tt.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0.2.0", "0.2.0"},
		{"v0.2.0", "0.2.0"},
		{"  v0.2.0  ", "0.2.0"},
		{"  0.2.0\n", "0.2.0"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalize(tt.in); got != tt.want {
			t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestChecker_FetchLatest(t *testing.T) {
	srv := httptest.NewServer(stubVersionHandler("0.2.0"))
	defer srv.Close()

	c := &Checker{
		VersionURL: srv.URL,
		HTTPClient: &http.Client{Transport: redirectTo(srv.URL)},
		Now:        time.Now,
	}
	got, err := c.fetchLatest(context.Background())
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if got != "0.2.0" {
		t.Errorf("fetchLatest = %q, want %q", got, "0.2.0")
	}
}

// TestChecker_FetchLatest_FallbackOnCurrentOnly pins the
// decoder's field-priority logic: when only `current` is
// present (no latest_cli), we still read it — but if both are
// present we prefer latest_cli even when current looks like a
// real version string.
func TestChecker_FetchLatest_FallbackOnCurrentOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current":     "0.5.0",
			"latest_cli":  "0.6.0",
			"updated_at":  "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	c := &Checker{
		VersionURL: srv.URL,
		HTTPClient: &http.Client{Transport: redirectTo(srv.URL)},
	}
	got, err := c.fetchLatest(context.Background())
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if got != "0.6.0" {
		t.Errorf("fetchLatest = %q, want latest_cli %q (not current)", got, "0.6.0")
	}
}

// TestChecker_FetchLatest_LegacyTagName covers the rollback
// path: a server that still emits the GitHub-style payload
// (tag_name) should still work because we read tag_name as
// the third-priority field.
func TestChecker_FetchLatest_LegacyTagName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v0.7.0",
			"name":         "legacy",
			"published_at": "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	c := &Checker{
		VersionURL: srv.URL,
		HTTPClient: &http.Client{Transport: redirectTo(srv.URL)},
	}
	got, err := c.fetchLatest(context.Background())
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if got != "v0.7.0" {
		t.Errorf("fetchLatest = %q, want %q (legacy tag_name)", got, "v0.7.0")
	}
}

// TestChecker_FetchLatest_NoUsableField ensures we surface a
// clear error when the response is well-formed JSON but
// carries no version-shaped field at all.
func TestChecker_FetchLatest_NoUsableField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hello":"world","updated_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c := &Checker{
		VersionURL: srv.URL,
		HTTPClient: &http.Client{Transport: redirectTo(srv.URL)},
	}
	_, err := c.fetchLatest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no usable version field") {
		t.Fatalf("expected 'no usable version field' error, got %v", err)
	}
}

// redirectTo returns a RoundTripper that rewrites every URL
// to the supplied base. Lets us point Checker at an httptest
// server without making the repo field part of the URL.
func redirectTo(base string) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		// Copy the original request but swap scheme+host.
		cloned := req.Clone(req.Context())
		baseReq, _ := http.NewRequest(req.Method, base+req.URL.Path, nil)
		cloned.URL = baseReq.URL
		cloned.Host = baseReq.URL.Host
		return http.DefaultTransport.RoundTrip(cloned)
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChecker_FetchLatest_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	c := &Checker{
		VersionURL: srv.URL,
		HTTPClient: &http.Client{Transport: redirectTo(srv.URL)},
	}
	_, err := c.fetchLatest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected rate limited error, got %v", err)
	}
}

func TestChecker_Check_CacheHit(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "version-check.json")
	// Pre-seed a fresh cache so the live fetch is skipped.
	if err := os.WriteFile(cachePath, []byte(`{
		"latest_version": "9.9.9",
		"checked_at": "2026-01-01T00:00:00Z"
	}`), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Now must be just after the seed time so age < TTL.
	seed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := &Checker{
		VersionURL: "",
		HTTPClient: &http.Client{Transport: mustErrorTransport(t, "must not hit network")},
		CachePath:  cachePath,
		Now:        func() time.Time { return seed.Add(time.Minute) },
	}

	var logged []string
	res := c.Check(context.Background(), "0.1.0", func(format string, args ...any) {
		logged = append(logged, format)
	})
	if !res.FromCache {
		t.Errorf("FromCache = false, want true (live fetch happened)")
	}
	if res.Latest != "9.9.9" {
		t.Errorf("Latest = %q, want %q", res.Latest, "9.9.9")
	}
	if !res.Outdated {
		t.Errorf("Outdated = false, want true (0.1.0 < 9.9.9)")
	}
	if len(logged) != 0 {
		t.Errorf("expected no log lines on cache hit, got %v", logged)
	}
}

func TestChecker_Check_LiveFetchAndPersist(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "version-check.json")

	srv := httptest.NewServer(stubVersionHandler("9.9.9"))
	defer srv.Close()

	// Force "now" to a known instant so cache timestamp is
	// predictable.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	c := &Checker{
		VersionURL: srv.URL,
		HTTPClient: &http.Client{Transport: redirectTo(srv.URL)},
		CachePath:  cachePath,
		Now:        func() time.Time { return now },
	}

	res := c.Check(context.Background(), "0.1.0", nil)
	if res.FromCache {
		t.Errorf("FromCache = true, want false")
	}
	if res.Latest != "9.9.9" {
		t.Errorf("Latest = %q, want %q (normalized)", res.Latest, "9.9.9")
	}
	if !res.Outdated {
		t.Errorf("Outdated = false, want true")
	}
	if !res.CheckedAt.Equal(now) {
		t.Errorf("CheckedAt = %v, want %v", res.CheckedAt, now)
	}

	// Cache file must exist and round-trip.
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	if e.Latest != "9.9.9" || !e.CheckedAt.Equal(now) {
		t.Errorf("cache entry = %+v, want latest=9.9.9 checked_at=%v", e, now)
	}
}

func TestChecker_Check_NetworkFailureIsSoft(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "version-check.json")
	// Pre-seed stale-but-present cache so we hit the
	// fallback-on-error branch.
	if err := os.WriteFile(cachePath, []byte(`{
		"latest_version": "5.5.5",
		"checked_at": "2020-01-01T00:00:00Z"
	}`), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	c := &Checker{
		VersionURL: "",
		HTTPClient: &http.Client{Transport: mustErrorTransport(t, "boom")},
		CachePath:  cachePath,
		Now:        func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	var logged []string
	res := c.Check(context.Background(), "0.1.0", func(format string, args ...any) {
		logged = append(logged, format)
	})
	if !res.FromCache {
		t.Errorf("FromCache = false, want true (fallback to stale cache)")
	}
	if res.Latest != "5.5.5" {
		t.Errorf("Latest = %q, want stale %q", res.Latest, "5.5.5")
	}
	if len(logged) == 0 {
		t.Errorf("expected a log line for the network error, got none")
	}
}

func TestChecker_Check_NoCacheNoNetwork(t *testing.T) {
	c := &Checker{
		VersionURL: "",
		HTTPClient: &http.Client{Transport: mustErrorTransport(t, "boom")},
		// CachePath empty → no fallback, no persistence.
		Now: func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	res := c.Check(context.Background(), "0.1.0", nil)
	if res.Latest != "" {
		t.Errorf("Latest = %q, want empty (no cache, no network)", res.Latest)
	}
	if res.Outdated {
		t.Errorf("Outdated = true, want false (we can't tell without data)")
	}
}

func mustErrorTransport(t *testing.T, msg string) http.RoundTripper {
	t.Helper()
	return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, &transportErr{msg: msg}
	})
}

type transportErr struct{ msg string }

func (e *transportErr) Error() string { return e.msg }

func TestDefaultChecker(t *testing.T) {
	c, path := DefaultChecker(t.TempDir())
	if c == nil {
		t.Fatal("DefaultChecker returned nil")
	}
	if c.VersionURL != DefaultVersionURL {
		t.Errorf("VersionURL = %q, want %q", c.VersionURL, DefaultVersionURL)
	}
	if path == "" {
		t.Errorf("expected non-empty cache path")
	}
	if !strings.HasSuffix(path, "version-check.json") {
		t.Errorf("cache path %q missing version-check.json suffix", path)
	}
}

func TestDefaultChecker_EmptyDataDir(t *testing.T) {
	c, path := DefaultChecker("")
	if c == nil {
		t.Fatal("DefaultChecker returned nil")
	}
	if path != "" {
		t.Errorf("path = %q, want empty when dataDir is empty", path)
	}
	if c.CachePath != "" {
		t.Errorf("CachePath = %q, want empty", c.CachePath)
	}
}