package wiki

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Skeleton templates for /wiki init. Pure content — no
// filesystem IO. Lives in skeleton.go so /wiki update logic
// can call the same template functions with per-module
// context and render without re-implementing the structure.
//
// Skeletons are intentionally empty-bodied: section headings
// exist so the user can see what /wiki update will fill, and
// so humans editing the file manually know what's expected.
// Empty sections are honest signals that the content has not
// been generated yet — better than placeholder prose that
// would have to be deleted on the first update pass.

// architectureSkeleton returns the top-level architecture
// page. Section list mirrors DeepWiki's page_type=architecture
// convention (Components / Main Flows / Dependency Graph /
// Key Invariants) so /wiki update can rely on those H2s as
// stable anchors.
func architectureSkeleton() string {
	return `# Architecture

## Components

## Main Flows

## Module Dependency Graph

## Key Invariants
`
}

// glossarySkeleton returns the term reference page. Empty
// table — populated by /wiki update as module docs surface
// cross-package concepts. The 3-column header (Term /
// Definition / Source) matches DeepWiki reference pages.
func glossarySkeleton() string {
	return `# Glossary

| Term | Definition | Source |
|---|---|---|
`
}

// llmsTxtSkeletonWithModules returns the llms.txt v1.0 index
// with the detected modules listed under "## Modules". Per
// llmstxt.org spec, each entry is a markdown link; the
// trailing ": description" is optional and omitted here
// because /wiki init has no LLM-generated prose yet.
//
// Architecture and Reference sections stay populated from
// day one so the file is a valid llms.txt the moment init
// completes. /wiki update can later add a blockquote summary
// and per-module descriptions once content exists.
func llmsTxtSkeletonWithModules(projectName string, modules []moduleEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", projectName)
	b.WriteString("## Architecture\n\n")
	b.WriteString("- [Architecture Overview](./architecture.md)\n\n")
	b.WriteString("## Modules\n")
	if len(modules) > 0 {
		for _, m := range modules {
			// Label is the package basename, not the full path.
			// Full path is conveyed by the link target; the
			// label stays short so the index is scannable.
			label := filepath.Base(strings.TrimSuffix(m.File, ".md"))
			fmt.Fprintf(&b, "- [%s](./%s)\n", label, m.WikiRelPath)
		}
	}
	b.WriteString("\n## Reference\n\n")
	b.WriteString("- [Glossary](./glossary.md)\n")
	return b.String()
}

// wikiYmlSkeletonWithModules renders the metadata file with
// the detected module list pre-populated. Each entry holds
// its source path, wiki file, and last_sha (null until the
// first /wiki update commits generated content).
//
// agent is written as YAML null at init time — no flag
// affects it. /wiki update records the agent it ran with
// into wiki.yml on commit, which then becomes the default
// for subsequent updates.
//
// include is always written (empty list by default) so
// users know the field exists and can hand-edit wiki.yml
// after init to force-include paths that .gitignore would
// otherwise exclude. /wiki update reads this list at run
// time; /wiki init itself does not consume it (init refuses
// to re-run when wiki.yml already exists).
//
// Schema version 1: the first shipped schema. Bump on any
// backwards-incompatible change to the modules[] shape.
//
// Location contract: this file lives at <cwd>/wiki.yml,
// SIBLING to <cwd>/wiki/. NOT inside <cwd>/.nightme/.
//
// Reason: <cwd>/.nightme/ is for runtime-emitted, repo-local
// config (mirrors ~/.nightme/gtw.yml). Those files are
// optionally committed per team preference and are
// regenerated freely. wiki.yml is different — it is a
// FIRST-CLASS repo artifact, like README.md or go.mod: it
// tracks the wiki's state (last_commit SHA, module roster,
// schema version) and is meant to be committed and evolve
// with the repo's history.
//
// Moving wiki.yml into <cwd>/.nightme/ would conflate the
// two roles and cause /wiki update to silently re-run on
// machines where .nightme/ was gitignored.
func wikiYmlSkeletonWithModules(modules []moduleEntry) string {
	var b strings.Builder
	b.WriteString("version: 1\n")
	b.WriteString("last_commit: null\n")
	b.WriteString("agent: null\n")
	b.WriteString("include: []\n")
	if len(modules) == 0 {
		b.WriteString("modules: []\n")
		return b.String()
	}
	b.WriteString("modules:\n")
	for _, m := range modules {
		fmt.Fprintf(&b, "  - path: %s\n", m.Path)
		fmt.Fprintf(&b, "    file: %s\n", m.File)
		b.WriteString("    last_sha: null\n")
	}
	return b.String()
}

// moduleDocSkeleton returns the per-package template page.
// Section list mirrors DeepWiki's page_type=component convention.
// H1 is the package basename — same as the wiki file's stem —
// so the rendered filename `<stem>.md` opens with `# <stem>`.
//
// Empty bodies are honest signals. /wiki update replaces each
// section wholesale rather than appending (AGENTS.md §1:
// "rewrite, don't keep two versions side by side").
func moduleDocSkeleton(pkgPath string) string {
	title := "# " + filepath.Base(pkgPath) + "\n\n"

	sections := []string{
		"## Public Surface",
		"## File Layout",
		"## Key Flows",
		"## Cross-cutting Patterns",
		"## Non-obvious Choices",
	}
	var b strings.Builder
	b.WriteString(title)
	for _, s := range sections {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	return b.String()
}

// moduleDocStub returns the per-package page that /wiki
// update writes when no LLM is available (or when the LLM
// path is intentionally bypassed for testing). It reuses
// moduleDocSkeleton's 5-section shape so the LLM path can
// later fill the placeholders without restructuring the
// file.
//
// What's filled in (real data from the source tree):
//   - File Layout table — file name + line count
//   - <!-- sources --> footer — every source file's relative
//     path, mirroring DeepWiki's "minimum 5 source files per
//     page" convention
//
// What's still placeholder:
//   - Public Surface, Key Flows, Cross-cutting Patterns,
//     Non-obvious Choices — each gets a single "[TBD — ...]"
//     line so the LLM prompt template can grep for "TBD"
//     and know what to expand. The bracketed hint reminds
//     humans editing by hand what's expected.
func moduleDocStub(pkgPath string, sources []sourceFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", filepath.Base(pkgPath))
	b.WriteString("> Stub generated by `/wiki update` (no LLM). Replace with LLM content on the next agent-driven update.\n\n")

	b.WriteString("## Public Surface\n\n")
	b.WriteString("[TBD — exported types and functions from this package]\n\n")

	b.WriteString("## File Layout\n\n")
	if len(sources) == 0 {
		b.WriteString("[no source files detected]\n\n")
	} else {
		b.WriteString("| File | Lines |\n")
		b.WriteString("|---|---|\n")
		for _, s := range sources {
			fmt.Fprintf(&b, "| `%s` | %d |\n", s.Name, s.Lines)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Key Flows\n\n")
	b.WriteString("[TBD — main user-facing flows in this package]\n\n")

	b.WriteString("## Cross-cutting Patterns\n\n")
	b.WriteString("[TBD — patterns observed across this package's source]\n\n")

	b.WriteString("## Non-obvious Choices\n\n")
	b.WriteString("[TBD — design decisions not obvious from reading the code]\n\n")

	b.WriteString("<!-- sources -->\n")
	for _, s := range sources {
		fmt.Fprintf(&b, "- %s\n", s.RelPath)
	}
	return b.String()
}

// gitkeepContent is the placeholder that keeps
// <cwd>/wiki/modules/ tracked by git when no module files
// are generated (a non-Go repo with /wiki init would land
// here). Empty content is conventional — git tracks the
// file's existence, not its bytes.
const gitkeepContent = ""
