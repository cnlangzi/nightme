package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Validation contract — docs/Wiki.md §6.
//
// The runtime inspects the file system after the Agent's
// init response. Each module file must have a parseable
// frontmatter with the four required fields, a unique
// kebab-case name, and a covers list whose paths exist in
// the repository. The union of all covers must cover every
// tracked source file in the repository.

// nameRE accepts kebab-case identifiers: lowercase letters,
// digits, and hyphens; must start and end with a letter or
// digit; 1-5 words separated by single hyphens.
var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+){0,4}$`)

// ModuleResult is one module's pass / fail outcome. Failed
// carries the reason for the reply and any log.
type ModuleResult struct {
	Name   string
	File   string
	Reason string // empty on success
}

// ValidateAll walks wiki/modules/*.md, reads each file's
// frontmatter, and reports per-file pass / fail. It also
// returns the union of all covers (deduplicated) and the
// set of duplicate names. Coverage against the repo's
// tracked source files is computed by CheckCoverage, not
// here, because git is a separate dependency.
func ValidateAll(wikiRoot string) (passed []ModuleResult, failed []ModuleResult, covers map[string]bool, dupNames []string) {
	modulesDir := filepath.Join(wikiRoot, "modules")
	covers = make(map[string]bool)
	seen := make(map[string]string) // name → file
	all := scanModules(modulesDir)

	for _, f := range all {
		meta, _, err := ReadFrontmatter(f)
		if err != nil {
			failed = append(failed, ModuleResult{
				File:   relWiki(wikiRoot, f),
				Reason: "frontmatter: " + err.Error(),
			})
			continue
		}
		if meta.Name == "" {
			failed = append(failed, ModuleResult{
				File:   relWiki(wikiRoot, f),
				Reason: "missing name",
			})
			continue
		}
		if !nameRE.MatchString(meta.Name) {
			failed = append(failed, ModuleResult{
				Name:   meta.Name,
				File:   relWiki(wikiRoot, f),
				Reason: fmt.Sprintf("name %q is not kebab-case (1-5 lowercase words, hyphen-separated)", meta.Name),
			})
			continue
		}
		if other, ok := seen[meta.Name]; ok {
			dupNames = append(dupNames, meta.Name)
			failed = append(failed, ModuleResult{
				Name:   meta.Name,
				File:   relWiki(wikiRoot, f),
				Reason: fmt.Sprintf("name %q already used by %s", meta.Name, relWiki(wikiRoot, other)),
			})
			continue
		}
		seen[meta.Name] = f

		bad := validateCovers(meta.Covers, wikiRoot)
		if bad != "" {
			failed = append(failed, ModuleResult{
				Name:   meta.Name,
				File:   relWiki(wikiRoot, f),
				Reason: bad,
			})
			continue
		}
		for _, c := range meta.Covers {
			covers[c] = true
		}
		passed = append(passed, ModuleResult{Name: meta.Name, File: relWiki(wikiRoot, f)})
	}
	sort.Slice(passed, func(i, j int) bool { return passed[i].Name < passed[j].Name })
	sort.Slice(failed, func(i, j int) bool { return failed[i].Name < failed[j].Name })
	return passed, failed, covers, dupNames
}

// validateCovers checks that every path in covers exists
// in the repository, relative to repoRoot. Returns an empty
// string on success; otherwise a human-readable reason.
func validateCovers(covers []string, repoRoot string) string {
	if len(covers) == 0 {
		return "covers is empty"
	}
	for _, c := range covers {
		if c == "" {
			return "covers contains an empty path"
		}
		if strings.HasPrefix(c, "/") {
			return fmt.Sprintf("covers path %q is absolute; must be repo-relative", c)
		}
		abs := filepath.Join(repoRoot, filepath.FromSlash(c))
		if _, err := os.Stat(abs); err != nil {
			return fmt.Sprintf("covers path %q does not exist: %v", c, err)
		}
	}
	return ""
}

// scanModules returns the absolute paths of every .md file
// under modulesDir, sorted.
func scanModules(modulesDir string) []string {
	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, filepath.Join(modulesDir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// relWiki returns the path of f relative to wikiRoot, with
// forward slashes for stable reply formatting.
func relWiki(wikiRoot, f string) string {
	rel, err := filepath.Rel(wikiRoot, f)
	if err != nil {
		return f
	}
	return filepath.ToSlash(rel)
}

// CheckCoverage returns the list of tracked source files in
// the repository that no module covers. trackedFiles is the
// output of `git ls-files` (or equivalent) — repo-relative
// paths. covered is the set of repo-relative paths from all
// module frontmatters.
//
// A tracked file is considered "covered" when it is a child
// of any cover path, or when any cover path equals it. Build
// artifacts and vendored dependencies are filtered out
// before this function is called.
func CheckCoverage(trackedFiles []string, covered map[string]bool) []string {
	var uncovered []string
	for _, f := range trackedFiles {
		if isCovered(f, covered) {
			continue
		}
		uncovered = append(uncovered, f)
	}
	sort.Strings(uncovered)
	return uncovered
}

// isCovered reports whether a file matches any path in the
// covered set. A file is covered if it equals a covered path
// or descends from one.
func isCovered(file string, covered map[string]bool) bool {
	for c := range covered {
		if c == file {
			return true
		}
		if isParent(c, file) {
			return true
		}
	}
	return false
}

// isParent reports whether parent is an ancestor of child
// (i.e. child is inside parent). Both paths are forward-
// slashed, no trailing slash on parent.
func isParent(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	if !strings.HasSuffix(parent, "/") {
		parent += "/"
	}
	return strings.HasPrefix(child, parent)
}

// HasExistingWiki reports whether wiki/modules/ already
// contains at least one .md file. Init refuses to overwrite
// an existing wiki (docs/Wiki.md §3.1).
func HasExistingWiki(wikiRoot string) bool {
	entries, err := os.ReadDir(filepath.Join(wikiRoot, "modules"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			return true
		}
	}
	return false
}
