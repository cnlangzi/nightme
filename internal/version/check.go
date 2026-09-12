// Package version — runtime version-check support.
//
// check.go is the bridge between the build-time identity
// (version.go: Version / GitCommit / BuildDate) and the live
// release feed. It exposes:
//
//   - LatestTagLookup: the network seam. Production wires it
//     to updater.LookupLatestTag (nightme.dev → GitHub fallback).
//   - Check: throttled lookup + 24h on-disk cache. The REPL
//     must never block on a slow or unreachable source, so
//     failures degrade silently.
//
// check.go does NOT touch the download path — the CLI / REPL
// stage 2 (download + verify + extract) calls
// updater.DownloadTag directly with the resolved tag. Keeping
// detection and download on separate I/O paths means a slow
// SHA256SUMS.txt or asset download can't block the REPL's
// startup probe, and a failed download can't poison the
// cached "what's latest" answer.
package version

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/pathutil"
	"golang.org/x/mod/semver"
)

// checkTTL is how long a cached "latest tag" is trusted.
// REPL startup happens repeatedly in the dev workflow, so we
// don't want to ping the API on every bare `nightme`
// invocation. 24h matches `brew` / `apt`.
const checkTTL = 24 * time.Hour

// httpTimeout caps the API fetch. The REPL must never appear
// to hang waiting on a slow nightme.dev / GitHub response —
// 5s is generous for a single GET.
const httpTimeout = 5 * time.Second

// LatestTagLookup is the network seam the Checker calls into.
// It returns the latest (or pinned) release tag — "v0.5.0" form,
// with the leading v — plus the source label and any error.
// Production wires this to updater.LookupLatestTag; tests
// inject a stub.
type LatestTagLookup func(ctx context.Context, tag string) (string, string, error)

// Checker holds the knobs the test harness needs to swap
// (Lookup function, cache path, now function) without touching
// production callers. Production code uses DefaultChecker().
type Checker struct {
	// Lookup is the network entry point. Required: Check
	// returns an empty result when Lookup is nil.
	Lookup LatestTagLookup

	// HTTPTimeout caps each Lookup call. 0 = use httpTimeout.
	HTTPTimeout time.Duration

	// CacheTTL is how long a cached result is trusted. 0 = use checkTTL.
	CacheTTL time.Duration

	// CachePath is the file used for the throttle cache. Empty
	// disables caching (every call hits Lookup).
	CachePath string

	// Now lets tests pin "time" without sleeping. nil = time.Now.
	Now func() time.Time
}

// DefaultChecker returns a Checker configured for production:
// real HTTP timeout, real cache file under
// cfg.Paths.DataDir/version-check.json. Lookup is NOT set —
// the caller (cmd/nightme) attaches updater.LookupLatestTag
// after construction.
func DefaultChecker(dataDir string) (*Checker, string) {
	c := &Checker{
		HTTPTimeout: httpTimeout,
		CacheTTL:    checkTTL,
		Now:         time.Now,
	}
	if dataDir != "" {
		// F-PATHUTIL-001 §13.3.1: pathutil.Join for cross-
		// platform separator handling, AND NormalizeForOS on
		// dataDir first because cfg.Paths.DataDir is user-
		// supplied (YAML) and on Windows is commonly written
		// with forward slashes.
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
	Current    string    `json:"current"`
	Latest     string    `json:"latest"`
	Outdated   bool      `json:"outdated"`
	ReleasedAt time.Time `json:"released_at,omitempty"`
	Source     string    `json:"source,omitempty"`
	FromCache  bool      `json:"from_cache"`
	CheckedAt  time.Time `json:"checked_at"`
}

// Check performs a throttled tag lookup:
//
//  1. If the cache file exists and is younger than CacheTTL,
//     serve the cached Latest and return immediately. No network.
//  2. Otherwise call Lookup. On any error, swallow it and fall
//     back to the stale cache (if any), otherwise return an
//     empty CheckResult with no error — the REPL must not
//     surface "API unreachable" to the user on every cold start.
//  3. On success, write the new cache file (best effort).
//
// The returned CheckResult has no error path. Network / API
// errors are logged via the supplied logger, not returned;
// the REPL treats empty Latest as "skip the prompt".
func (c *Checker) Check(ctx context.Context, currentVersion string, logf func(string, ...any)) CheckResult {
	if c.Lookup == nil {
		// Construction contract violation: Lookup is the
		// only network seam and DefaultChecker leaves it
		// nil on purpose (cycle avoidance). Return a zero
		// result so the caller degrades silently.
		return CheckResult{Current: normalize(currentVersion)}
	}

	now := c.now()
	current := normalize(currentVersion)

	if c.CachePath != "" {
		if cached, ok := c.readCache(); ok {
			age := now.Sub(cached.CheckedAt)
			ttl := c.CacheTTL
			if ttl <= 0 {
				ttl = checkTTL
			}
			if age < ttl && cached.Latest != "" {
				return CheckResult{
					Current:   current,
					Latest:    cached.Latest,
					Outdated:  isOutdated(current, cached.Latest),
					Source:    cached.Source,
					FromCache: true,
					CheckedAt: cached.CheckedAt,
				}
			}
		}
	}

	timeout := c.HTTPTimeout
	if timeout <= 0 {
		timeout = httpTimeout
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tag, source, err := c.Lookup(fctx, "")
	if err != nil {
		if logf != nil {
			logf("version check: %v", err)
		}
		if c.CachePath != "" {
			if cached, ok := c.readCache(); ok && cached.Latest != "" {
				return CheckResult{
					Current:   current,
					Latest:    cached.Latest,
					Outdated:  isOutdated(current, cached.Latest),
					Source:    cached.Source,
					FromCache: true,
					CheckedAt: cached.CheckedAt,
				}
			}
		}
		return CheckResult{Current: current}
	}
	if tag == "" {
		if logf != nil {
			logf("version check: empty tag")
		}
		if c.CachePath != "" {
			if cached, ok := c.readCache(); ok && cached.Latest != "" {
				return CheckResult{
					Current:   current,
					Latest:    cached.Latest,
					Outdated:  isOutdated(current, cached.Latest),
					Source:    cached.Source,
					FromCache: true,
					CheckedAt: cached.CheckedAt,
				}
			}
		}
		return CheckResult{Current: current}
	}

	if c.CachePath != "" {
		_ = c.writeCache(cacheEntry{
			Latest:    tag,
			Source:    source,
			CheckedAt: now,
		})
	}

	return CheckResult{
		Current:   current,
		Latest:    tag,
		Outdated:  isOutdated(current, tag),
		Source:    source,
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

// cacheEntry is what we persist. The schema is intentionally
// minimal — just the tag, source, and timestamp — because
// anything else (full release payload, asset list) bloats the
// cache file for no gain: stage 2 re-fetches whatever it needs
// from updater.DownloadTag.
type cacheEntry struct {
	Latest    string    `json:"latest"`
	Source    string    `json:"source,omitempty"`
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
		return cacheEntry{}, false
	}
	return e, true
}

// writeCache persists the entry. Best effort: we never surface
// a write error to the caller.
func (c *Checker) writeCache(e cacheEntry) error {
	if c.CachePath == "" {
		return nil
	}
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

// Normalize is the exported form of normalize.
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
// rules.
func isOutdated(current, latest string) bool {
	cur := canonical(current)
	lat := canonical(latest)
	if cur == "v0.0.0" || lat == "v0.0.0" {
		return cur < lat
	}
	return semver.Compare(cur, lat) < 0
}

// Equal reports whether a and b name the same release.
func Equal(a, b string) bool {
	ca, cb := canonical(a), canonical(b)
	if ca == "v0.0.0" || cb == "v0.0.0" {
		return normalize(a) == normalize(b) && normalize(a) != ""
	}
	return semver.Compare(ca, cb) == 0
}

// IsOutdated is the exported alias used by other packages
// that need to compare a build version against a latest tag
// without depending on the Checker's on-disk cache.
func IsOutdated(current, latest string) bool { return isOutdated(current, latest) }
