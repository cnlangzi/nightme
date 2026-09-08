package wiki

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Validation contract — docs/Wiki.md §6.
//
// The Agent's only contract is the file itself. There is no
// frontmatter. The module's identity is its filename
// (`<name>.md` where `<name>` is the kebab-case ID). Inside
// the file, the Agent must write an H1 heading and a
// one-line blockquote immediately after.
//
// Validation walks wiki/modules/*.md and reports pass / fail
// per file. There is no covers-list, no central index, no
// coverage check. The Agent owns every byte of content;
// the runtime owns the directory shape and the filename
// contract.

// nameRE accepts kebab-case identifiers: lowercase letters,
// digits, and hyphens; must start and end with a letter or
// digit; 1-5 words separated by single hyphens.
var nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+){0,4}$`)

// ModuleResult is one module's pass / fail outcome.
type ModuleResult struct {
	Name   string
	File   string
	Reason string // empty on success
}

// ValidateAll walks wiki/modules/*.md. Each file must:
//   - have a basename matching nameRE (kebab-case)
//   - have a unique basename across the directory
//   - start with an H1 heading ("# <something>")
//   - have a one-line blockquote ("<something>") right after
//     the H1
func ValidateAll(wikiRoot string) (passed []ModuleResult, failed []ModuleResult) {
	modulesDir := filepath.Join(wikiRoot, "modules")
	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		return nil, []ModuleResult{{
			File:   relWiki(wikiRoot, modulesDir),
			Reason: fmt.Sprintf("read modules dir: %v", err),
		}}
	}

	seen := make(map[string]string) // name → filename
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if name == e.Name() || name == "" {
			// Not a .md file or empty name; skip silently.
			continue
		}
		file := filepath.Join(modulesDir, e.Name())
		result := ModuleResult{
			Name: name,
			File: relWiki(wikiRoot, file),
		}
		reason := ""
		if !nameRE.MatchString(name) {
			reason = fmt.Sprintf("name %q is not kebab-case (1-5 lowercase words, hyphen-separated)", name)
		} else if other, ok := seen[name]; ok {
			reason = fmt.Sprintf("name %q already used by %s", name, relWiki(wikiRoot, other))
		} else {
			seen[name] = file
			reason = checkHeader(file)
		}
		if reason != "" {
			result.Reason = reason
			failed = append(failed, result)
			continue
		}
		passed = append(passed, result)
	}
	sort.Slice(passed, func(i, j int) bool { return passed[i].Name < passed[j].Name })
	sort.Slice(failed, func(i, j int) bool { return failed[i].Name < failed[j].Name })
	return passed, failed
}

// checkHeader verifies that file begins with an H1 line and
// has a one-line blockquote immediately after. Returns "" on
// success; otherwise a short reason.
func checkHeader(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("open: %v", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)

	var sawH1, sawBlockquote bool
	mode := 0 // 0 = before H1, 1 = after H1 (looking for blockquote), 2 = ok
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch mode {
		case 0:
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "# ") {
				return "first non-empty line must be an H1 heading"
			}
			sawH1 = true
			mode = 1
		case 1:
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "> ") {
				return "line after H1 must be a one-line blockquote"
			}
			sawBlockquote = true
			mode = 2
		default:
			// We only care about the first two non-empty lines.
		}
		if sawH1 && sawBlockquote {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Sprintf("scan: %v", err)
	}
	if !sawH1 {
		return "missing H1 heading"
	}
	if !sawBlockquote {
		return "missing one-line blockquote after H1"
	}
	return ""
}

// relWiki returns path of f relative to wikiRoot, with
// forward slashes for stable reply formatting.
func relWiki(wikiRoot, f string) string {
	rel, err := filepath.Rel(wikiRoot, f)
	if err != nil {
		return f
	}
	return filepath.ToSlash(rel)
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
