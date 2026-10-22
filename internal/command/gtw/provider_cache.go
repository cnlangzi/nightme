package gtw

import (
	"context"
	"sync"
)

// providerIdentity is the cacheable half of a detected provider.
// Platform identity is a function of the host alone — Stage B
// probes <host>/api/v4/version and <host>/api/v3/meta — so kind,
// host and version are host-determined and safe to memoise.
type providerIdentity struct {
	kind    ProviderKind
	host    string
	version string
}

// provider rebuilds a provider from a cached identity, bound to
// the worktree of the call that hit the cache.
//
// It deliberately does not hand back the instance that was cached:
// providers carry a per-call Worktree field so gh / glab don't
// inherit the daemon's stale CWD, and a memoised instance would
// pin every later call to the first caller's directory.
func (id providerIdentity) provider(worktree string) GitProvider {
	switch id.kind {
	case ProviderGitHub:
		return &GitHubProvider{host: id.host, Worktree: worktree}
	case ProviderGitLab:
		return &GitLabProvider{host: id.host, version: id.version, Worktree: worktree}
	}
	return nil
}

// ProviderCache memoises platform detection per remote host for the
// lifetime of the daemon process. Nothing is written to disk, so a
// restart re-probes.
//
// Detection costs up to four HTTP round-trips (two endpoints, each
// tried over HTTPS then plain HTTP on its own defaultProbeTimeout
// budget) and one host shows up in every repo the user owns — /gtw
// new, /gtw fix, /gtw push, /gtw pr and the per-stamp CollectPR
// each paid it again. Keying by host instead of cwd keeps the entry
// count at "hosts this daemon has talked to" rather than growing
// with every directory the user visits.
//
// Only successful detections are stored. A host that was
// unreachable a minute ago may well be reachable now, and these
// paths are user-triggered, so the next attempt re-probes instead
// of replaying a stale failure.
//
// nil is a valid inert cache — every lookup misses and every store
// is dropped — which is the behaviour any HandlerDeps that doesn't
// wire one had before detection was memoised.
type ProviderCache struct {
	mu    sync.RWMutex
	hosts map[string]providerIdentity
}

// lookup returns the identity memoised for host, if any.
func (c *ProviderCache) lookup(host string) (providerIdentity, bool) {
	if c == nil {
		return providerIdentity{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.hosts[host]
	return id, ok
}

// store memoises id for host. Only successful detections reach
// here — failures are never cached.
func (c *ProviderCache) store(host string, id providerIdentity) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hosts == nil {
		c.hosts = make(map[string]providerIdentity)
	}
	c.hosts[host] = id
}

// detectProvider is the single detection entry point for /gtw:
// resolveProvider, runFixRemote and CollectPR all go through it, so
// the nil-Detect / nil-Prober fallbacks and the host-keyed cache
// live in one place instead of being re-derived per call site.
func detectProvider(ctx context.Context, deps HandlerDeps, remoteURL, worktree string) (GitProvider, error) {
	// An injected Detect owns its own behaviour: its providers need
	// not be the built-in GitHub/GitLab ones, and a cache hit has to
	// rebuild from an identity to rebind the worktree — which would
	// silently swap the caller's implementation out from under it.
	if deps.Detect != nil {
		return deps.Detect(ctx, remoteURL, deps.prober(), worktree)
	}
	// Lexical and cheap: a malformed URL fails here, before the cache
	// lookup and before any network work.
	host, err := parseRemoteHost(remoteURL)
	if err != nil {
		return nil, err
	}
	if id, ok := deps.ProviderCache.lookup(host); ok {
		if prov := id.provider(worktree); prov != nil {
			return prov, nil
		}
	}
	prov, err := Detect(ctx, remoteURL, deps.prober(), worktree)
	if err != nil || prov == nil {
		return prov, err
	}
	deps.ProviderCache.store(host, providerIdentity{
		kind:    prov.Kind(),
		host:    prov.Host(),
		version: prov.Version(),
	})
	return prov, nil
}

// prober resolves HandlerDeps.Prober to the HTTPProber Detect
// should use. nil falls back to the production prober, whose zero
// Timeout means defaultProbeTimeout per scheme attempt.
func (d HandlerDeps) prober() HTTPProber {
	if d.Prober != nil {
		return d.Prober
	}
	return &ExecHTTPProber{}
}
