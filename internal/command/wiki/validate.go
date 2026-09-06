package wiki

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Validation contract — Wiki.md §12.
//
// All page validators share the same return shape: (nil, "")
// when valid; otherwise a non-nil error whose Error() is the
// human-readable reason. Callers collect these as validation
// failures and decide whether to accept or retry.

var (
	// Required heading order per page kind. The validators
	// check exact matches (case-sensitive, no leading
	// whitespace) and only allow additional H2/H3 sections
	// AFTER the listed ones (modulo "## Source Anchors" which
	// must always be last in a module page).
	moduleRequired = []string{
		"# ",
		"> ",
		"## Purpose",
		"## Entry Points",
		"## Main Flows",
		"## Related Modules",
		"## Where to Change",
		"## Source Anchors",
	}
	architectureRequired = []string{
		"# Architecture",
		"## Components",
		"## Entry Points",
		"## Main Flows",
		"## Dependency Direction",
		"## Extension Points",
		"## Cross-cutting Concerns",
		"## Source Anchors",
	}
	quickstartRequired = []string{
		"# Quickstart",
		"## Repository Rules",
		"## Development Setup",
		"## Find the Relevant Code",
		"## Make a Focused Change",
		"## Verify",
	}

	// Page size budgets. Wiki.md §4.1 lists these as token
	// targets; we approximate with bytes (1 token ≈ 4 bytes,
	// ±25%) since the LLM-side metering lives in the agent
	// process, not here. Hard limit = 2× upper bound.
	moduleBudget       = 4 * 800 // ~600 tokens × ~4 bytes; cap at 2× upper
	architectureBudget = 4 * 1200
	quickstartBudget   = 4 * 800
)

// pageKind names which contract to apply.
type pageKind int

const (
	pageModule pageKind = iota
	pageArchitecture
	pageQuickstart
)

// validatePage runs the appropriate validator based on kind.
// path is the wiki-relative path; used only for error messages.
func validatePage(kind pageKind, path string, content []byte) error {
	switch kind {
	case pageModule:
		return validateModule(path, content)
	case pageArchitecture:
		return validateArchitecture(content)
	case pageQuickstart:
		return validateQuickstart(content)
	default:
		return fmt.Errorf("unknown page kind")
	}
}

func validateModule(path string, content []byte) error {
	if err := checkHeadingOrder(content, moduleRequired); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := checkSize(path, content, moduleBudget); err != nil {
		return err
	}
	if bytes.Contains(content, []byte("[TBD")) {
		return fmt.Errorf("%s: contains [TBD] placeholder", path)
	}
	// Module pages must have Purpose + Entry Points + Source
	// Anchors. We already enforced heading presence; the body
	// emptiness check is below.
	if !hasNonEmptySection(content, "## Purpose") {
		return fmt.Errorf("%s: Purpose section is empty", path)
	}
	if !hasNonEmptySection(content, "## Entry Points") {
		return fmt.Errorf("%s: Entry Points section is empty", path)
	}
	if !hasNonEmptySection(content, "## Source Anchors") {
		return fmt.Errorf("%s: Source Anchors section is empty", path)
	}
	return nil
}

func validateArchitecture(content []byte) error {
	if err := checkHeadingOrder(content, architectureRequired); err != nil {
		return fmt.Errorf("architecture.md: %w", err)
	}
	if err := checkSize("architecture.md", content, architectureBudget); err != nil {
		return err
	}
	if bytes.Contains(content, []byte("[TBD")) {
		return fmt.Errorf("architecture.md: contains [TBD] placeholder")
	}
	return nil
}

func validateQuickstart(content []byte) error {
	if err := checkHeadingOrder(content, quickstartRequired); err != nil {
		return fmt.Errorf("quickstart.md: %w", err)
	}
	if err := checkSize("quickstart.md", content, quickstartBudget); err != nil {
		return err
	}
	if bytes.Contains(content, []byte("[TBD")) {
		return fmt.Errorf("quickstart.md: contains [TBD] placeholder")
	}
	return nil
}

// checkHeadingOrder scans content for the required headings in
// the listed order. Additional sections may appear after the
// last required one (e.g. "## Notes" after "## Source Anchors"
// is fine in a module page — but the order of the listed ones
// must be preserved).
func checkHeadingOrder(content []byte, required []string) error {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	idx := 0
	for scanner.Scan() {
		line := scanner.Text()
		if idx >= len(required) {
			break
		}
		if line == required[idx] || strings.HasPrefix(line, required[idx]) {
			idx++
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if idx < len(required) {
		return fmt.Errorf("missing or out-of-order heading: %q (got %d of %d required)",
			required[idx], idx, len(required))
	}
	return nil
}

// hasNonEmptySection returns true if content contains the H2
// heading followed by at least one non-blank line before the
// next H2 (or EOF).
func hasNonEmptySection(content []byte, heading string) bool {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	inSection := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, heading) {
			inSection = true
			continue
		}
		if inSection {
			if strings.HasPrefix(line, "## ") {
				return false
			}
			if strings.TrimSpace(line) != "" {
				return true
			}
		}
	}
	return false
}

// checkSize warns (returns nil) for the soft budget and errors
// for 2× hard cap. Wiki.md §4.1 says budgets are soft.
func checkSize(path string, content []byte, budget int) error {
	n := len(content)
	if n > budget*2 {
		return fmt.Errorf("%s: size %d exceeds hard cap %d", path, n, budget*2)
	}
	return nil
}

// --- framed result parsing (§11.3 final response format) ---

const (
	framedBegin = "WIKI_RESULT_BEGIN"
	framedEnd   = "WIKI_RESULT_END"
)

// FramedResult is the decoded body of one Wiki Prompt's final
// response. Paths are validated against the batch allowlist
// before they reach this struct; validation here is purely
// structural.
type FramedResult struct {
	JobID     string         `json:"job_id"`
	Completed []string       `json:"completed"`
	Failed    []FramedFailed `json:"failed"`
}

// FramedFailed describes one path that the Agent could not
// produce. Path must be wiki-relative; Error is free-form.
type FramedFailed struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// ParseFramedResult scans text for the first WIKI_RESULT_BEGIN
// / WIKI_RESULT_END pair and JSON-decodes the body between
// them. Empty / malformed framing is reported as a non-nil
// error so callers can apply the §11.4 fallback (file changed
// + validation passed).
func ParseFramedResult(text string) (*FramedResult, error) {
	_, afterBegin, hasBegin := strings.Cut(text, framedBegin)
	if !hasBegin {
		return nil, errors.New("missing WIKI_RESULT_BEGIN")
	}
	rest, _, hasEnd := strings.Cut(afterBegin, framedEnd)
	if !hasEnd {
		return nil, errors.New("missing WIKI_RESULT_END")
	}
	body := strings.TrimSpace(rest)
	if body == "" {
		return nil, errors.New("empty framed body")
	}
	var out FramedResult
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("framed JSON: %w", err)
	}
	if out.JobID == "" {
		return nil, errors.New("framed result missing job_id")
	}
	return &out, nil
}

// --- output-boundary check (§11.5) ---

// BoundaryReport summarizes the diff between a "before" git
// status snapshot and the current state. Anything outside
// allowlist is a violation.
type BoundaryReport struct {
	Violations []string // repo-relative paths
}

// CheckOutputBoundary inspects the post-batch git status,
// comparing it against the target allowlist. Anything outside
// the allowlist is a boundary violation.
//
// beforeStatus / afterStatus are the output of
// `git status --porcelain` taken at SubmitBatch and after
// PromptEnd, respectively. allowlist is the wiki-relative set
// of paths the Agent was authorized to write (e.g.
// "wiki/modules/internal/bridge/index.md",
// "wiki/architecture.md").
//
// wiki.yml is implicitly allowed because the command persists
// the metadata file inside runBatch (markInProgress / final
// yml writes) and the boundary snapshot window covers that
// write. Per Wiki.md §11.5, the Agent itself MUST NOT touch
// wiki.yml — a deliberate out-of-band Agent write of wiki.yml
// is still flagged because the Agent does not have it in its
// batch allowlist.
//
// llms.txt files (root + per-directory) are rebuilt by the
// command in finalize(), AFTER runBatch, so they are NOT
// inside the boundary snapshot window. An Agent that writes
// an llms.txt during its batch would appear in afterStatus
// and is correctly flagged.
func CheckOutputBoundary(before, after []byte, allowlist []string) BoundaryReport {
	beforeSet := parsePorcelain(before)
	afterSet := parsePorcelain(after)

	allow := make(map[string]bool, len(allowlist)+1)
	for _, p := range allowlist {
		allow[filepath.ToSlash(p)] = true
	}
	// Command-owned writes inside runBatch.
	allow["wiki.yml"] = true

	var rep BoundaryReport
	for path := range afterSet {
		if beforeSet[path] {
			// Modification — same rule as new (must be allowed).
			if !allow[path] {
				rep.Violations = append(rep.Violations, path)
			}
			continue
		}
		// Newly-created path — must be in allowlist.
		if !allow[path] {
			rep.Violations = append(rep.Violations, path)
		}
	}
	// Deletions are acceptable when the path was in the
	// allowlist (a delete action). Unauthorised deletions are
	// also flagged as boundary violations.
	for path := range beforeSet {
		if afterSet[path] {
			continue
		}
		if !allow[path] {
			rep.Violations = append(rep.Violations, path)
		}
	}
	sort.Strings(rep.Violations)
	return rep
}

// parsePorcelain turns `git status --porcelain` output into a
// set of repo-relative paths. Both staged and unstaged entries
// are included — Wiki.md §11.5 cares about the on-disk state.
func parsePorcelain(out []byte) map[string]bool {
	set := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: "XY path" where XY is two chars.
		// Rename / copy entries have "XY old -> new"; we take
		// the RHS.
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, "->") {
			parts := strings.SplitN(path, "->", 2)
			path = strings.TrimSpace(parts[1])
		}
		// Strip leading "./"
		path = strings.TrimPrefix(path, "./")
		path = filepath.ToSlash(path)
		if path != "" {
			set[path] = true
		}
	}
	return set
}

// --- containment check (§11.5) ---

// PathContained reports whether path resolves to a file
// beneath the physical wiki directory. Lexical-cleaned
// comparison is the first filter; resolvedUnder in storage.go
// is the authoritative implementation — this is a thin wrapper
// for callers that already have an absolute repoRoot.
func PathContained(repoRoot, path string) bool {
	return resolvedUnder(filepath.Join(repoRoot, "wiki"), path)
}
