package wiki

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ignoreFilter combines a parsed .gitignore with the user's
// wiki.yml `include` list and applies them to candidate
// directories during discovery.
//
// Layering (highest priority first):
//
//  1. Exact include match — exempts the path from all other
//     rules. Children of an included dir are NOT auto-exempt;
//     each child path is checked independently. To exempt a
//     nested hidden dir, list it explicitly in include.
//  2. .gitignore / built-in — matched as gitignore (see
//     loadGitignore for syntax).
//  3. dot-prefix rule — any path component starting with "."
//     is skipped, unless the leading-dot component's full path
//     exactly matches an include entry.
//
// Built-in ignores (always applied, even with no .gitignore):
//
//   - "wiki" — self-recursion guard, cannot be overridden by
//     include. Without this, /wiki update could re-discover
//     its own output.
//
// Everything else ("node_modules", "vendor", "target", ".git",
// ".vscode", ...) is expected in the user's .gitignore. We
// deliberately do NOT duplicate those defaults —
// "进 git 的都是需要的" is the wiki's filtering philosophy:
// trust the user's .gitignore as the single source of truth.
type ignoreFilter struct {
	git     *gitignoreParser
	include []string
}

// loadIgnoreFilter reads <cwd>/.gitignore (if present) and
// <cwd>/wiki.yml (if present, for the include list) and
// returns the combined filter. Missing files are non-fatal;
// the filter still works with whatever layers are available.
//
// Returns an error only on I/O failures other than "file not
// found" — those would indicate a deeper problem worth
// surfacing.
func loadIgnoreFilter(cwd string) (*ignoreFilter, error) {
	git, err := loadGitignore(cwd)
	if err != nil {
		return nil, err
	}
	includes, err := loadWikiInclude(cwd)
	if err != nil {
		return nil, err
	}
	return &ignoreFilter{git: git, include: includes}, nil
}

// shouldSkip reports whether relPath (relative to cwd, no
// leading "./") should be excluded from module discovery.
// isDir tells the matcher whether relPath is a directory
// (needed for gitignore's dir-only patterns like "foo/").
//
// Dot-prefix consumption: when a path's leading dot-component
// (e.g. ".github" in ".github/workflows") exactly matches an
// include entry, the dot is "consumed" and the walker
// proceeds into sub-paths. Nested dot-components (".private"
// inside ".github/.private") are NOT auto-consumed — each
// one must be its own include entry to be visible.
func (f *ignoreFilter) shouldSkip(relPath string, isDir bool) bool {
	// Built-in guards run first and cannot be overridden by
	// include. Same applies for any future built-in that has
	// the same hard-skip character.
	for _, bi := range builtinIgnores {
		if relPath == bi || strings.HasPrefix(relPath, bi+"/") {
			return true
		}
	}

	// Exact include match on the FULL path exempts entirely
	// (covers both gitignore and dot-prefix rules).
	if f.includesExact(relPath) {
		return false
	}

	if f.git != nil && f.git.isIgnored(relPath, isDir) {
		return true
	}

	// dot-prefix: scan components left-to-right. The first
	// dot-prefixed component may be consumed by an include
	// match (allowing the walker to descend into it). Any
	// LATER dot-prefixed component is NOT auto-consumed — the
	// user must list each one explicitly.
	parts := strings.Split(relPath, string(filepath.Separator))
	for i, p := range parts {
		if !strings.HasPrefix(p, ".") {
			continue
		}
		componentPath := strings.Join(parts[:i+1], string(filepath.Separator))
		if f.includesExact(componentPath) {
			continue
		}
		return true
	}
	return false
}

// includesExact reports whether path matches any include
// entry verbatim. Children are NOT matched; the walker
// re-evaluates each child against the full filter stack.
func (f *ignoreFilter) includesExact(path string) bool {
	for _, inc := range f.include {
		if path == inc {
			return true
		}
	}
	return false
}

// builtinIgnores is the hard-coded minimum. Only "wiki"
// (self-recursion guard) lives here. All other typical
// ignore patterns (node_modules, vendor, target, .git,
// .vscode, ...) are expected in the user's .gitignore.
var builtinIgnores = []string{"wiki"}

// --- .gitignore parser (minimal, covers v0 syntax) ---

// gitignoreParser holds the patterns read from a single
// .gitignore file. v0 supports:
//
//   - literal patterns:      foo
//   - glob patterns:         *.tmp  foo?bar  [abc]
//   - anchored patterns:     /foo   (only matches at repo root)
//   - directory-only:        foo/   (only matches directories)
//   - negation:              !foo   (un-ignores a path)
//
// Out of scope for v0 (defer until users ask):
//   - ** across directories (e.g. foo/**/bar)
//   - nested .gitignore files in subdirectories
//   - character classes with negation ([!abc])
//
// Patterns are applied in file order, matching gitignore's
// "last match wins" semantics.
type gitignoreParser struct {
	patterns []gitignorePattern
}

type gitignorePattern struct {
	negated  bool
	anchored bool
	dirOnly  bool
	pattern  string
}

// loadGitignore reads <cwd>/.gitignore if it exists. Missing
// file returns an empty parser (no error) — the filter still
// works via built-ins + dot-prefix + include.
func loadGitignore(cwd string) (*gitignoreParser, error) {
	p := &gitignoreParser{}
	path := filepath.Join(cwd, ".gitignore")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p.patterns = append(p.patterns, parseGitignoreLine(line))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

func parseGitignoreLine(line string) gitignorePattern {
	p := gitignorePattern{}
	if strings.HasPrefix(line, "!") {
		p.negated = true
		line = line[1:]
	}
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	p.pattern = line
	return p
}

// isIgnored reports whether relPath (with no leading "./")
// is ignored under gitignore semantics. isDir tells the
// matcher whether the path is a directory (needed for
// dir-only patterns).
//
// "Last match wins": a later negated pattern un-ignores a
// previously-ignored path; a later non-negated pattern
// re-ignores a previously-un-ignored path. This matches
// gitignore's documented behaviour.
func (p *gitignoreParser) isIgnored(relPath string, isDir bool) bool {
	ignored := false
	for _, pat := range p.patterns {
		if pat.dirOnly && !isDir {
			continue
		}
		if !matchPattern(pat, relPath) {
			continue
		}
		ignored = !pat.negated
	}
	return ignored
}

// matchPattern reports whether pat matches relPath.
//
// Anchored patterns match the FULL relPath only. Unanchored
// patterns match any single path component OR the full
// relPath — this is the simplification we make instead of
// implementing gitignore's full ** semantics.
func matchPattern(pat gitignorePattern, relPath string) bool {
	if pat.anchored {
		ok, _ := filepath.Match(pat.pattern, relPath)
		return ok
	}
	for _, part := range strings.Split(relPath, string(filepath.Separator)) {
		if ok, _ := filepath.Match(pat.pattern, part); ok {
			return true
		}
	}
	if ok, _ := filepath.Match(pat.pattern, relPath); ok {
		return true
	}
	return false
}

// --- wiki.yml include reader (minimal YAML scan) ---

// loadWikiInclude reads the `include:` field from
// <cwd>/wiki.yml. Returns nil if the file is missing OR
// the field is absent — both states mean "no force-includes".
//
// We deliberately do NOT pull in a YAML dependency just for
// this one field. The scanner only handles:
//
//	include:
//	  - .github
//	  - .config
//
// which is exactly what /wiki init writes. If the user
// hand-edits wiki.yml into a richer shape (anchors,
// multi-line strings, etc.), this scanner will silently miss
// the include field — acceptable for v0, with a future
// upgrade to a real YAML parser if/when /wiki update needs
// the modules[] entries too.
func loadWikiInclude(cwd string) ([]string, error) {
	path := filepath.Join(cwd, "wiki.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var includes []string
	inInclude := false
	for _, raw := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(trimmed, "include:"):
			inInclude = true
			// Handle inline list (uncommon): "include: [.github]"
			rest := strings.TrimPrefix(trimmed, "include:")
			rest = strings.TrimSpace(rest)
			if rest == "[]" || rest == "" {
				continue
			}
			if strings.HasPrefix(rest, "[") {
				// Inline list — extract items between [ and ].
				rest = strings.Trim(rest, "[]")
				for _, item := range strings.Split(rest, ",") {
					item = strings.TrimSpace(item)
					item = strings.Trim(item, "\"'")
					if item != "" {
						includes = append(includes, item)
					}
				}
				inInclude = false
			}
		case inInclude && strings.HasPrefix(trimmed, "- "):
			entry := strings.TrimPrefix(trimmed, "- ")
			entry = strings.TrimSpace(entry)
			entry = strings.Trim(entry, "\"'")
			if entry != "" {
				includes = append(includes, entry)
			}
		default:
			inInclude = false
		}
	}
	return includes, nil
}
