// Package version — runtime version-check support.
//
// check.go is the bridge between the build-time identity
// (version.go: Version / GitCommit / BuildDate) and the live
// release feed served by /releases/latest. It exposes:
//
//   - Latest release lookup: delegated to internal/updater,
//     which composes nightme.dev (primary) and GitHub (fallback)
//     in LookupForLatest. The detection path NEVER hits GitHub
//     directly when nightme.dev is up.
//   - CachedCheck: a small on-disk cache that throttles how
//     often REPL startup pings the release feed. Cache TTL is
//     24h (matches `brew` / `apt`). Lookup failure / network
//     timeout / parse failure all degrade silently — the REPL
//     must never block on a slow or unreachable source.
//
// What check.go does NOT do:
//   - Carry the *updater.Release downstream. The detection
//     layer only consumes tag_name (and, for display, the
//     published_at timestamp). The download stage re-fetches
//     via updater.LookupForDownload so its GitHub-first
//     fallback order stays independent of where detection
//     sourced the version from. Mixing the two would let
//     detection's choice of source poison download's priority.
//
// Why the version package owns the cache (not updater)?
// The cache is keyed on (current, latest, outdated) — version
// semantics — not on a *Release pointer. Keeping it here keeps
// updater focused on network orchestration and lets the CLI /
// REPL paths share one cache file under cfg.Paths.DataDir.
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

// checkTTL is how long a cached "latest version" is trusted.
// REPL startup happens repeatedly in the dev workflow, so we
// don't want to ping the API on every bare `nightme` invocation.
// 24h is the same window `brew` / `apt` use by default.
const checkTTL = 24 * time.Hour

// httpTimeout caps the API fetch. The REPL must never appear
// to hang waiting on a slow nightme.dev / GitHub response —
// 5s is generous for a single GET. Also matches the visible
// 5s countdown in repl_update_prompt.go so the UI and the
// HTTP client agree on "give up after 5s".
const httpTimeout = 5 * time.Second

// ReleaseMeta is the projection of a release payload that the
// version-check layer cares about. It deliberately carries
// only the fields CheckResult needs (tag + publish time); the
// download stage reads the full *updater.Release via its own
// LookupForDownload call. Keeping this type local to the
// version package avoids an import cycle (internal/updater
// already imports internal/version for UserAgent).
type ReleaseMeta struct {
	TagName     string
	PublishedAt time.Time
}

// ReleaseLookup is the network seam the Checker calls into.
// It returns the latest release's metadata, the source label
// ("nightme.dev" or "github"), and any error. Production
// wires this to internal/updater.LookupForLatest — see
// cmd/nightme/wire_updater.go for the thin adapter.
//
// Tests inject a stub that returns canned data.
type ReleaseLookup func(ctx context.Context, tag string) (ReleaseMeta, string, error)

// Checker holds the knobs the test harness needs to swap
// (Lookup function, cache path, now function) without touching
// production callers. Production code uses DefaultChecker().
//
// Lookup is the only network seam. DefaultChecker leaves it
// nil — the wiring site (cmd/nightme) attaches
// updater.LookupForLatest after construction so this package
// doesn't import internal/updater (cycle, see ReleaseMeta).
type Checker struct {
	// Lookup is the network entry point. Required: Check
	// returns an empty result when Lookup is nil.
	Lookup ReleaseLookup

	// HTTPTimeout caps each Lookup call. 0 = use httpTimeout.
	HTTPTimeout time.Duration

	// CacheTTL is how long a cached result is trusted. 0 = use checkTTL.
	CacheTTL time.Duration

	// CachePath is the file used for the throttle cache. Empty
	// disables caching (every call hits Lookup). Production wires
	// this to <DataDir>/version-check.json; tests use t.TempDir().
	CachePath string

	// Now lets tests pin "time" without sleeping. nil = time.Now.
	Now func() time.Time
}

// DefaultChecker returns a Checker configured for production:
// real HTTP timeout, real cache file under
// cfg.Paths.DataDir/version-check.json. Lookup is NOT set —
// the caller (cmd/nightme) attaches updater.LookupForLatest
// after construction. See the ReleaseMeta doc for the
// rationale.
//
// It returns the Checker and the cache path it picked (so
// callers can surface "where the cache lives" in diagnostics).
//
// dataDir is the nightme data dir (cfg.Paths.DataDir). When
// empty (e.g. tests that don't have a config yet), caching
// is disabled and CachePath stays empty.
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
//
// The result deliberately does NOT carry *updater.Release —
// see the package doc for why. The download stage re-fetches.
type CheckResult struct {
	Current    string    `json:"current"`
	Latest     string    `json:"latest"`
	Outdated   bool      `json:"outdated"`
	ReleasedAt time.Time `json:"released_at,omitempty"`
	Source     string    `json:"source,omitempty"`
	FromCache  bool      `json:"from_cache"`
	CheckedAt  time.Time `json:"checked_at"`
}

// Check performs a throttled version lookup:
//
//  1. If the cache file exists and is younger than CacheTTL,
//     serve the cached Latest and return immediately. No network.
//  2. Otherwise call Lookup (which itself tries nightme.dev
//     first then GitHub). On any error, swallow it and fall
//     back to the stale cache (if any), otherwise return an
//     empty CheckResult with no error — the REPL must not
//     surface "API unreachable" to the user on every cold start.
//  3. On success, write the new cache file (best effort).
//
// The returned CheckResult has no error path. Network / API
// errors are logged via the supplied logger, not returned;
// the REPL treats empty Latest as "skip the prompt".
func (c *Checker) Check(ctx context.Context, currentVersion string, logf func(string, ...any)) CheckResult {
	now := c.now()
	current := normalize(currentVersion)

	// Step 1: cache hit.
	if c.CachePath != "" {
		if cached, ok := c.readCache(); ok {
			age := now.Sub(cached.CheckedAt)
			ttl := c.CacheTTL
			if ttl <= 0 {
				ttl = checkTTL
			}
			if age < ttl && cached.Latest != "" {
				return CheckResult{
					Current:    current,
					Latest:     cached.Latest,
					Outdated:   isOutdated(current, cached.Latest),
					ReleasedAt: cached.ReleasedAt,
					Source:     cached.Source,
					FromCache:  true,
					CheckedAt:  cached.CheckedAt,
				}
			}
		}
	}

	// Step 2: live lookup with a hard timeout. The HTTPTimeout
	// field is the single source of truth; 0 falls back to the
	// default so callers that didn't set it still get the 5s
	// budget the REPL countdown is sized against.
	timeout := c.HTTPTimeout
	if timeout <= 0 {
		timeout = httpTimeout
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if c.Lookup == nil {
		// Lookup is a function field that DefaultChecker leaves
		// nil (cmd/nightme wires it via wireUpdaterLookup to
		// avoid an import cycle). When unwired — e.g. a third
		// party constructs a Checker directly — degrade the
		// same way a network error would.
		if logf != nil {
			logf("version check: no Lookup wired")
		}
		return CheckResult{Current: current}
	}
	meta, source, err := c.Lookup(fctx, "")
	if err != nil {
		if logf != nil {
			logf("version check: %v", err)
		}
		// Fall back to whatever the cache held, even if stale.
		if c.CachePath != "" {
			if cached, ok := c.readCache(); ok && cached.Latest != "" {
				return CheckResult{
					Current:    current,
					Latest:     cached.Latest,
					Outdated:   isOutdated(current, cached.Latest),
					ReleasedAt: cached.ReleasedAt,
					Source:     cached.Source,
					FromCache:  true,
					CheckedAt:  cached.CheckedAt,
				}
			}
		}
		// Nothing on disk either — return a zero result and let
		// the caller silently skip the prompt.
		return CheckResult{Current: current}
	}
	if meta.TagName == "" {
		// Lookup succeeded but produced no usable tag — the
		// 200-OK-with-empty-body case (e.g. a misbehaving mirror).
		// Treat the same as a network failure: stale cache, then
		// zero result.
		if logf != nil {
			logf("version check: empty release")
		}
		if c.CachePath != "" {
			if cached, ok := c.readCache(); ok && cached.Latest != "" {
				return CheckResult{
					Current:    current,
					Latest:     cached.Latest,
					Outdated:   isOutdated(current, cached.Latest),
					ReleasedAt: cached.ReleasedAt,
					Source:     cached.Source,
					FromCache:  true,
					CheckedAt:  cached.CheckedAt,
				}
			}
		}
		return CheckResult{Current: current}
	}

	// Step 3: persist (best effort).
	if c.CachePath != "" {
		_ = c.writeCache(cacheEntry{
			Latest:     meta.TagName,
			ReleasedAt: meta.PublishedAt,
			Source:     source,
			CheckedAt:  now,
		})
	}

	return CheckResult{
		Current:    current,
		Latest:     meta.TagName,
		Outdated:   isOutdated(current, meta.TagName),
		ReleasedAt: meta.PublishedAt,
		Source:     source,
		FromCache:  false,
		CheckedAt:  now,
	}
}

// now() with the override hook.
func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// cacheEntry is what we persist. Field tags match the on-disk
// JSON so external tooling (or `cat version-check.json` from
// a debugger) reads cleanly.
//
// ReleasedAt / Source are optional; older cache files written
// before they existed will deserialize with zero values, and
// the readCache path treats Latest == "" as a cache miss so the
// new fields can be filled in on the next Check.
type cacheEntry struct {
	Latest     string    `json:"latest"`
	ReleasedAt time.Time `json:"released_at,omitempty"`
	Source     string    `json:"source,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
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
// (e.g. internal/updater's older Check path) that need to
// compare a current build version against a latest tag without
// taking a dependency on the Checker's on-disk cache.
func IsOutdated(current, latest string) bool { return isOutdated(current, latest) }
