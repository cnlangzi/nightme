// Package version — runtime version-check support.
//
// check.go is the bridge between the build-time identity
// (version.go: Version / GitCommit / BuildDate) and the live
// release feed served by nightme.dev. It exposes three things
// the REPL startup path needs:
//
//   - Latest(): fetch the latest release tag from nightme.dev.
//   - IsOutdated(current, latest): semver compare against the
//     tag returned by Latest().
//   - CachedCheck: a small on-disk cache that throttles how
//     often REPL startup pings the API. Network failure, JSON
//     parse failure, and rate-limit (HTTP 403 / 429) responses
//     all degrade silently — the REPL must never block on a
//     slow or unreachable nightme.dev.
//
// Why stdlib only (no go-github-selfupdate)?
// We only do "is there a newer release?" this round; the actual
// download / replace lives behind `nightme update`, which is
// still a stub. When the download path lands we'll likely swap
// in a small self-update helper, but the check layer should
// stay minimal and predictable.
//
// # Endpoint choice
//
// The version endpoint is https://nightme.dev/api/version. The
// response shape (observed 2026-08-17):
//
//	{
//	  "current":     "dev",
//	  "latest_cli":  "0.3.7",
//	  "commit":      "unknown",
//	  "updated_at":  "2026-08-17T06:56:53.154834639+08:00"
//	}
//
// We read `latest_cli` as the latest version. `current` is the
// server-side "currently recommended" pointer; if `latest_cli`
// is missing we fall back to it. The decoder also tolerates
// `tag_name` / `tag` / `version` so a future rename doesn't
// silently break every client.
package version

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/httpclient"
	"github.com/cnlangzi/nightme/internal/pathutil"
	"golang.org/x/mod/semver"
)

// DefaultVersionURL is the canonical endpoint. Held as a var
// (not const) so tests can swap it; production callers use it
// unchanged.
var DefaultVersionURL = "https://nightme.dev/api/version"

// checkTTL is how long a cached "latest version" is trusted.
// REPL startup happens repeatedly in the dev workflow, so we
// don't want to ping the API on every bare `nightme` invocation.
// 24h is the same window `brew` / `apt` use by default. (Note:
// the endpoint itself sends cache-control: max-age=60 from its
// CDN; that controls the CDN's caching, not ours. Our client-
// side TTL is a deliberate over-estimate so dev iteration does
// not hammer the API.)
const checkTTL = 24 * time.Hour

// httpTimeout caps the API fetch. The REPL must never appear to
// hang waiting on a slow nightme.dev response — 5s is generous
// for a single GET.
const httpTimeout = 5 * time.Second

// Checker holds the knobs the test harness needs to swap
// (HTTP transport, endpoint URL, now function) without touching
// production callers. Production code uses DefaultChecker().
type Checker struct {
	// VersionURL is the endpoint to GET. Empty falls back to
	// DefaultVersionURL. Tests override this to point at an
	// httptest server.
	VersionURL string

	// HTTPClient is the transport used for the fetch. nil
	// falls back to httpclient.DefaultWithTimeout(httpTimeout).
	HTTPClient *http.Client

	// Now lets tests pin "time" without sleeping. nil = time.Now.
	Now func() time.Time

	// CachePath is the file used for the throttle cache. nil =
	// disable caching (every call hits the API). Tests override
	// this with a t.TempDir() path.
	CachePath string
}

// DefaultChecker returns a Checker configured for production:
// the real nightme.dev endpoint, a real HTTP client, and the
// cache file under cfg.Paths.DataDir/version-check.json.
//
// It returns the Checker and the cache path it picked (so
// callers can surface "where the cache lives" in diagnostics).
//
// dataDir is the nightme data dir (cfg.Paths.DataDir). When
// empty (e.g. tests that don't have a config yet), caching is
// disabled.
func DefaultChecker(dataDir string) (*Checker, string) {
	c := &Checker{
		VersionURL: DefaultVersionURL,
		HTTPClient: httpclient.DefaultWithTimeout(httpTimeout),
		Now:        time.Now,
	}
	if dataDir != "" {
		// F-PATHUTIL-001 §13.3.1: pathutil.Join for cross-
		// platform separator handling, AND NormalizeForOS on
		// dataDir first because cfg.Paths.DataDir is user-
		// supplied (YAML) and on Windows is commonly written
		// with forward slashes (Git Bash / WSL copy-paste
		// habits). Without the Normalize, filepath.Join would
		// return "F:/foo\version-check.json" — a mixed-
		// separator path that os.OpenFile on Windows rejects.
		if n, err := pathutil.NormalizeForOS(dataDir); err == nil {
			dataDir = n
		}
		c.CachePath = pathutil.Join(dataDir, "version-check.json")
		return c, c.CachePath
	}
	return c, ""
}

// CheckResult is what the REPL consumes. Latest == current
// means up-to-date; Latest != current + semver compare says
// outdated. FromCache tells the caller whether the answer was
// served from disk (so the UI can label it "last checked 3h ago").
type CheckResult struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest"`
	Outdated  bool      `json:"outdated"`
	FromCache bool      `json:"from_cache"`
	CheckedAt time.Time `json:"checked_at"`
}

// Check performs a throttled version lookup:
//
//  1. If the cache file exists and is younger than checkTTL,
//     serve the cached Latest and return immediately. No network.
//  2. Otherwise call fetchLatest against nightme.dev. On any
//     error, swallow it and fall back to the stale cache (if
//     any), otherwise return an empty CheckResult with no
//     error — the REPL must not surface "API unreachable" to
//     the user on every cold start.
//  3. On success, write the new cache file (best effort).
//
// The returned error is only non-nil when the caller asked
// for something we can't honour at all (e.g. semver parse of
// the cached value). Network / API errors are logged via the
// supplied logger, not returned.
func (c *Checker) Check(ctx context.Context, currentVersion string, logf func(string, ...any)) CheckResult {
	now := c.now()
	current := normalize(currentVersion)

	// Step 1: cache hit.
	if c.CachePath != "" {
		if cached, ok := c.readCache(); ok {
			age := now.Sub(cached.CheckedAt)
			if age < checkTTL && cached.Latest != "" {
				return CheckResult{
					Current:   current,
					Latest:    cached.Latest,
					Outdated:  isOutdated(current, cached.Latest),
					FromCache: true,
					CheckedAt: cached.CheckedAt,
				}
			}
		}
	}

	// Step 2: live fetch.
	latestRaw, err := c.fetchLatest(ctx)
	if err != nil {
		if logf != nil {
			logf("version check: %v", err)
		}
		// Fall back to whatever the cache held, even if stale.
		if c.CachePath != "" {
			if cached, ok := c.readCache(); ok && cached.Latest != "" {
				return CheckResult{
					Current:   current,
					Latest:    cached.Latest,
					Outdated:  isOutdated(current, cached.Latest),
					FromCache: true,
					CheckedAt: cached.CheckedAt,
				}
			}
		}
		// Nothing on disk either — return a zero result and let
		// the caller silently skip the prompt.
		return CheckResult{Current: current}
	}

	latest := normalize(latestRaw)

	// Step 3: persist (best effort).
	if c.CachePath != "" {
		_ = c.writeCache(cacheEntry{Latest: latest, CheckedAt: now})
	}

	return CheckResult{
		Current:   current,
		Latest:    latest,
		Outdated:  isOutdated(current, latest),
		FromCache: false,
		CheckedAt: now,
	}
}

// now() with the override hook.
func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// httpDo is the network call. Wrapped so tests can swap the
// transport via Checker.HTTPClient.
func (c *Checker) httpDo(req *http.Request) (*http.Response, error) {
	client := c.HTTPClient
	if client == nil {
		client = httpclient.DefaultWithTimeout(httpTimeout)
	}
	return client.Do(req)
}

// fetchLatest hits the configured endpoint and returns the
// latest CLI version string (e.g. "0.3.7"). The endpoint URL
// lives on Checker.VersionURL (defaults to DefaultVersionURL).
// The call carries a User-Agent because some intermediaries
// reject empty UAs.
//
// Response shape (preferred → fallback order):
//   - latest_cli   ← primary; what nightme.dev/api/version emits
//   - current      ← server-side "currently recommended"
//   - tag_name     ← legacy GitHub shape (so we can swap back)
//   - tag / version ← final fallback for any other shape
func (c *Checker) fetchLatest(ctx context.Context) (string, error) {
	url := c.VersionURL
	if url == "" {
		url = DefaultVersionURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nightme/"+Version)

	resp, err := c.httpDo(req)
	if err != nil {
		return "", fmt.Errorf("version api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		switch resp.StatusCode {
		case http.StatusForbidden, http.StatusTooManyRequests:
			// Rate limited — caller should treat as soft failure.
			return "", fmt.Errorf("version api: rate limited (HTTP %d)", resp.StatusCode)
		case http.StatusNotFound:
			return "", errors.New("version api: endpoint not found")
		default:
			return "", fmt.Errorf("version api: HTTP %d", resp.StatusCode)
		}
	}

	// Decode into a permissive shape first, then pick the best
	// field. Doing it this way means we keep working when the
	// server renames fields, instead of hard-failing on a
	// single missing key.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("version api: decode: %w", err)
	}
	for _, key := range []string{"latest_cli", "current", "tag_name", "tag", "version"} {
		rawValue, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(rawValue, &s); err != nil {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// `current` is intentionally a fallback: many servers
		// (including nightme.dev today) put a literal "dev"
		// there. If we'd hit it first we'd conclude the user
		// is on the latest. Skip it unless it's the only
		// field with a usable string.
		if key == "current" && len(raw) > 1 {
			continue
		}
		return s, nil
	}
	return "", errors.New("version api: no usable version field in response")
}

// cacheEntry is what we persist. Field tags match the on-disk
// JSON so external tooling (or `cat version-check.json` from
// a debugger) reads cleanly.
type cacheEntry struct {
	Latest    string    `json:"latest_version"`
	CheckedAt time.Time `json:"checked_at"`
}

// readCache returns (entry, true) when the file exists and
// parses cleanly. A missing file is not an error — first run.
func (c *Checker) readCache() (cacheEntry, bool) {
	if c.CachePath == "" {
		return cacheEntry{}, false
	}
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		// Corrupt cache: treat as missing. The next write
		// will overwrite.
		return cacheEntry{}, false
	}
	return e, true
}

// writeCache persists the entry. Best effort: we never surface
// a write error to the caller because the REPL must not
// refuse to start over a flaky disk.
func (c *Checker) writeCache(e cacheEntry) error {
	if c.CachePath == "" {
		return nil
	}
	// F-PATHUTIL-001 §13.3.1: pathutil.Dir for the cache parent.
	// c.CachePath is already in canonical form (DefaultChecker
	// NormalizeForOS'd dataDir before joining), so this is
	// equivalent to filepath.Dir but keeps the rule honest.
	if err := os.MkdirAll(pathutil.Dir(c.CachePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.CachePath, data, 0o600)
}

// normalize strips a leading "v"/"V" so "v0.2.0" and "0.2.0"
// both compare correctly. golang.org/x/mod/semver requires
// the "v" prefix, so we add it back inside canonical().
func normalize(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}

// Normalize is the exported form of normalize. Callers that
// display or persist a version should use this so GitHub's
// "v0.3.10" and nightme.dev's "0.3.10" render the same.
func Normalize(v string) string { return normalize(v) }

// Tag returns the GitHub-style tag ("v0.3.10") for a version
// written either with or without the leading v. Empty input
// stays empty.
func Tag(v string) string {
	n := normalize(v)
	if n == "" {
		return ""
	}
	return "v" + n
}

// canonical returns the form semver.Compare expects ("v0.2.0").
// An empty / unparseable string becomes "v0.0.0" so the
// compare degrades to "we're newer than nothing" instead of
// erroring out — still not great, but the REPL path catches
// it via the IsOutdated bool.
func canonical(v string) string {
	v = normalize(v)
	if v == "" {
		return "v0.0.0"
	}
	if !semver.IsValid("v" + v) {
		return "v0.0.0"
	}
	return "v" + v
}

// isOutdated reports whether current < latest under semver
// rules. Equal or newer returns false.
func isOutdated(current, latest string) bool {
	cur := canonical(current)
	lat := canonical(latest)
	if cur == "v0.0.0" || lat == "v0.0.0" {
		// Unparseable inputs — fall back to string compare
		// so dev builds with odd version strings still get
		// some answer.
		return cur < lat
	}
	return semver.Compare(cur, lat) < 0
}

// Equal reports whether a and b name the same release,
// ignoring a leading "v" and surrounding whitespace.
// "0.3.10" and "v0.3.10" are equal; "0.3.10" and "0.3.11"
// are not.
func Equal(a, b string) bool {
	ca, cb := canonical(a), canonical(b)
	if ca == "v0.0.0" || cb == "v0.0.0" {
		return normalize(a) == normalize(b) && normalize(a) != ""
	}
	return semver.Compare(ca, cb) == 0
}

// IsOutdated is the exported alias used by other packages
// (e.g. internal/updater.Check) that need to compare a
// current build version against a latest tag without taking
// a dependency on the Checker's on-disk cache.
func IsOutdated(current, latest string) bool { return isOutdated(current, latest) }
