package gtw

import (
	"context"
	"fmt"
	"slices"
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

// lookup reports whether the repository's catalog was already
// verified.
func (c *LabelCache) lookup(owner, repo string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.repos[owner+"/"+repo]
	return ok
}

// store records the repository as verified. Only successful checks
// reach here — a failure is never cached, so the next command
// checks again.
func (c *LabelCache) store(owner, repo string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.repos == nil {
		c.repos = make(map[string]struct{})
	}
	c.repos[owner+"/"+repo] = struct{}{}
}

// ensureRepoLabels verifies that every label in AllLabels exists in
// the repository's label catalog and creates the ones that are
// missing.
//
// It returns the labels it created, so the caller can tell the user
// that the repository was repaired. An error means the catalog could
// not be verified — the caller decides whether that is fatal.
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
) (created []string, err error) {
	if deps.LabelCache.lookup(owner, repo) {
		return nil, nil
	}
	catalog, err := provider.ListLabels(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("read label catalog: %w", err)
	}
	present := make(map[string]struct{}, len(catalog))
	for _, name := range catalog {
		present[name] = struct{}{}
	}
	for _, name := range AllLabels {
		if _, ok := present[name]; ok {
			continue
		}
		meta := LabelMetaFor(name)
		if err := provider.CreateLabel(ctx, owner, repo, name, meta.Color, meta.Description); err != nil {
			return created, fmt.Errorf("ensure %q: %w", name, err)
		}
		created = append(created, name)
	}
	deps.LabelCache.store(owner, repo)
	return created, nil
}

// labelCatalogNote explains an empty queue in terms of the label
// catalog: the queue marker exists in the repository, so the queue is
// empty because nobody has attached it to an issue yet.
//
// When the check created the queue marker during this run, the note
// says so — the repository was repaired, and the user is the one who
// fills the queue.
func labelCatalogNote(created []string) string {
	if slices.Contains(created, LabelAssigned) {
		return fmt.Sprintf("`%s` was just created in this repository — attach it to an issue to fill the queue",
			LabelAssigned)
	}
	return fmt.Sprintf("`%s` exists in this repository — attach it to an issue to fill the queue",
		LabelAssigned)
}

// labelCheckWarning is the house warning line for a label check that
// could not be completed. The commands that do not attach labels
// warn and carry on; only /gtw fix, which attaches one right away,
// treats the failure as fatal.
func labelCheckWarning(owner, repo string, err error) string {
	return fmt.Sprintf("⚠️ could not ensure gtw labels on %s/%s: %v", owner, repo, err)
}

// staleCatalogNote explains why attaching a label can fail even
// though the catalog was verified: the catalog may have changed
// since the verification, and the remedy is to re-verify or to
// create the label by hand.
func staleCatalogNote(owner, repo, label string) string {
	return fmt.Sprintf(
		"→ the label catalog of %s/%s was verified earlier in this session — it may have changed since\n"+
			"↳ restart the nightme daemon to re-verify it, or create %q by hand",
		owner, repo, label)
}
