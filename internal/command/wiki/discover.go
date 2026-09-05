package wiki

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// moduleEntry is one source package detected during /wiki.
//
// Path is the package's directory relative to cwd, slash-
// separated (e.g. "internal/command/gtw"). File is the wiki
// file path RELATIVE TO <cwd>/wiki/ — by convention a
// mirror of Path with ".md" appended (nested directory
// layout: wiki/modules/internal/command/gtw.md). The mirror
// means an LLM reading a wiki page can locate the
// corresponding source without cross-referencing wiki.yml.
//
// WikiRelPath is the File prefixed with "modules/" — used
// for llms.txt module links and any caller that needs the
// full path relative to the wiki root.
type moduleEntry struct {
	Path        string // e.g. "internal/command/gtw"
	File        string // e.g. "internal/command/gtw.md" (nested, mirror of Path)
	WikiRelPath string // e.g. "modules/internal/command/gtw.md"
}

// discoverModules walks cwd and returns every directory that
// contains at least one file (the leaf rule — directories
// with only sub-directories are containers, recursed into).
//
// Filtering (see ignore.go for the full filter stack):
//   - Built-in ignores (just "wiki" for now).
//   - User's .gitignore.
//   - dot-prefix rule (any path component starting with ".").
//   - User's wiki.yml.include — exact-match exemption.
//
// The walker is bounded:
//   - directories listed in ignoreFilter.shouldSkip are not
//     entered (SkipDir).
//   - directories with at least one regular file are LEAF
//     modules — their sub-directories are not entered.
//
// The returned slice is sorted by Path for stable wiki.yml
// and llms.txt output.
func discoverModules(cwd string) ([]moduleEntry, error) {
	filter, err := loadIgnoreFilter(cwd)
	if err != nil {
		return nil, fmt.Errorf("load ignore filter: %w", err)
	}

	var entries []moduleEntry

	var walk func(dir string) error
	walk = func(dir string) error {
		rel, relErr := filepath.Rel(cwd, dir)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			rel = ""
		}

		if rel != "" && filter.shouldSkip(rel, true) {
			return filepath.SkipDir
		}

		children, err := os.ReadDir(dir)
		if err != nil {
			return err
		}

		// Module detection: any non-hidden regular file makes
		// this directory a module. Test files (_test.go) count
		// — they go into git, so the directory is a real unit
		// of work and deserves its own page. Language agnostic:
		// no extension allowlist; gitignore already filtered
		// out anything that shouldn't be tracked.
		//
		// Hidden files (.*) do NOT count — they are tooling /
		// OS junk that would otherwise mark e.g. .vscode/ as
		// a module. The dot-prefix rule on directories handles
		// the directory case; this handles the file-inside-
		// visible-dir case (rare, but e.g. ".envrc" in src/).
		hasModuleFile := false
		var subDirs []os.DirEntry
		for _, c := range children {
			if c.IsDir() {
				subDirs = append(subDirs, c)
				continue
			}
			if strings.HasPrefix(c.Name(), ".") {
				continue
			}
			hasModuleFile = true
		}

		if rel != "" && hasModuleFile {
			// Emit module. Then CONTINUE recursing — no leaf
	 // rule. Every non-empty dir gets its own wiki page;
	 // sub-packages stay as their own modules, not
	 // folded into the parent.
	 //
	 // Wiki file path mirrors source path one-to-one:
	 // internal/command/gtw → wiki/modules/internal/
	 // command/gtw.md. The mirror is what kills basename
	 // collisions (internal/command/gtw vs
	 // internal/cli/gtw otherwise want the same file).
	 entries = append(entries, moduleEntry{
		 Path:        filepath.ToSlash(rel),
		 File:        filepath.ToSlash(rel) + ".md",
		 WikiRelPath: filepath.ToSlash(filepath.Join("modules", rel)) + ".md",
	 })
		}

		// Always recurse into sub-directories that survive
		// the filter — the walk goes all the way down.
		for _, c := range subDirs {
			name := c.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			if filter.shouldSkip(childRel, true) {
				continue
			}
			if err := walk(filepath.Join(dir, name)); err != nil {
				if errors.Is(err, filepath.SkipDir) {
					continue
				}
				return err
			}
		}
		return nil
	}

	if err := walk(cwd); err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries, nil
}
