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

// Sync is the single entry point for `/wiki`. It runs in two
// phases every invocation:
//
//	Phase 1 — Plan:  scan source, compute pending list, write
//	          wiki.yml.pending. Pure mechanical (git diff).
//	          Free.
//	Phase 2 — Apply: consume wiki.yml.pending in bottom-up
//	          order, write content. Stub mode writes the
//	          placeholder template; LLM mode (with
//	          `-a <agent>`) dispatches the agent — stub for
//	          now, real wiring when the agent call lands.
//
// Both phases run on every invocation; the cost differs. The
// plan is persistent (wiki.yml.pending survives) so resuming
// after a crash picks up where the previous run stopped.
//
// State machine:
//
//	wiki.yml absent, wiki/ absent   → fresh scaffold (calls Scaffold), then Plan + Apply
//	wiki.yml present, wiki/ present → Plan + Apply on the existing state
//	wiki.yml present, wiki/ absent   → recover — trust yml, recreate wiki/
//	wiki.yml absent, wiki/ present   → recover — reconstruct yml from wiki/
//
// Git requirement: /wiki reads committed history (SHAs and
// file diffs). Local uncommitted changes are invisible by
// design — Sync refuses with a clear error if the working
// tree is dirty. Users commit first, then run /wiki.
//
// agent is plumbed for the future LLM-driven path. v0 always
// uses the stub dispatcher; -a is accepted but ignored.
func Sync(cwd, agent string) (SyncResult, error) {
	return SyncWith(cwd, agent, ExecGitRunner{}, stubDispatcher{})
}

// SyncWith is the dependency-injected form of Sync. Tests use
// it to wire mock git / dispatcher implementations.
func SyncWith(cwd, agent string, git GitRunner, llm LLMDispatcher) (SyncResult, error) {
	_ = agent // reserved for LLM dispatcher selection; v0 ignores

	// Git pre-flight: the whole incremental mechanism depends
	// on committed SHAs and git diff. Refuse early with a
	// clear message when either precondition fails.
	head, err := git.Head(cwd)
	if err != nil {
		return SyncResult{}, fmt.Errorf("not a git repo (or git unavailable): %w", err)
	}
	clean, err := git.IsClean(cwd)
	if err != nil {
		return SyncResult{}, fmt.Errorf("git status failed: %w", err)
	}
	if !clean {
		return SyncResult{}, errors.New("working tree has uncommitted changes; commit first (wiki reads committed history, not working tree)")
	}
	_ = head // captured by Plan via subsequent git calls

	ymlPath := filepath.Join(cwd, "wiki.yml")
	wikiDir := filepath.Join(cwd, "wiki")

	ymlExists := fileExists(ymlPath)
	wikiExists := dirExists(wikiDir)

	var result SyncResult

	switch {
	case !ymlExists && !wikiExists:
		if _, err := Scaffold(cwd); err != nil {
			return result, fmt.Errorf("scaffold: %w", err)
		}
		result.Fresh = true

	case ymlExists && !wikiExists:
		if err := os.MkdirAll(filepath.Join(wikiDir, "modules"), 0o755); err != nil {
			return result, fmt.Errorf("recreate wiki/: %w", err)
		}
		result.Recovered = "wiki dir missing — recreated from wiki.yml"

	case !ymlExists && wikiExists:
		yml, err := reconstructYmlFromWiki(wikiDir)
		if err != nil {
			return result, fmt.Errorf("reconstruct wiki.yml: %w", err)
		}
		encoded, err := encodeWikiYml(yml)
		if err != nil {
			return result, err
		}
		if err := atomicWrite(ymlPath, string(encoded)); err != nil {
			return result, fmt.Errorf("write reconstructed wiki.yml: %w", err)
		}
		result.Recovered = "yml missing — reconstructed from wiki/ contents (last_sha unknown; modules may regenerate as stubs on next run)"
	}

	ymlData, err := os.ReadFile(ymlPath)
	if err != nil {
		return result, fmt.Errorf("read wiki.yml: %w", err)
	}
	yml, err := parseWikiYml(ymlData)
	if err != nil {
		return result, fmt.Errorf("parse wiki.yml: %w", err)
	}

	// Snapshot the module roster BEFORE reconcile so we can
	// compute Added / Removed from the diff. (Plan operates
	// on pending[]; it doesn't tell us "this path was newly
// in source this run" cleanly.)
	beforeLive := make(map[string]bool)
	beforeRemoved := make(map[string]bool)
	for _, m := range yml.Modules {
		if m.Removed {
			beforeRemoved[m.Path] = true
		} else {
			beforeLive[m.Path] = true
		}
	}

	// Phase 1: Plan. Refresh modules[] from current source
	// (reconcile new / removed), then compute pending.
	currentModules, err := discoverModules(cwd)
	if err != nil {
		return result, fmt.Errorf("discover: %w", err)
	}
	yml.Modules = reconcileModules(yml.Modules, currentModules)

	if err := Plan(cwd, yml, git); err != nil {
		return result, err
	}

	// Compute Added / Removed from the before/after diff.
	// Also track Preserved: any live module with LastSHA set
	// that was NOT added to pending this run (Plan skipped
	// it because git diff was empty) had its wiki file left
	// untouched.
	pendingPaths := make(map[string]bool)
	for _, p := range yml.Pending {
		pendingPaths[p.Path] = true
	}
	for _, m := range yml.Modules {
		switch {
		case !m.Removed && !beforeLive[m.Path]:
			result.Added = append(result.Added, m.Path)
		case m.Removed && !beforeRemoved[m.Path]:
			result.Removed = append(result.Removed, m.Path)
		case !m.Removed && !pendingPaths[m.Path] && m.LastSHA != nil:
			// Live module, no pending entry, has LastSHA
			// recorded → Plan decided nothing to do → preserved.
			result.Preserved = append(result.Preserved, m.File)
		}
	}

	// Phase 2: Apply.
	applyRes, err := Apply(cwd, yml, llm, git)
	if err != nil {
		return result, err
	}
	// Merge Apply results with the Plan-skip Preserved entries
	// computed above (dedup — same module can appear in both).
	seen := make(map[string]bool)
	for _, p := range result.Preserved {
		seen[p] = true
	}
	for _, p := range applyRes.Skipped {
		if !seen[p] {
			result.Preserved = append(result.Preserved, p)
			seen[p] = true
		}
	}
	result.Written = applyRes.Done
	for _, p := range applyRes.Failed {
		result.Failed = append(result.Failed, p)
	}

	// Always persist the post-reconcile yml even when Apply
	// had nothing to do — reconcile may have changed Removed
	// flags (e.g., re-introduced modules must have removed:true
	// cleared). Apply only writes when it processes entries;
	// a Plan-skipped-everything run would otherwise leave the
	// file stale.
	if len(yml.Pending) == 0 {
		if err := writeWikiYmlFile(cwd, yml); err != nil {
			return result, err
		}
	}

	// After Plan + Apply, the final wiki.yml reflects the
	// post-apply state. Reload it so the in-memory copy
	// callers see is the persisted one.
	ymlData, _ = os.ReadFile(ymlPath)
	if ymlData != nil {
		if final, err := parseWikiYml(ymlData); err == nil {
			*yml = *final
		}
	}

	return result, nil
}

// SyncResult is the structured outcome of a /wiki run.
// Reply formatter reads these slices.
type SyncResult struct {
	Fresh     bool     // true when /wiki created the wiki from scratch
	Recovered string   // non-empty when /wiki recovered from a half-state
	Added     []string // paths newly added to wiki.yml.modules[]
	Removed   []string // paths marked removed:true
	Written   []string // wiki files written this run (stub mode)
	Preserved []string // wiki files skipped (already done from a prior run)
	Failed    []string // pending entries that errored (kept for retry)
}

// reconstructYmlFromWiki walks wiki/modules/ and produces a
// wikiYml from the file paths. Every <pkg-path>.md becomes a
// moduleYml with last_sha=nil (we have no record of git SHAs
// from filenames alone).
func reconstructYmlFromWiki(wikiDir string) (*wikiYml, error) {
	modulesDir := filepath.Join(wikiDir, "modules")
	var modules []moduleYml
	err := filepath.WalkDir(modulesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(modulesDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		pkgPath := strings.TrimSuffix(rel, ".md")
		modules = append(modules, moduleYml{
			Path: pkgPath,
			File: rel,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortModulesByPath(modules)
	return &wikiYml{
		Version: 1,
		Modules: modules,
	}, nil
}

// reconcileModules produces the new modules[] from existing
// + currently-discovered. The order is stable: live modules
// first (path asc), then removed modules (path asc).
func reconcileModules(existing []moduleYml, current []moduleEntry) []moduleYml {
	existingByPath := make(map[string]moduleYml, len(existing))
	for _, e := range existing {
		existingByPath[e.Path] = e
	}
	currentByPath := make(map[string]moduleEntry, len(current))
	for _, c := range current {
		currentByPath[c.Path] = c
	}

	sortedCurrent := append([]moduleEntry(nil), current...)
	sort.Slice(sortedCurrent, func(i, j int) bool {
		return sortedCurrent[i].Path < sortedCurrent[j].Path
	})

	var out []moduleYml
	for _, c := range sortedCurrent {
		if e, ok := existingByPath[c.Path]; ok {
			e.Removed = false
			e.File = c.File
			out = append(out, e)
		} else {
			out = append(out, moduleYml{
				Path: c.Path,
				File: c.File,
			})
		}
	}

	var removedPaths []string
	for path := range existingByPath {
		if _, ok := currentByPath[path]; !ok {
			removedPaths = append(removedPaths, path)
		}
	}
	sortStrings(removedPaths)
	for _, p := range removedPaths {
		e := existingByPath[p]
		e.Removed = true
		out = append(out, e)
	}
	return out
}

func sortModulesByPath(modules []moduleYml) {
	sortModuleEntries(nil, modules) // indirection to avoid duplication
}

// sortModuleEntries sorts module entries by Path. The module
// slice is sorted in place via index mapping; the entries
// slice is a copy that gets reordered.
func sortModuleEntries(entries []moduleEntry, modules []moduleYml) {
	// Sort modules by path using a simple insertion sort
	// (n is typically < 100, so this is fine).
	for i := 1; i < len(modules); i++ {
		for j := i; j > 0 && modules[j].Path < modules[j-1].Path; j-- {
			modules[j], modules[j-1] = modules[j-1], modules[j]
		}
	}
	_ = entries
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// --- source file scanning (used by regenerate step) ---

type sourceFile struct {
	Name    string
	RelPath string
	Lines   int
}

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

	result, err := SyncWith(cwd, args.Agent, f.git, stubDispatcher{})
	if err != nil {
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

// formatSyncReply renders the success card. Lean — counts and
// short bullets, per AGENTS.md §1 / project reply convention.
func formatSyncReply(r SyncResult) string {
	var b strings.Builder
	switch {
	case r.Fresh:
		b.WriteString("✅ /wiki (fresh)\n\n")
	case r.Recovered != "":
		b.WriteString("✅ /wiki (recovered)\n\n")
	default:
		b.WriteString("✅ /wiki\n\n")
	}

	if r.Recovered != "" {
		b.WriteString("Recovery: ")
		b.WriteString(r.Recovered)
		b.WriteString("\n\n")
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
		fmt.Fprintf(&b, "Marked %d removed (wiki files deleted):\n", len(r.Removed))
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

	if len(r.Failed) > 0 {
		fmt.Fprintf(&b, "Failed %d (will retry on next /wiki):\n", len(r.Failed))
		for _, p := range r.Failed {
			b.WriteString("• ")
			b.WriteString(p)
			b.WriteByte('\n')
		}
	}

	if r.Fresh {
		b.WriteString("\nCreated wiki/ + wiki.yml from scratch.")
	} else if len(r.Added) == 0 && len(r.Removed) == 0 && len(r.Written) == 0 && len(r.Preserved) == 0 && len(r.Failed) == 0 && r.Recovered == "" {
		b.WriteString("\nNo changes — wiki already in sync with source tree.")
	}

	return b.String()
}

// silenced unused import in stubDispatcher code path
var _ = fmt.Sprintf