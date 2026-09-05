package wiki

import (
	"fmt"
	"path/filepath"
	"strings"
)

// GitRunner is the minimal git surface Plan needs. The
// production implementation shells out to `git`; tests use
// a fake.
//
// /wiki requires git — every SHA comparison and file diff
// goes through git. We never compute hashes ourselves: the
// working tree state is git's responsibility, and /wiki only
// reads committed history. Local uncommitted changes are
// invisible to /wiki by design (commit first, then run).
type GitRunner interface {
	// Head returns the current git HEAD SHA of cwd's repo.
	// Returns an error if cwd is not in a git repo or git
	// is unavailable.
	Head(cwd string) (string, error)
	// ChangedFiles returns the list of files changed between
	// `from` (exclusive) and HEAD (inclusive) of cwd's repo,
	// restricted to paths under `pathFilter` (empty = no
	// filter). Returns nil if `from` is empty or does not
	// resolve (treated as "everything changed").
	ChangedFiles(cwd, from, pathFilter string) ([]string, error)
	// IsClean reports whether cwd's working tree has no
	// uncommitted modifications. /wiki refuses to run when
	// the tree is dirty because incremental diffs depend on
	// committed SHAs — uncommitted changes would be invisible
	// to the diff and produce wrong "no changes" verdicts.
	IsClean(cwd string) (bool, error)
}

// planDiff is the per-module file-change information Plan
// uses to populate pendingEntry.FilesChanged. Empty when the
// module has no recorded LastSHA (treat as full regen) or when
// git cannot compute the diff (sha unreachable).
type planDiff struct {
	path         string
	filesChanged []string // empty means "module content changed, but git can't list specifics"
}

// Plan produces the incremental update list. It is purely
// mechanical — no file content is read beyond what git
// reports, no LLM is invoked.
//
// Steps:
//
//  1. Read current wiki.yml (if missing, recover from wiki/ as
//     in Sync's wiki-only branch — handled by the caller).
//  2. Discover current source-tree modules.
//  3. Reconcile: for each existing yml entry:
//     - if removed (in yml, not in source) → action=delete
//     - if path matches a discovered module AND LastSHA
//     differs from current HEAD content → action=regenerate,
//       files_changed = git diff <last_sha>..HEAD -- <path>
//     - if path is in yml AND in source AND no diff → skip
//  4. For each discovered module NOT in yml → action=new
//  5. Write wiki.yml.pending with all of the above.
//
// Each pending entry starts at status=pending. Apply moves
// it through in_progress → done / failed.
//
// module entries in yml are refreshed (reconciled) as a side
// effect — last_sha remains null until Apply actually writes.
func Plan(cwd string, yml *wikiYml, git GitRunner) error {
	modules, err := discoverModules(cwd)
	if err != nil {
		return fmt.Errorf("plan: discover: %w", err)
	}

	// Index existing entries by path for the reconcile loop.
	existingByPath := make(map[string]moduleYml, len(yml.Modules))
	for _, m := range yml.Modules {
		existingByPath[m.Path] = m
	}
	currentByPath := make(map[string]moduleEntry, len(modules))
	for _, c := range modules {
		currentByPath[c.Path] = c
	}

	var pending []pendingEntry
	seen := make(map[string]bool)

	// 1) Discovered modules — check for diff against last_sha.
	for _, c := range modules {
		seen[c.Path] = true
		existing, had := existingByPath[c.Path]
		_ = existing

		if !had || existing.LastSHA == nil {
			// New module, or no recorded SHA → needs regen.
			pending = append(pending, pendingEntry{
				Path:   c.Path,
				Action: pendingActionRegenerate,
				Reason: "no last_sha recorded",
				Status: pendingStatusPending,
			})
			continue
		}

		// Compute the diff. git may fail to resolve last_sha
		// (force-push, history rewrite) — treat as "full
		// regen, can't list specifics".
		files := []string(nil)
		if git != nil {
			files, _ = git.ChangedFiles(cwd, *existing.LastSHA, c.Path)
		}
		if len(files) == 0 {
			// No specific files changed in this module's
			// path since the last write. Either nothing
			// changed, or git couldn't enumerate (force-push,
			// etc.). Both → skip; user can force-regen by
			// removing the entry from wiki.yml.
			continue
		}
		reason := fmt.Sprintf("source changed since %s", shortSHA(*existing.LastSHA))
		pending = append(pending, pendingEntry{
			Path:         c.Path,
			Action:       pendingActionRegenerate,
			Reason:       reason,
			FilesChanged: files,
			Status:       pendingStatusPending,
		})
	}

	// 2) Existing-but-not-in-source → delete.
	var removedPaths []string
	for path := range existingByPath {
		if _, ok := currentByPath[path]; !ok {
			removedPaths = append(removedPaths, path)
		}
	}
	for _, p := range removedPaths {
		existing := existingByPath[p]
		pending = append(pending, pendingEntry{
			Path:   p,
			Action: pendingActionDelete,
			Reason: "directory no longer exists in source",
			Status: pendingStatusPending,
		})
		_ = existing
	}

	yml.Pending = pending
	return nil
}

// shortSHA returns the first 7 chars of a SHA (git's default
// short format). Empty string in → empty string out.
func shortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// pendingOrder returns the indices into pending in the order
// Apply should process them. The order is:
//
//  1. Deepest source-path entries first (path segment count
//     desc), so sub-packages complete before their parents
//  2. Delete actions last (they're quick and don't depend
//     on anything else)
//  3. Aggregate paths (architecture.md, glossary.md) handled
//     outside this slice by Apply itself
//
// Stable sort preserves Plan's discovery order within each
// depth bucket.
func pendingOrder(pending []pendingEntry) []int {
	deletes := []int{}
	rest := []int{}
	for i, p := range pending {
		if p.Action == pendingActionDelete {
			deletes = append(deletes, i)
			continue
		}
		rest = append(rest, i)
	}
	// Sort rest by depth desc, stable.
	for i := 1; i < len(rest); i++ {
		for j := i; j > 0 && pathDepth(pending[rest[j]].Path) > pathDepth(pending[rest[j-1]].Path); j-- {
			rest[j], rest[j-1] = rest[j-1], rest[j]
		}
	}
	return append(rest, deletes...)
}

// pathDepth counts the number of slash-separated segments in
// p. "internal/foo/bar" → 3, "configs" → 1.
func pathDepth(p string) int {
	if p == "" {
		return 0
	}
	return strings.Count(filepath.ToSlash(p), "/") + 1
}