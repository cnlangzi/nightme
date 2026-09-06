package wiki

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// GitRunner is the minimal git surface Plan needs.
//
// Production implementation (ExecGitRunner) shells out to the
// git binary; tests inject a fake. /wiki reads only committed
// history — uncommitted changes are invisible by design
// (Handle preflights IsClean).
type GitRunner interface {
	Head(repoRoot string) (string, error)
	ChangedFiles(repoRoot, from, pathFilter string) ([]string, error)
	IsClean(repoRoot string) (bool, error)
	RepoRoot(cwd string) (string, error)
	Status(repoRoot string) ([]byte, error)
}

// ExecGitRunner shells out to the git binary. Production
// default — no extra deps beyond `git` itself.
type ExecGitRunner struct{}

// Head returns the current git HEAD SHA of repoRoot.
func (ExecGitRunner) Head(repoRoot string) (string, error) {
	return gitOutput(repoRoot, "rev-parse", "HEAD")
}

// RepoRoot returns the toplevel of the repository containing
// cwd. Used by Handle to normalise a ChatSession CWD that
// lives inside a worktree.
func (ExecGitRunner) RepoRoot(cwd string) (string, error) {
	out, err := gitOutputAllowFailure(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repo (or git unavailable): %w", err)
	}
	if out == "" {
		return "", errors.New("not a git repo (or git unavailable): empty toplevel")
	}
	return out, nil
}

// ChangedFiles returns the list of files changed between `from`
// (exclusive) and HEAD (inclusive), restricted to paths under
// `pathFilter` (empty = no filter).
//
// pathFilter is treated as a single literal path (no recursion
// suffix). This is the §10.1 anti-ancestor key — passing
// "internal/bridge" must NOT match "internal/bridge/claudecode".
//
// Returns (nil, nil) when `from` is empty or does not resolve;
// Plan treats this as "everything changed" and falls back to
// full-module regeneration.
func (ExecGitRunner) ChangedFiles(repoRoot, from, pathFilter string) ([]string, error) {
	if from == "" {
		return nil, nil
	}
	args := []string{"diff", "--name-only", from + "..HEAD"}
	if pathFilter != "" {
		args = append(args, "--", pathFilter)
	}
	out, err := gitOutputAllowFailure(repoRoot, args...)
	if err != nil {
		// Force-push / history rewrite → unreachable SHA.
		// Treat as "no specific file list" per contract.
		return nil, nil
	}
	if out == "" {
		return nil, nil
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// IsClean reports whether repoRoot's working tree has no
// uncommitted modifications.
func (ExecGitRunner) IsClean(repoRoot string) (bool, error) {
	out, err := gitOutputAllowFailure(repoRoot, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// Status returns the raw `git status --porcelain` output,
// used by CheckOutputBoundary (§11.5).
func (ExecGitRunner) Status(repoRoot string) ([]byte, error) {
	out, err := gitOutputBytesAllowFailure(repoRoot, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- shared git plumbing ---

func gitOutput(repoRoot string, args ...string) (string, error) {
	out, err := gitOutputBytesAllowFailure(repoRoot, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitOutputAllowFailure(repoRoot string, args ...string) (string, error) {
	out, err := gitOutputBytesAllowFailure(repoRoot, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitOutputBytesAllowFailure shells out to git and returns the
// raw stdout bytes. Exits with non-zero are surfaced (with
// stderr appended) for commands whose failure is meaningful;
// for diff-style commands the caller treats non-zero as
// "unreachable SHA → empty list".
func gitOutputBytesAllowFailure(repoRoot string, args ...string) ([]byte, error) {
	full := append([]string{"-C", repoRoot}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// Plan: §10 deterministic reconciliation.

// Plan produces the incremental update list. Steps (§10):
//
//  1. Discover source modules (mirror rule: every non-empty
//     directory is a module; uses discover.go).
//  2. Reconcile discovered modules with yml.Modules: new →
//     append; removed → mark removed:true.
//  3. For each live module with a last_sha, compute direct
//     child file changes since last_sha (§10.1). Any change
//     OR prompt_version mismatch OR missing page → action
//     "regenerate".
//  4. Modules present in yml but absent from source →
//     action "delete".
//  5. Merge retryable (in_progress / failed) entries per
//     (path, action) — preserve failure context (§10.2).
//  6. Mark aggregates.architecture.dirty when any module
//     action exists.
//  7. Mark aggregates.quickstart.dirty when its tracked set
//     changed OR prompt version changed.
//  8. yml.PlanSha = head; atomicWrite yml.
//
// "Direct file changes" uses git diff --name-only
// last_sha..HEAD -- <module-path>. The lack of a recursive
// suffix (no trailing slash) is what prevents a leaf change
// from regenerating every ancestor page.
func Plan(repoRoot, head string, yml *wikiYml, git GitRunner) error {
	modules, err := discoverModules(repoRoot)
	if err != nil {
		return fmt.Errorf("plan: discover: %w", err)
	}

	currentByPath := make(map[string]moduleEntry, len(modules))
	for _, m := range modules {
		currentByPath[m.Path] = m
	}
	existingByPath := make(map[string]moduleYml, len(yml.Modules))
	for _, m := range yml.Modules {
		existingByPath[m.Path] = m
	}

	// Reconcile modules[] in place: refresh File from current,
	// clear Removed when the path is back in source, mark
	// removed when it's not, append brand-new paths.
	merged := mergeModules(yml.Modules, modules)
	yml.Modules = merged

	// Build new pending[] deterministically.
	pending := make([]pendingEntry, 0, len(merged))

	// (3) Per-module change detection.
	for _, m := range yml.Modules {
		if m.Removed {
			// (4) Removed modules: action=delete only when
			// the source path is absent.
			if _, ok := currentByPath[m.Path]; !ok {
				pending = append(pending, pendingEntry{
					Path:   m.Path,
					Action: pendingActionDelete,
					Reason: "directory no longer exists in source",
					Status: pendingStatusPending,
				})
			}
			continue
		}

		// Live module. Need regeneration if:
		//   - no last_sha
		//   - prompt version mismatch
		//   - directly owned files changed since last_sha
		//   - page file is missing (try to skip; §10 step 7:
		//     "absent or structurally invalid" → regenerate)
		needs, reason, filesChanged := moduleNeedsRegen(repoRoot, m, currentByPath[m.Path], git)
		if !needs {
			continue
		}
		pending = append(pending, pendingEntry{
			Path:         m.Path,
			Action:       pendingActionRegenerate,
			Reason:       reason,
			FilesChanged: filesChanged,
			Status:       pendingStatusPending,
		})
	}

	// (5) Merge retryable entries: keep error/status when
	// (path, action) collides with the freshly-computed list.
	pending = mergeRetryable(pending, yml.Pending)
	yml.Pending = pending

	// (6) Architecture dirty when any module action exists OR
	// when aggregates itself is missing / has no last_sha.
	arch := ensureAggregate(yml, ArchitectureKey)
	arch.Dirty = len(filterLivePending(pending)) > 0 ||
		arch.LastSHA == nil ||
		arch.PromptVer != currentPromptVerFor(ArchitectureKey) ||
		!pageExists(repoRoot, architectureFile(yml, ArchitectureKey))

	// (7) Quickstart dirty when its tracked set or prompt
	// version changed, or its page is missing.
	qst := ensureAggregate(yml, QuickstartKey)
	qst.Dirty = quickstartDirty(repoRoot, qst, git) ||
		qst.PromptVer != currentPromptVerFor(QuickstartKey) ||
		!pageExists(repoRoot, architectureFile(yml, QuickstartKey))

	// (8) Persist.
	yml.PlanSha = head
	return writeWikiYml(repoRoot, yml)
}

// mergeModules reconciles yml.Modules with the freshly
// discovered set. New paths are appended; removed paths are
// marked removed:true (and remain in the slice so the
// delete path can find them).
func mergeModules(existing []moduleYml, current []moduleEntry) []moduleYml {
	currentByPath := make(map[string]moduleEntry, len(current))
	for _, c := range current {
		currentByPath[c.Path] = c
	}
	existingByPath := make(map[string]moduleYml, len(existing))
	for _, e := range existing {
		existingByPath[e.Path] = e
	}

	var out []moduleYml
	for _, e := range existing {
		if cur, ok := currentByPath[e.Path]; ok {
			e.Removed = false
			e.File = cur.File
			out = append(out, e)
		} else {
			e.Removed = true
			out = append(out, e)
		}
	}
	for _, c := range current {
		if _, ok := existingByPath[c.Path]; ok {
			continue
		}
		out = append(out, moduleYml{
			Path:      c.Path,
			File:      c.File,
			PromptVer: ModulePromptVersion,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Removed != out[j].Removed {
			return !out[i].Removed
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// moduleNeedsRegen reports whether the given module needs
// regeneration and returns the reason + changed files list
// (empty when the module has no last_sha).
func moduleNeedsRegen(repoRoot string, m moduleYml, _ moduleEntry, git GitRunner) (bool, string, []string) {
	if m.LastSHA == nil || *m.LastSHA == "" {
		return true, "no last_sha recorded", nil
	}
	if m.PromptVer != ModulePromptVersion {
		return true, fmt.Sprintf("prompt_version %d ≠ %d", m.PromptVer, ModulePromptVersion), nil
	}
	pagePath := filepath.Join(repoRoot, "wiki", "modules", m.File)
	if _, err := os.Stat(pagePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, "page missing on disk", nil
		}
		return true, fmt.Sprintf("stat page: %v", err), nil
	}
	files, _ := git.ChangedFiles(repoRoot, *m.LastSHA, m.Path)
	if len(files) == 0 {
		// No specific files — either unchanged or git can't
		// enumerate (force-push / history rewrite). Both
		// mean "no regeneration needed from Plan's view";
		// the user can force-regen by removing the entry
		// from wiki.yml.
		return false, "", nil
	}
	return true, fmt.Sprintf("source changed since %s", shortSHA(*m.LastSHA)), files
}

// shortSHA returns the first 7 chars of sha. Empty → empty.
func shortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// mergeRetryable folds in retryable pending entries from the
// previous yml.Pending, keyed by (path, action). When the
// freshly-computed Plan produces the same (path, action) the
// existing error/status is preserved (§10.2).
func mergeRetryable(fresh []pendingEntry, prev []pendingEntry) []pendingEntry {
	key := func(p pendingEntry) string { return p.Path + "|" + p.Action }
	freshByKey := make(map[string]int, len(fresh))
	for i, e := range fresh {
		freshByKey[key(e)] = i
	}
	for _, p := range prev {
		if p.Status != pendingStatusInProgress && p.Status != pendingStatusFailed {
			continue
		}
		k := key(p)
		if i, ok := freshByKey[k]; ok {
			// Same action already requested — preserve the
			// error message and status, keep the freshly
			// computed reason + files_changed.
			fresh[i].Error = p.Error
			fresh[i].BeforeHash = p.BeforeHash
			if p.Status == pendingStatusInProgress {
				fresh[i].Status = pendingStatusInProgress
			} else {
				fresh[i].Status = pendingStatusFailed
			}
			continue
		}
		// Stale retryable that no longer matches the new
		// Plan. Per §10.2 "A conflicting action for the same
		// path is discarded, such as regenerate becoming
		// delete" — keep entries that are still consistent
		// (delete→regenerate on a re-introduced module).
		if _, stillThere := freshByKey[p.Path+"|"+pendingActionDelete]; stillThere && p.Action == pendingActionRegenerate {
			continue
		}
		if _, stillThere := freshByKey[p.Path+"|"+pendingActionRegenerate]; stillThere && p.Action == pendingActionDelete {
			continue
		}
		fresh = append(fresh, p)
	}
	return fresh
}

func filterLivePending(pending []pendingEntry) []pendingEntry {
	var out []pendingEntry
	for _, p := range pending {
		if p.Action == pendingActionNew || p.Action == pendingActionRegenerate {
			out = append(out, p)
		}
	}
	return out
}

// quickstartDirty checks whether the Quickstart tracked set
// (AGENTS.md, Makefile, go.mod, .github/, etc.) has changed
// since its last_sha.
func quickstartDirty(repoRoot string, qst *aggregateYml, git GitRunner) bool {
	if qst.LastSHA == nil {
		return true
	}
	// Cheap version: AGENTS.md presence + any tracked file
	// under known config roots. Plan will not over-regenerate
	// because quickstart prompt_version is also checked.
	files, _ := git.ChangedFiles(repoRoot, *qst.LastSHA, "AGENTS.md")
	if len(files) > 0 {
		return true
	}
	for _, p := range []string{"Makefile", "go.mod", "go.sum", ".github"} {
		f, _ := git.ChangedFiles(repoRoot, *qst.LastSHA, p)
		if len(f) > 0 {
			return true
		}
	}
	return false
}

// pageExists is a small wrapper used by Plan's aggregates.
func pageExists(repoRoot, rel string) bool {
	if rel == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(repoRoot, "wiki", rel))
	return err == nil
}

// architectureFile returns the wiki-relative path for an
// aggregate. Defaults to "<name>.md" when the aggregate is
// missing the field.
func architectureFile(yml *wikiYml, name string) string {
	if yml == nil {
		return name + ".md"
	}
	if a, ok := yml.Aggregates[name]; ok && a != nil && a.File != "" {
		return a.File
	}
	return name + ".md"
}

// writeWikiYml serialises y and writes atomically.
func writeWikiYml(repoRoot string, yml *wikiYml) error {
	encoded, err := encodeWikiYml(yml)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(repoRoot, "wiki.yml"), string(encoded))
}
