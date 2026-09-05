package wiki

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LLMDispatcher is the hook Apply uses to fill per-module
// content. v0 ships a stub (writes the standard template).
// The real implementation (future) dispatches via
// agent.Builtins.RunOnce with the per-module prompt.
type LLMDispatcher interface {
	Generate(cwd, pkgPath string, filesChanged []string) (content string, err error)
}

// stubDispatcher writes moduleDocStub for each module. No
// LLM call. /wiki without `-a` uses this.
type stubDispatcher struct{}

func (stubDispatcher) Generate(cwd, pkgPath string, filesChanged []string) (string, error) {
	sources, err := readSourceFiles(cwd, pkgPath)
	if err != nil {
		return "", fmt.Errorf("read sources: %w", err)
	}
	return moduleDocStub(pkgPath, sources), nil
}

// ApplyResult is the structured outcome of the Apply phase.
// Reply formatter reads these slices.
type ApplyResult struct {
	Done    []string // paths successfully processed
	Failed  []string // paths where generation errored (error in yml.Pending[].Error)
	Skipped []string // paths already done (resumed mid-run)
}

// Apply consumes wikiYml.Pending and writes content for each
// entry in bottom-up order: deepest source path first
// (sub-packages complete before their parents), then
// aggregate pages (architecture.md, glossary.md), then
// deletes.
//
// Each entry moves pending → in_progress → done | failed.
// moduleYml.LastSHA is set on done. Delete entries unlink
// the wiki file from disk; the yml entry stays marked
// removed:true for audit.
//
// wiki.yml is rewritten after every entry so a crash
// mid-Apply leaves a coherent, resumable state on disk.
// Done entries are cleared from pending at the end so the
// file doesn't grow forever.
func Apply(cwd string, yml *wikiYml, llm LLMDispatcher, git GitRunner) (ApplyResult, error) {
	var res ApplyResult
	if len(yml.Pending) == 0 {
		return res, nil
	}

	head := ""
	if git != nil {
		if h, err := git.Head(cwd); err == nil {
			head = h
		}
	}

	order := pendingOrder(yml.Pending)
	for _, idx := range order {
		entry := yml.Pending[idx]
		if entry.Status == pendingStatusDone {
			res.Skipped = append(res.Skipped, entry.Path)
			continue
		}
		entry.Status = pendingStatusInProgress
		yml.Pending[idx] = entry
		if err := writeWikiYmlFile(cwd, yml); err != nil {
			return res, err
		}

		switch entry.Action {
		case pendingActionRegenerate, pendingActionNew:
			content, err := llm.Generate(cwd, entry.Path, entry.FilesChanged)
			if err != nil {
				entry = failEntry(entry, err)
				yml.Pending[idx] = entry
				_ = writeWikiYmlFile(cwd, yml)
				res.Failed = append(res.Failed, entry.Path)
				continue
			}
			file := wikiFileForPath(yml, entry.Path)
			out := filepath.Join(cwd, "wiki", "modules", file)
			if werr := atomicWrite(out, content); werr != nil {
				entry = failEntry(entry, werr)
				yml.Pending[idx] = entry
				_ = writeWikiYmlFile(cwd, yml)
				res.Failed = append(res.Failed, entry.Path)
				continue
			}
			updateModuleAfterApply(yml, entry.Path, headOrUnknown(head))
			entry.Status = pendingStatusDone
			entry.Error = ""
			yml.Pending[idx] = entry

		case pendingActionDelete:
			file := wikiFileForPath(yml, entry.Path)
			out := filepath.Join(cwd, "wiki", "modules", file)
			if rerr := os.Remove(out); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				entry = failEntry(entry, rerr)
				yml.Pending[idx] = entry
				_ = writeWikiYmlFile(cwd, yml)
				res.Failed = append(res.Failed, entry.Path)
				continue
			}
			for i := range yml.Modules {
				if yml.Modules[i].Path == entry.Path {
					yml.Modules[i].Removed = true
					break
				}
			}
			entry.Status = pendingStatusDone
			entry.Error = ""
			yml.Pending[idx] = entry
		}

		if err := writeWikiYmlFile(cwd, yml); err != nil {
			return res, err
		}
		res.Done = append(res.Done, entry.Path)
	}

	// After per-module entries, refresh llms.txt from the
	// (possibly changed) live module list.
	if len(res.Done) > 0 || len(res.Failed) > 0 {
		llmsContent := llmsTxtSkeletonWithModules(projectName(cwd), liveModuleEntries(yml))
		llmsOut := filepath.Join(cwd, "wiki", "llms.txt")
		if err := atomicWrite(llmsOut, llmsContent); err != nil {
			return res, fmt.Errorf("write llms.txt: %w", err)
		}
	}

	// Clean done items from pending so the file doesn't grow
	// forever. Failed and in-progress items remain for retry.
	cleanPending(yml)
	_ = writeWikiYmlFile(cwd, yml)

	return res, nil
}

// failEntry stamps an entry as failed with the given error.
func failEntry(e pendingEntry, err error) pendingEntry {
	e.Status = pendingStatusFailed
	e.Error = err.Error()
	return e
}

// writeWikiYmlFile serialises yml and writes it to disk.
// Atomic write (via the existing helper) — partial writes
// can never corrupt the file mid-Apply.
func writeWikiYmlFile(cwd string, yml *wikiYml) error {
	encoded, err := encodeWikiYml(yml)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(cwd, "wiki.yml"), string(encoded))
}

// updateModuleAfterApply sets LastSHA on the module entry
// matching path. If no matching entry exists (action=new
// before reconcile), the entry is appended with the mirror
// file path. Removed flag is unconditionally cleared so a
// re-introduced module doesn't carry the stale marker.
func updateModuleAfterApply(yml *wikiYml, path, sha string) {
	for i := range yml.Modules {
		if yml.Modules[i].Path == path {
			s := sha
			yml.Modules[i].LastSHA = &s
			yml.Modules[i].Removed = false
			return
		}
	}
	yml.Modules = append(yml.Modules, moduleYml{
		Path:    path,
		File:    filepath.ToSlash(path) + ".md",
		LastSHA: &sha,
	})
}

// headOrUnknown returns head if non-empty, else "unknown"
// (a sentinel used when git is unavailable; the sha field
// still being non-null is what gates future "no diff" logic).
func headOrUnknown(head string) string {
	if head == "" {
		return "unknown"
	}
	return head
}

// wikiFileForPath returns the yml-recorded file name for
// path, falling back to the mirror rule if the module isn't
// in yml yet (e.g. action=new before reconcile).
func wikiFileForPath(yml *wikiYml, path string) string {
	for _, m := range yml.Modules {
		if m.Path == path {
			return m.File
		}
	}
	return filepath.ToSlash(path) + ".md"
}

// liveModuleEntries returns the live (non-removed) module
// entries for llms.txt. We synthesise moduleEntry from yml's
// moduleYml so llms.txt generation is isolated from
// discovery.
func liveModuleEntries(yml *wikiYml) []moduleEntry {
	var out []moduleEntry
	for _, m := range yml.Modules {
		if m.Removed {
			continue
		}
		file := m.File
		if file == "" {
			file = filepath.ToSlash(m.Path) + ".md"
		}
		out = append(out, moduleEntry{
			Path:        m.Path,
			File:        file,
			WikiRelPath: filepath.ToSlash(filepath.Join("modules", file)),
		})
	}
	return out
}

// cleanPending drops done entries. Failed and in-progress
// entries remain for visibility + retry on next /wiki.
func cleanPending(yml *wikiYml) {
	var remaining []pendingEntry
	for _, p := range yml.Pending {
		if p.Status != pendingStatusDone {
			remaining = append(remaining, p)
		}
	}
	yml.Pending = remaining
	if len(yml.Pending) == 0 {
		yml.Pending = nil
	}
}