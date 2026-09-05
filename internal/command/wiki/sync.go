package wiki

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Sync is the single entry point for `/wiki`. It is idempotent:
// handles both first-time scaffold AND reconcile-on-existing.
//
// State machine:
//
//	wiki.yml absent, wiki/ absent   → fresh scaffold (calls Scaffold)
//	wiki.yml present, wiki/ present → reconcile against source tree
//	wiki.yml XOR wiki/              → refuse with ErrWikiHalfState
//	                                  (user must delete the stale half
//	                                  before retrying — silent recovery
//	                                  risks losing either the metadata
//	                                  or the content)
//
// Reconcile algorithm (see reconcileModules for details):
//
//   - path in yml AND in source  → keep entry, un-mark removed
//   - path in yml, NOT in source  → mark removed:true (file preserved)
//   - path NOT in yml, in source  → append new entry
//
// Content regeneration:
//
//   - last_sha is null (no LLM has written here) → write stub
//   - last_sha is non-null (LLM wrote content)   → preserve as-is
//
// The last_sha check is forward-compatible: today stub mode
// leaves last_sha null, so every sync overwrites every stub.
// When the LLM path lands and sets last_sha on commit, sync
// will naturally stop overwriting content the LLM produced.
//
// agent is plumbed through for the future LLM-driven path.
// Empty means "stub mode" (current behaviour).
func Sync(cwd, agent string) (SyncResult, error) {
	ymlPath := filepath.Join(cwd, "wiki.yml")
	wikiDir := filepath.Join(cwd, "wiki")

	ymlExists := fileExists(ymlPath)
	wikiExists := dirExists(wikiDir)

	if ymlExists != wikiExists {
		return SyncResult{}, fmt.Errorf(
			"%w: wiki.yml exists=%v, wiki/ exists=%v (delete one before retrying)",
			ErrWikiHalfState, ymlExists, wikiExists)
	}

	var result SyncResult

	if !ymlExists {
		// Fresh scaffold: write core files + per-module
		// skeletons + a fresh wiki.yml with the discovered
		// modules pre-populated.
		if _, err := Scaffold(cwd); err != nil {
			return result, fmt.Errorf("scaffold: %w", err)
		}
		result.Fresh = true
	}

	// Read yml (now guaranteed to exist).
	ymlData, err := os.ReadFile(ymlPath)
	if err != nil {
		return result, fmt.Errorf("read wiki.yml: %w", err)
	}
	yml, err := parseWikiYml(ymlData)
	if err != nil {
		return result, fmt.Errorf("parse wiki.yml: %w", err)
	}

	// Discover current source-tree modules.
	currentModules, err := discoverModules(cwd)
	if err != nil {
		return result, fmt.Errorf("discover: %w", err)
	}

	// Track each path's prior state (live vs removed vs absent)
	// so the reply can name only the genuinely-changed entries.
	// Modules that were live AND are still live are not
	// reported — that would spam chat on every re-run.
	wasLive := make(map[string]bool)
	wasRemoved := make(map[string]bool)
	for _, m := range yml.Modules {
		if m.Removed {
			wasRemoved[m.Path] = true
		} else {
			wasLive[m.Path] = true
		}
	}

	yml.Modules = reconcileModules(yml.Modules, currentModules)

	for _, m := range yml.Modules {
		switch {
		case m.Removed && !wasRemoved[m.Path]:
			// Newly removed (was live before, or wasn't in yml).
			result.Removed = append(result.Removed, m.Path)
		case !m.Removed && !wasLive[m.Path]:
			// Newly added (was removed before, or wasn't in yml).
			result.Added = append(result.Added, m.Path)
		}
	}

	// Rewrite wiki.yml.
	encoded, err := encodeWikiYml(yml)
	if err != nil {
		return result, err
	}
	if err := atomicWrite(ymlPath, string(encoded)); err != nil {
		return result, fmt.Errorf("write wiki.yml: %w", err)
	}

	// Rewrite llms.txt with the live module list. Removed
	// modules are intentionally omitted — they no longer
	// exist in source so external readers shouldn't see them
	// as live links.
	llmsContent := llmsTxtSkeletonWithModules(projectName(cwd), currentModules)
	if err := atomicWrite(filepath.Join(wikiDir, "llms.txt"), llmsContent); err != nil {
		return result, fmt.Errorf("write llms.txt: %w", err)
	}

	// Regenerate content per module.
	for _, m := range yml.Modules {
		if m.Removed {
			continue // wiki file preserved as-is, marked removed in yml
		}
		if m.LastSHA != nil {
			// LLM already wrote content here; preserve.
			result.Preserved = append(result.Preserved, m.File)
			continue
		}
		sources, rerr := readSourceFiles(cwd, m.Path)
		if rerr != nil {
			continue
		}
		content := moduleDocStub(m.Path, sources)
		out := filepath.Join(wikiDir, "modules", m.File)
		if werr := atomicWrite(out, content); werr != nil {
			continue
		}
		result.Written = append(result.Written,
			filepath.ToSlash(filepath.Join("wiki", "modules", m.File)))
	}

	return result, nil
}

// SyncResult is the structured outcome of a /wiki run.
// Reply formatter reads these slices; nothing else should.
type SyncResult struct {
	Fresh     bool     // true when /wiki created the wiki from scratch
	Added     []string // paths newly added to yml.modules[]
	Removed   []string // paths marked removed:true (file preserved)
	Written   []string // wiki files written this run (stub mode)
	Preserved []string // wiki files kept untouched (had LLM content)
}

// ErrWikiHalfState signals that wiki.yml AND wiki/ disagree
// on existence — see Sync for the policy.
var ErrWikiHalfState = errors.New("wiki half-state")

// reconcileModules produces the new modules[] from existing
// + currently-discovered. See Sync's docstring for the rules.
//
// Output is sorted: live (non-removed) modules first in path
// order, then removed modules in path order. Stable layout
// keeps wiki.yml diffs minimal across runs.
func reconcileModules(existing []moduleYml, current []moduleEntry) []moduleYml {
	existingByPath := make(map[string]moduleYml, len(existing))
	for _, e := range existing {
		existingByPath[e.Path] = e
	}
	currentByPath := make(map[string]moduleEntry, len(current))
	for _, c := range current {
		currentByPath[c.Path] = c
	}

	sortedCurrent := make([]moduleEntry, len(current))
	copy(sortedCurrent, current)
	sort.Slice(sortedCurrent, func(i, j int) bool {
		return sortedCurrent[i].Path < sortedCurrent[j].Path
	})

	var out []moduleYml

	// Live modules first.
	for _, c := range sortedCurrent {
		if e, ok := existingByPath[c.Path]; ok {
			e.Removed = false // un-mark if previously removed
			e.File = c.File   // refresh filename in case it changed
			out = append(out, e)
		} else {
			out = append(out, moduleYml{
				Path: c.Path,
				File: c.File,
			})
		}
	}

	// Then removed (in yml, not in source).
	var removedPaths []string
	for path := range existingByPath {
		if _, ok := currentByPath[path]; !ok {
			removedPaths = append(removedPaths, path)
		}
	}
	sort.Strings(removedPaths)
	for _, p := range removedPaths {
		e := existingByPath[p]
		e.Removed = true
		out = append(out, e)
	}

	return out
}

// fileExists reports whether path is an existing regular
// file. Symlinks are followed (os.Stat's default).
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// --- source file scanning (used by regenerate step) ---

// sourceFile is one source file inside a module, scanned for
// the stub generator's File Layout table and the <!-- sources -->
// footer. Path is relative to cwd; Name is just the basename.
type sourceFile struct {
	Name    string
	RelPath string
	Lines   int
}

// readSourceFiles enumerates a module's directory and returns
// metadata for each non-hidden, non-test source file. We skip
// the file CONTENT itself in stub mode — only name + line
// count feed the stub template. The future LLM-driven path
// will read each file's full content into the agent prompt.
//
// Skip rules mirror discover.go's module-detection rules so
// the stub's sources footer matches the init-time module
// roster:
//   - directories (recursion is the walker's job, not ours)
//   - names starting with "." (hidden / OS junk)
//   - files ending with "_test.go"
//   - files larger than maxSourceBytes (defends against
//     accidentally-vendored blobs)
//   - files we cannot stat or read (silently skipped)
//
// Returned slice is sorted by Name for stable diffs.
const maxSourceBytes = 100 * 1024 // 100 KiB

func readSourceFiles(cwd, modulePath string) ([]sourceFile, error) {
	dir := filepath.Join(cwd, modulePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var sources []sourceFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > maxSourceBytes {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sources = append(sources, sourceFile{
			Name:    name,
			RelPath: filepath.ToSlash(filepath.Join(modulePath, name)),
			Lines:   bytesCountLines(data),
		})
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Name < sources[j].Name
	})
	return sources, nil
}

// bytesCountLines counts '\n' bytes — a good-enough "lines of
// code" metric for the stub table. Files without a trailing
// newline report one fewer than wc -l; acceptable.
func bytesCountLines(data []byte) int {
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

// runWiki is the slash-command entry point. Wraps Sync with
// argv parsing and reply formatting.
func (f *Factory) runWiki(ctx context.Context, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	cwd, fail := command.RequireActiveCwd(cs)
	if fail != nil {
		return fail, nil
	}

	args, err := parseWikiArgs(input.Args[1:])
	if err != nil {
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ %v", err),
			Consumed: true,
		}, nil
	}

	result, err := Sync(cwd, args.Agent)
	if err != nil {
		if errors.Is(err, ErrWikiHalfState) {
			return &command.SlashOutput{
				Reply:    "❌ " + err.Error(),
				Consumed: true,
			}, nil
		}
		return &command.SlashOutput{
			Reply:    fmt.Sprintf("❌ /wiki failed: %v", err),
			Consumed: true,
		}, nil
	}

	return &command.SlashOutput{
		Reply:    formatSyncReply(result),
		Consumed: true,
	}, nil
}

// formatSyncReply renders the success card.
//
// Per project convention, keep the post-summary lean — group
// counts, then up to N (e.g. 20) bullets. A repo with 100
// modules shouldn't dump 100 lines into chat; it should say
// "Wrote 100 stubs." and let the user git diff to see details.
func formatSyncReply(r SyncResult) string {
	var b strings.Builder
	if r.Fresh {
		b.WriteString("✅ /wiki (fresh)\n\n")
	} else {
		b.WriteString("✅ /wiki\n\n")
	}

	if len(r.Added) > 0 {
		fmt.Fprintf(&b, "Added %d module(s):\n", len(r.Added))
		for _, p := range r.Added {
			b.WriteString("• ")
			b.WriteString(p)
			b.WriteByte('\n')
		}
		b.WriteString("\n")
	}

	if len(r.Removed) > 0 {
		fmt.Fprintf(&b, "Marked %d removed (wiki files preserved):\n", len(r.Removed))
		for _, p := range r.Removed {
			b.WriteString("• ")
			b.WriteString(p)
			b.WriteByte('\n')
		}
		b.WriteString("\n")
	}

	if len(r.Written) > 0 {
		fmt.Fprintf(&b, "Wrote %d stub(s).\n", len(r.Written))
	}

	if len(r.Preserved) > 0 {
		fmt.Fprintf(&b, "Preserved %d module file(s) (LLM content kept).\n", len(r.Preserved))
	}

	if r.Fresh {
		b.WriteString("\nCreated wiki/ + wiki.yml from scratch.")
	} else if len(r.Added) == 0 && len(r.Removed) == 0 && len(r.Written) == 0 && len(r.Preserved) == 0 {
		b.WriteString("\nNo changes — wiki already in sync with source tree.")
	}

	return b.String()
}
