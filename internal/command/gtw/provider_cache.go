package gtw

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// providerIdentity is the cacheable half of a detected provider.
// Only probed (Stage B) detections are stored: those are a function
// of the host alone — Stage B probes <host>/api/v4/version and
// <host>/api/v3/meta — so kind, host and version are host-determined
// and safe to memoise. Stage A's URL hint is deliberately excluded;
// see detectProvider.
type providerIdentity struct {
	kind    ProviderKind
	host    string
	version string
}

// provider rebuilds a provider from a cached identity, bound to
// the directory the CLI of the call that hit the cache runs in.
//
// It deliberately does not hand back the instance that was cached:
// providers carry a per-call directory field so gh / glab don't
// inherit the daemon's stale CWD, and a memoised instance would
// pin every later call to the first caller's directory.
func (id providerIdentity) provider(dir string) GitProvider {
	switch id.kind {
	case ProviderGitHub:
		return &GitHubProvider{host: id.host, Worktree: dir}
	case ProviderGitLab:
		return &GitLabProvider{host: id.host, version: id.version, Worktree: dir}
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
//
// dir is the directory the provider's CLI runs in. It must exist
// when the CLI is spawned: a bound directory that is gone makes the
// spawn fail with an ENOENT that names the binary rather than the
// directory.
func detectProvider(ctx context.Context, deps HandlerDeps, remoteURL, dir string) (GitProvider, error) {
	// An injected Detect owns its own behaviour: its providers need
	// not be the built-in GitHub/GitLab ones, and a cache hit has to
	// rebuild from an identity to rebind the directory — which would
	// silently swap the caller's implementation out from under it.
	if deps.Detect != nil {
		return deps.Detect(ctx, remoteURL, deps.prober(), dir)
	}
	// Lexical and cheap: a malformed URL fails here, before the cache
	// lookup and before any network work.
	host, err := parseRemoteHost(remoteURL)
	if err != nil {
		return nil, err
	}
	if id, ok := deps.ProviderCache.lookup(host); ok {
		if prov := id.provider(dir); prov != nil {
			return prov, nil
		}
	}
	prov, probed, err := detect(ctx, remoteURL, deps.prober(), dir)
	if err != nil || prov == nil {
		return prov, err
	}
	// Only a probed answer is host-shaped. Detect's Stage A hint reads
	// the whole raw remote URL, so "…/gitlab-mirror/foo.git" yields a
	// GitLab verdict that says nothing about the other repos on that
	// host; memoising it would pin every future remote on the host to
	// whatever substring that first URL happened to contain.
	if probed {
		deps.ProviderCache.store(host, providerIdentity{
			kind:    prov.Kind(),
			host:    prov.Host(),
			version: prov.Version(),
		})
	}
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

// providerErrorCard maps a provider-detection failure to the card
// the user reads and the outcome summary the program reads.
//
// The three failures are told apart because their remedies differ: a
// malformed remote URL is the user's to fix, an unreachable host is
// the network's, and an unrecognised host is a platform nightme does
// not speak to. The remote URL is redacted — it may carry userinfo.
//
// The commands that answer the user with a card read it here, so
// their cards cannot drift apart.
func providerErrorCard(remoteURL string, err error) (card, summary string) {
	redacted := redactForDisplay(remoteURL)
	switch {
	case errors.Is(err, ErrInvalidRemoteURL):
		if redacted == "" {
			return "❌ 无效的 remote URL（凭证已脱敏）\n  Expected: https://github.com/<owner>/<repo>.git, git@github.com:<owner>/<repo>.git, ssh://git@<host>/path, git://<host>/path, etc.",
				"the remote URL is not valid"
		}
		return fmt.Sprintf("❌ 无效的 remote URL: %s\n  Expected: https://github.com/<owner>/<repo>.git, git@github.com:<owner>/<repo>.git, ssh://git@<host>/path, git://<host>/path, etc.", redacted),
			"the remote URL is not valid"
	case errors.Is(err, ErrProviderUnreachable):
		return fmt.Sprintf("❌ 无法连接 Git 主机 (%s)\n  → %v", redacted, err),
			"the git host could not be reached"
	default:
		return fmt.Sprintf("❌ 暂不支持的 Git 平台 (host: %s — neither github.com/gitlab.com URL hint nor /api/v3/meta or /api/v4/version probe recognised it).", redacted),
			"the git platform is not supported"
	}
}

// remoteTarget is the tracker a command talks to: the repository the
// chat's current working directory lives in, and the provider behind
// its origin remote.
type remoteTarget struct {
	// RepoRoot is the repository root of the chat's cwd.
	RepoRoot string
	// Provider is the resolved platform provider.
	Provider GitProvider
	// Owner and Repo are the "owner/repo" half of the remote URL.
	Owner string
	Repo  string
}

// resolveRemoteTarget resolves the tracker for the chat's current
// working directory.
//
// It answers the user itself when the resolution fails: the reply is
// sent and the outcome reported, and a non-nil *Result comes back —
// the same shape loadDispatchContext uses. The caller returns that
// result verbatim.
//
// The chat's cwd must be set; the callers check that before they
// get here.
func resolveRemoteTarget(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	chatID, messageID string,
) (remoteTarget, *Result) {
	repoRoot, err := RepoRoot(ctx, cs.SelectedCwd(), deps.Git)
	if err != nil {
		return remoteTarget{}, replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ Not in a git repository. Run /cwd <inside a repo> first.",
			command.FailedOutcome("not_a_repo", "the workspace is not inside a git repository"))
	}
	remoteURL, err := RemoteOriginURL(ctx, repoRoot, deps.Git)
	if err != nil || remoteURL == "" {
		return remoteTarget{}, replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ No `origin` remote. Add one with `git remote add origin <url>`.",
			command.FailedOutcome("no_remote", "the repository has no origin remote"))
	}
	// The provider needs a real directory to run gh / glab in — the
	// repository root is one, even when the daemon's own cwd is
	// stale.
	provider, err := detectProvider(ctx, deps, remoteURL, repoRoot)
	if err != nil {
		// D3 split: distinguish "URL is malformed" (user error),
		// "daemon can't reach the host" (network) from "host not
		// recognised as GitHub/GitLab".
		card, summary := providerErrorCard(remoteURL, err)
		return remoteTarget{}, replyOutcome(ctx, cs.Emitter(), chatID, messageID, card,
			command.FailedOutcome("provider_error", summary))
	}
	if provider == nil {
		return remoteTarget{}, replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			"❌ Provider detection returned no result (deps.Detect override bug).",
			command.FailedOutcome("provider_error", "the git platform could not be detected"))
	}
	owner, repo, err := ParseRepoOwner(remoteURL)
	if err != nil {
		return remoteTarget{}, replyOutcome(ctx, cs.Emitter(), chatID, messageID,
			fmt.Sprintf("❌ Cannot parse owner/repo from remote URL %s.", redactForDisplay(remoteURL)),
			command.FailedOutcome("provider_error", "the remote URL does not name a repository"))
	}
	return remoteTarget{
		RepoRoot: repoRoot,
		Provider: provider,
		Owner:    owner,
		Repo:     repo,
	}, nil
}
