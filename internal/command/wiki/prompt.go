package wiki

import (
	"fmt"
	"sort"
	"strings"
)

// Prompt construction — Wiki.md §11.3.
//
// The Wiki Prompt is a single ContentBlock of text rendered for
// the Agent on every batch. It carries:
//
//   - the resolved repository root
//   - the plan_sha the Agent is generating against
//   - the page contracts (§6) inline, so the Agent has one
//     source of truth for what a valid page looks like
//   - context budgets (§4.1)
//   - the exact ordered target-page allowlist for this batch
//   - the rules: code authoritative, use existing tools, do
//     NOT edit wiki.yml or generated llms.txt indexes
//   - the required framed final response format

const (
	promptHeaderRepoRoot = "Repository root: %s"
	promptHeaderPlanSha  = "Plan SHA: %s"
	promptHeaderBatchID  = "Job ID: %s"

	promptRulesSection = `## Rules

- Code is the source of truth. Verify every summary against the
  source files before writing.
- Use existing source-reading and file-editing tools (do NOT
  shell out to git or shell commands for tasks the tools cover).
- DO NOT edit wiki.yml. It is command-owned; the runtime rewrites
  it on every batch boundary.
- DO NOT edit generated llms.txt indexes. The /wiki command
  rebuilds those mechanically after every batch.
- Write ONLY the files in the allowlist below. Any other write
  is an output-boundary violation (the runtime refuses to
  finalize the batch).
- Module pages do NOT contain:
    - Complete exported API lists
    - File-by-file line-count tables
    - Long code excerpts
    - Design intent not stated in code
    - Coding rules duplicated from AGENTS.md
    - [TBD] placeholders

## Context budgets (§4.1)

- Root llms.txt: 300–500 tokens
- Directory llms.txt: 200–500 tokens
- Module page: 300–600 tokens
- Quickstart: 500–800 tokens
- Architecture: 800–1,200 tokens

Required fields are not truncated mid-section. Compression goes
through the field priority:

  1. Purpose
  2. Entry Points
  3. Main Flows
  4. Source Anchors
  5. Where to Change
  6. Related Modules
`

	promptFramedFooter = `## Final response format

End your reply with EXACTLY one framed block:

` + "```" + `
WIKI_RESULT_BEGIN
{"job_id":"<the job_id from this prompt>","completed":["wiki/modules/<path>/index.md", ...],"failed":[{"path":"wiki/modules/<path>/index.md","error":"<reason>"}, ...]}
WIKI_RESULT_END
` + "```" + `

Rules:
- "completed" lists paths the Agent wrote AND that pass the
  contract listed above.
- "failed" lists paths the Agent could not produce; include a
  short reason.
- Do NOT include generated page content in the JSON body.
- The block must be the last thing in your reply.
`
)

// BuildBatchPrompt renders the Wiki Prompt for one batch. The
// target list is rendered in the order Apply chose (deepest
// first, parent next, then aggregates).
//
// jobID is the Wiki Job ID (§3.3); it travels through to the
// framed JSON body so Finalize can correlate the response.
func BuildBatchPrompt(repoRoot, planSha, jobID string, batch Batch) string {
	var b strings.Builder
	fmt.Fprintf(&b, promptHeaderRepoRoot+"\n", repoRoot)
	fmt.Fprintf(&b, promptHeaderPlanSha+"\n", planSha)
	fmt.Fprintf(&b, promptHeaderBatchID+"\n\n", jobID)

	// Allowlist — exact wiki-relative paths.
	b.WriteString("## Targets for this batch (exact allowlist)\n\n")
	for _, t := range batch.Targets {
		fmt.Fprintf(&b, "- %s   (%s — %s)\n", t.RelFile, t.KindLabel(), t.Action)
	}
	b.WriteString("\n")

	// Page contracts — one section per target kind. We render
	// each contract exactly once even when multiple targets
	// share a kind, so the Agent has the contract for the
	// kinds in this batch.
	kinds := batchKinds(batch)
	for _, k := range kinds {
		b.WriteString(k.header())
		b.WriteString("\n")
	}

	// Per-target context — file purpose + files_changed (when
	// the target is a module regeneration). This is what makes
	// the batch useful: the Agent knows exactly which files
	// changed since the last write.
	b.WriteString("## Per-target context\n\n")
	for _, t := range batch.Targets {
		writeTargetContext(&b, t)
	}

	b.WriteString(promptRulesSection)
	b.WriteString("\n")
	b.WriteString(promptFramedFooter)

	return b.String()
}

func writeTargetContext(b *strings.Builder, t Target) {
	fmt.Fprintf(b, "### %s\n\n", t.RelFile)
	if t.Purpose != "" {
		fmt.Fprintf(b, "Source path: %s\nPurpose (prior): %s\n\n", t.Path, t.Purpose)
	} else {
		fmt.Fprintf(b, "Source path: %s\n\n", t.Path)
	}
	if len(t.FilesChanged) > 0 {
		b.WriteString("Files changed since last write:\n")
		sort.Strings(t.FilesChanged)
		for _, f := range t.FilesChanged {
			fmt.Fprintf(b, "- %s\n", f)
		}
		b.WriteString("\n")
	}
	if t.Reason != "" {
		fmt.Fprintf(b, "Reason: %s\n\n", t.Reason)
	}
}

func batchKinds(batch Batch) []pageKindLabel {
	seen := map[pageKindLabel]bool{}
	var out []pageKindLabel
	for _, t := range batch.Targets {
		k := t.kindLabel()
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

type pageKindLabel int

const (
	kindModule pageKindLabel = iota
	kindArchitecture
	kindQuickstart
)

func (k pageKindLabel) header() string {
	switch k {
	case kindModule:
		return "## Module page contract\n" + moduleContract
	case kindArchitecture:
		return "## Architecture page contract\n" + architectureContract
	case kindQuickstart:
		return "## Quickstart page contract\n" + quickstartContract
	}
	return ""
}

func (k pageKindLabel) String() string {
	switch k {
	case kindModule:
		return "module"
	case kindArchitecture:
		return "architecture"
	case kindQuickstart:
		return "quickstart"
	}
	return ""
}

// validateFraming parses the Agent's final reply for the
// required WIKI_RESULT_BEGIN / END frame. Returns (result, true)
// on success; (nil, false) on missing/malformed framing.
//
// §11.4: a missing frame is not an automatic failure — the
// caller may still accept files whose content changed since
// before_hash and passed validation.
func validateFraming(reply string, jobID string) (*FramedResult, bool) {
	out, err := ParseFramedResult(reply)
	if err != nil {
		return nil, false
	}
	if out.JobID != jobID {
		return nil, false
	}
	return out, true
}
