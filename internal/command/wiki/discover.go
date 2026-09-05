package wiki

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// moduleEntry is one Go package detected during /wiki init.
// Path is the package's directory relative to cwd; File is
// the wiki page name (basename + ".md"); WikiRelPath is the
// path relative to <cwd>/wiki/ — used when rendering
// wiki.yml.modules[].filePaths and llms.txt module links.
type moduleEntry struct {
	Path        string // e.g. "internal/command/gtw"
	File        string // e.g. "gtw.md"
	WikiRelPath string // e.g. "modules/gtw.md"
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

		// Two-phase: first check whether this directory is a
		// leaf module (has at least one module-relevant file),
		// then decide whether to recurse. Reading the directory
		// once and using it for both phases avoids a redundant
		// ReadDir syscall per directory.
		//
		// "Module-relevant" = a regular file that is not a Go
		// test file (*_test.go). The Go convention is the only
		// test-file pattern recognised in v0 — non-Go languages
		// use different conventions (*.test.ts, *_test.py,
		// *_spec.rb, ...) and will extend this rule when their
		// providers land. Today the rule still does its job for
		// Go-heavy repos like nightme itself: a directory with
		// only *_test.go is a test-only concern that the LLM
		// surfaces inside the corresponding production module's
		// doc (cross-cutting pattern), not as its own page.
		hasModuleFile := false
		var subDirs []os.DirEntry
		for _, c := range children {
			if c.IsDir() {
				subDirs = append(subDirs, c)
				continue
			}
			if strings.HasSuffix(c.Name(), "_test.go") {
				continue
			}
			hasModuleFile = true
		}

		if rel != "" && hasModuleFile {
			slug := filepath.Base(rel)
			entries = append(entries, moduleEntry{
				Path:        filepath.ToSlash(rel),
				File:        slug + ".md",
				WikiRelPath: filepath.ToSlash(filepath.Join("modules", slug+".md")),
			})
			// Leaf — do not recurse into sub-packages.
			return nil
		}

		// Container (no regular files): recurse into children
		// that survive the filter.
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
