package gtw

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// LabelCache memoises, per repository, that every label in AllLabels
// exists in that repository's label catalog. Nothing is written to
// disk, so a restart re-checks.
//
// The label catalog is a property of the repository, not of a
// worktree or a branch, so every worktree of the same repository
// shares one entry.
//
// Entries are never invalidated: once a repository is verified, that
// answer stands until the daemon restarts. A label a human deletes
// mid-session therefore surfaces when /gtw fix attaches it — the
// reply names the stale catalog and its remedy.
type LabelCache struct {
	mu    sync.RWMutex
	repos map[string]struct{}
}

// labelCacheKey identifies a repository's label catalog.
//
// The provider kind is part of the key: two platforms can host
// repositories with the same owner and name, and each platform has
// its own catalog. The host is not part of it — the yml-cached
// provider path resolves the same repository without a host, so a
// host in the key would give the same repository two entries
// depending on how the provider was resolved.
func labelCacheKey(kind ProviderKind, owner, repo string) string {
	return string(kind) + "/" + owner + "/" + repo
}

// lookup reports whether the repository's catalog was already
// verified.
func (c *LabelCache) lookup(kind ProviderKind, owner, repo string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.repos[labelCacheKey(kind, owner, repo)]
	return ok
}

// store records the repository as verified. Only successful checks
// reach here — a failure is never cached, so the next command
// checks again.
func (c *LabelCache) store(kind ProviderKind, owner, repo string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.repos == nil {
		c.repos = make(map[string]struct{})
	}
	c.repos[labelCacheKey(kind, owner, repo)] = struct{}{}
}

// labelCheck is what the catalog check found for a repository.
type labelCheck struct {
	// Created lists the labels the check created.
	Created []string
	// Verified is true when the check read the catalog during this
	// call. It is false when the answer came from the cache — the
	// catalog was verified earlier in the session, and it may have
	// changed since.
	Verified bool
}

// ensureRepoLabels verifies that every label in AllLabels exists in
// the repository's label catalog and creates the ones that are
// missing.
//
// It returns what it found, so the caller can tell the user that the
// repository was repaired. An error means the catalog could not be
// verified — the caller decides whether that is fatal.
//
// A verified repository is answered from the cache without any
// remote call. A failed check records nothing, so the next command
// checks again.
//
// The comparison against the catalog is exact: a label name is
// matched as the platform reports it, without trimming or case
// folding.
func ensureRepoLabels(
	ctx context.Context,
	deps HandlerDeps,
	provider GitProvider,
	owner, repo string,
) (labelCheck, error) {
	if deps.LabelCache.lookup(provider.Kind(), owner, repo) {
		return labelCheck{}, nil
	}
	catalog, err := provider.ListLabels(ctx, owner, repo)
	if err != nil {
		return labelCheck{}, fmt.Errorf("read label catalog: %w", err)
	}
	present := make(map[string]struct{}, len(catalog))
	for _, name := range catalog {
		present[name] = struct{}{}
	}
	var chk labelCheck
	for _, name := range AllLabels {
		if _, ok := present[name]; ok {
			continue
		}
		meta := LabelMetaFor(name)
		if err := provider.CreateLabel(ctx, owner, repo, name, meta.Color, meta.Description); err != nil {
			return chk, fmt.Errorf("ensure %q: %w", name, err)
		}
		chk.Created = append(chk.Created, name)
	}
	deps.LabelCache.store(provider.Kind(), owner, repo)
	chk.Verified = true
	return chk, nil
}

// labelCatalogNote explains an empty queue in terms of the label
// catalog: the queue marker is in the repository's catalog, so the
// queue is empty because nobody has attached it to an issue yet.
//
// When the check created the queue marker during this run, the note
// says so — the repository was repaired, and the user is the one who
// fills the queue. When the answer came from the cache, the note
// says the catalog was verified earlier: a label deleted since then
// would have left the queue empty the same way.
func labelCatalogNote(chk labelCheck) string {
	switch {
	case slices.Contains(chk.Created, LabelAssigned):
		return fmt.Sprintf("`%s` was just created in this repository — attach it to an issue to fill the queue",
			LabelAssigned)
	case !chk.Verified:
		return fmt.Sprintf("`%s` was verified in this repository's label catalog earlier in this session — attach it to an issue to fill the queue",
			LabelAssigned)
	default:
		return fmt.Sprintf("`%s` is in this repository's label catalog — attach it to an issue to fill the queue",
			LabelAssigned)
	}
}

// labelCheckWarning is the house warning line for a label check that
// could not be completed. The commands that do not attach labels
// warn and carry on; only /gtw fix, which attaches one right away,
// treats the failure as fatal.
func labelCheckWarning(owner, repo string, err error) string {
	return fmt.Sprintf("⚠️ could not ensure gtw labels on %s/%s: %v", owner, repo, err)
}

// prependWarning puts the warning lines at the front of a reply
// body, in the order they are given. Empty warnings are dropped; a
// reply without any warning is left alone.
func prependWarning(body string, warnings ...string) string {
	kept := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		if warning != "" {
			kept = append(kept, warning)
		}
	}
	if len(kept) == 0 {
		return body
	}
	return strings.Join(kept, "\n") + "\n" + body
}

// staleCatalogNote explains why attaching a label can fail even
// though the catalog was verified: the catalog may have changed
// since the verification, and the remedy is to re-verify or to
// create the label by hand.
//
// It is a possibility, not a diagnosis — the provider's own error
// above it says what actually failed.
func staleCatalogNote(owner, repo, label string) string {
	return fmt.Sprintf(
		"→ the label catalog of %s/%s was verified earlier in this session — if the label is missing now, it was changed since\n"+
			"↳ restart the nightme daemon to re-verify it, or create %q by hand",
		owner, repo, label)
}
