package version

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- pure-helper tests -----------------------------------

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
		{"equal mixed prefix", "0.3.10", "v0.3.10", false},
		{"equal mixed prefix reverse", "v0.3.10", "0.3.10", false},
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

func TestEqual(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"0.3.10", "0.3.10", true},
		{"0.3.10", "v0.3.10", true},
		{"v0.3.10", "0.3.10", true},
		{"V0.3.10", "0.3.10", true},
		{"  v0.3.10  ", "0.3.10", true},
		{"0.3.10", "0.3.11", false},
		{"0.3.10", "v0.3.11", false},
		{"", "", false},
		{"dev", "dev", true},
	}
	for _, tt := range tests {
		if got := Equal(tt.a, tt.b); got != tt.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0.2.0", "0.2.0"},
		{"v0.2.0", "0.2.0"},
		{"V0.2.0", "0.2.0"},
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

func TestTag(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0.3.10", "v0.3.10"},
		{"v0.3.10", "v0.3.10"},
		{"V0.3.10", "v0.3.10"},
		{"  0.3.10  ", "v0.3.10"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := Tag(tt.in); got != tt.want {
			t.Errorf("Tag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- lookup stubs ----------------------------------------

// stubLookup returns a LatestTagLookup that always returns
// the supplied tag. The call counter lets tests assert on
// how many times the network seam fired.
func stubLookup(tag string, calls *atomic.Int32) LatestTagLookup {
	return func(_ context.Context, _ string) (string, string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return tag, "nightme.dev", nil
	}
}

// errLookup returns a LatestTagLookup that always errors.
func errLookup(msg string, calls *atomic.Int32) LatestTagLookup {
	return func(_ context.Context, _ string) (string, string, error) {
		if calls != nil {
			calls.Add(1)
		}
		return "", "", errors.New(msg)
	}
}

// --- DefaultChecker / production wiring -----------------

func TestDefaultChecker(t *testing.T) {
	c, path := DefaultChecker(t.TempDir())
	if c == nil {
		t.Fatal("DefaultChecker returned nil")
	}
	if c.Lookup != nil {
		t.Errorf("DefaultChecker set Lookup; production wires it via cmd/nightme (cycle avoidance)")
	}
	if c.HTTPTimeout != httpTimeout {
		t.Errorf("HTTPTimeout = %v, want %v", c.HTTPTimeout, httpTimeout)
	}
	if c.CacheTTL != checkTTL {
		t.Errorf("CacheTTL = %v, want %v", c.CacheTTL, checkTTL)
	}
	if path == "" || !strings.HasSuffix(path, "version-check.json") {
		t.Errorf("cache path %q missing version-check.json suffix", path)
	}
}

func TestDefaultChecker_EmptyDataDir(t *testing.T) {
	c, path := DefaultChecker("")
	if c == nil {
		t.Fatal("DefaultChecker returned nil")
	}
	if path != "" || c.CachePath != "" {
		t.Errorf("expected empty path when dataDir is empty, got path=%q CachePath=%q", path, c.CachePath)
	}
}

// --- Check behavior -------------------------------------

func TestCheck_LookupNil_ReturnsZero(t *testing.T) {
	// Construction contract: DefaultChecker leaves Lookup nil
	// (cycle avoidance). Check must degrade silently rather
	// than panic when the caller forgot to wire Lookup.
	c := &Checker{}
	res := c.Check(context.Background(), "0.1.0", nil)
	if res.Latest != "" {
		t.Errorf("Latest = %q, want empty when Lookup nil", res.Latest)
	}
}

func TestCheck_CacheHit_DoesNotCallLookup(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "version-check.json")
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(cachePath, []byte(`{
		"latest": "9.9.9",
		"source": "nightme.dev",
		"checked_at": "`+now.UTC().Format(time.RFC3339)+`"
	}`), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var calls atomic.Int32
	c := &Checker{
		Lookup:    stubLookup("never-called", &calls),
		CachePath: cachePath,
		Now:       func() time.Time { return now.Add(time.Minute) },
	}

	res := c.Check(context.Background(), "0.1.0", nil)
	if !res.FromCache {
		t.Errorf("FromCache = false, want true")
	}
	if res.Latest != "9.9.9" {
		t.Errorf("Latest = %q, want %q", res.Latest, "9.9.9")
	}
	if !res.Outdated {
		t.Errorf("Outdated = false, want true")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("Lookup called %d times on cache hit; want 0", got)
	}
}

func TestCheck_CacheMiss_CallsLookup_StoresResult(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "version-check.json")

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	c := &Checker{
		Lookup:    stubLookup("v9.9.9", &calls),
		CachePath: cachePath,
		Now:       func() time.Time { return now },
	}

	res := c.Check(context.Background(), "0.1.0", nil)
	if res.FromCache {
		t.Errorf("FromCache = true on cache miss")
	}
	if res.Latest != "v9.9.9" {
		t.Errorf("Latest = %q, want %q", res.Latest, "v9.9.9")
	}
	if !res.Outdated {
		t.Errorf("Outdated = false, want true")
	}
	if res.Source != "nightme.dev" {
		t.Errorf("Source = %q, want %q", res.Source, "nightme.dev")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Lookup called %d times; want 1", got)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	if e.Latest != "v9.9.9" || e.Source != "nightme.dev" {
		t.Errorf("cache entry = %+v", e)
	}
}

func TestCheck_LookupFails_FallsBackToStaleCache(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "version-check.json")
	if err := os.WriteFile(cachePath, []byte(`{
		"latest": "5.5.5",
		"source": "github",
		"checked_at": "2020-01-01T00:00:00Z"
	}`), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var calls atomic.Int32
	c := &Checker{
		Lookup:    errLookup("network down", &calls),
		CachePath: cachePath,
		Now:       func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	var logged []string
	res := c.Check(context.Background(), "0.1.0", func(format string, args ...any) {
		logged = append(logged, format)
	})
	if !res.FromCache {
		t.Errorf("FromCache = false, want true (stale fallback)")
	}
	if res.Latest != "5.5.5" {
		t.Errorf("Latest = %q, want stale %q", res.Latest, "5.5.5")
	}
	if len(logged) == 0 {
		t.Errorf("expected a log line for the lookup failure")
	}
}

func TestCheck_BothFail_ReturnsZero(t *testing.T) {
	var calls atomic.Int32
	c := &Checker{
		Lookup: errLookup("network down", &calls),
		Now:    func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	res := c.Check(context.Background(), "0.1.0", nil)
	if res.Latest != "" {
		t.Errorf("Latest = %q, want empty (no cache, no network)", res.Latest)
	}
	if res.Outdated {
		t.Errorf("Outdated = true, want false")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Lookup called %d times; want 1", got)
	}
}

func TestCheck_EmptyTag_NotTreatedAsSuccess(t *testing.T) {
	var calls atomic.Int32
	emptyLookup := func(_ context.Context, _ string) (string, string, error) {
		calls.Add(1)
		return "", "nightme.dev", nil
	}
	c := &Checker{
		Lookup: emptyLookup,
		Now:    func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	res := c.Check(context.Background(), "0.1.0", nil)
	if res.Latest != "" {
		t.Errorf("Latest = %q, want empty for empty tag", res.Latest)
	}
}

func TestCheck_TimeoutRespected(t *testing.T) {
	slowLookup := func(ctx context.Context, _ string) (string, string, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return "v9.9.9", "nightme.dev", nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	c := &Checker{
		Lookup:      slowLookup,
		HTTPTimeout: 50 * time.Millisecond,
		Now:         func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	start := time.Now()
	res := c.Check(context.Background(), "0.1.0", nil)
	elapsed := time.Since(start)
	if elapsed > 150*time.Millisecond {
		t.Errorf("Check took %v, expected <150ms (timeout=50ms)", elapsed)
	}
	if res.Latest != "" {
		t.Errorf("Latest = %q, want empty (lookup timed out)", res.Latest)
	}
}

// TestCheck_LegacyCacheSchema_LatestVersionIsIgnored pins the
// upgrade path: a pre-rewrite cache file used `latest_version`,
// not `latest`. The new schema doesn't read `latest_version`,
// so the cache is treated as empty and a live Lookup fires.
func TestCheck_LegacyCacheSchema_LatestVersionIsIgnored(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "version-check.json")
	if err := os.WriteFile(cachePath, []byte(`{
		"latest_version": "9.9.9",
		"checked_at": "2026-06-01T11:59:00Z"
	}`), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	var calls atomic.Int32
	c := &Checker{
		Lookup:    stubLookup("v1.0.0", &calls),
		CachePath: cachePath,
		Now:       func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	res := c.Check(context.Background(), "0.1.0", nil)
	if res.Latest != "v1.0.0" {
		t.Errorf("Latest = %q, want %q (live lookup should override legacy schema)",
			res.Latest, "v1.0.0")
	}
	if res.FromCache {
		t.Errorf("FromCache = true; legacy schema must not satisfy the new reader")
	}
}

// TestCheck_TimeoutFieldDefaults verifies that HTTPTimeout=0
// falls back to the package's httpTimeout default.
func TestCheck_TimeoutFieldDefaults(t *testing.T) {
	c := &Checker{
		Lookup: stubLookup("v0.0.1", nil),
		Now:    func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
	if c.HTTPTimeout != 0 {
		t.Errorf("HTTPTimeout = %v, want 0 (test setup should leave it default)", c.HTTPTimeout)
	}
	if c.HTTPTimeout == 0 && httpTimeout == 0 {
		t.Errorf("both HTTPTimeout and httpTimeout are 0 — production would hang")
	}
}
