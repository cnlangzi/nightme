package wiki

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// wikiYml is the in-memory representation of <repoRoot>/wiki.yml.
//
// Schema v1 (Wiki.md §8). Every field has an explicit purpose; do
// not extend the schema without bumping Version and updating
// parseWikiYml / encodeWikiYml accordingly.
//
//	version:        schema version (1)
//	last_commit:    source HEAD when pending is empty AND every
//	                aggregate is clean
//	plan_sha:       source HEAD used to build the current pending
//	include:        exact path opt-ins for discovery
//	aggregates:     per-aggregate (architecture | quickstart) state
//	modules:        per-module bookkeeping
//	pending:        live Plan output consumed by Apply
type wikiYml struct {
	Version    int                      `yaml:"version"`
	LastCommit *string                  `yaml:"last_commit,omitempty"`
	PlanSha    string                   `yaml:"plan_sha,omitempty"`
	Include    []string                 `yaml:"include"`
	Aggregates map[string]*aggregateYml `yaml:"aggregates"`
	Modules    []moduleYml              `yaml:"modules"`
	Pending    []pendingEntry           `yaml:"pending,omitempty"`
}

// aggregateYml tracks one of the two aggregate pages (architecture
// or quickstart). Keyed by lower-case name in wikiYml.Aggregates.
type aggregateYml struct {
	File       string  `yaml:"file"`
	LastSHA    *string `yaml:"last_sha,omitempty"`
	PromptVer  int     `yaml:"prompt_version"`
	Dirty      bool    `yaml:"dirty"`
	Status     string  `yaml:"status"` // pending | in_progress | done | failed
	BeforeHash *string `yaml:"before_hash,omitempty"`
	Error      string  `yaml:"error,omitempty"`
}

// moduleYml is one entry in wikiYml.Modules. Path is the source
// directory relative to repoRoot; File is the wiki page path
// relative to wiki/ (mirror rule — Path + ".md"). Purpose is the
// short description rendered into parent indexes.
//
// LastSHA is the source HEAD at the time the page was last
// validated by Apply. PromptVer tracks the module prompt version
// at validation time — a mismatch triggers regeneration on the
// next Plan.
type moduleYml struct {
	Path      string  `yaml:"path"`
	File      string  `yaml:"file"`
	Purpose   string  `yaml:"purpose,omitempty"`
	LastSHA   *string `yaml:"last_sha,omitempty"`
	PromptVer int     `yaml:"prompt_version"`
	Removed   bool    `yaml:"removed,omitempty"`
}

// pendingEntry is one item in the incremental plan produced by
// Plan and consumed by Apply. Path is the source module path or
// one of the aggregate names ("architecture" | "quickstart").
//
// Status transitions:
//
//	pending → in_progress → done | failed
//
// failed and in_progress remain on disk for the next invocation
// to retry. done entries are removed by Finalize.
type pendingEntry struct {
	Path         string   `yaml:"path"`
	Action       string   `yaml:"action"` // new | regenerate | delete
	Reason       string   `yaml:"reason"`
	FilesChanged []string `yaml:"files_changed,omitempty"`
	Status       string   `yaml:"status"`
	BeforeHash   *string  `yaml:"before_hash,omitempty"`
	Error        string   `yaml:"error,omitempty"`
}

const (
	// Schema version. Bump on any backward-incompatible change
	// to wikiYml's wire shape.
	SchemaVersion = 1

	// Aggregate names — keys into wikiYml.Aggregates.
	ArchitectureKey = "architecture"
	QuickstartKey   = "quickstart"

	// Page prompt versions. Plan compares these against the
	// stored PromptVer and requests regeneration on mismatch.
	ModulePromptVersion     = 1
	ArchitecturePromptVer   = 1
	QuickstartPromptVersion = 1

	// Pending status constants.
	pendingStatusPending    = "pending"
	pendingStatusInProgress = "in_progress"
	pendingStatusDone       = "done"
	pendingStatusFailed     = "failed"

	// Pending action constants.
	pendingActionNew        = "new"
	pendingActionRegenerate = "regenerate"
	pendingActionDelete     = "delete"
)

// parseWikiYml decodes wiki.yml data. A missing or malformed
// file is reported as an error — the caller is responsible for
// recovery (see loadOrReconcileYml in cmd.go).
func parseWikiYml(data []byte) (*wikiYml, error) {
	var y wikiYml
	if err := yaml.Unmarshal(data, &y); err != nil {
		return nil, fmt.Errorf("parse wiki.yml: %w", err)
	}
	if y.Aggregates == nil {
		y.Aggregates = map[string]*aggregateYml{}
	}
	return &y, nil
}

// encodeWikiYml renders y as deterministic YAML. Output uses
// 2-space indent and is stable across processes.
func encodeWikiYml(y *wikiYml) ([]byte, error) {
	if y.Aggregates == nil {
		y.Aggregates = map[string]*aggregateYml{}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(y); err != nil {
		return nil, fmt.Errorf("encode wiki.yml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ensureAggregate returns the named aggregate, creating an empty
// record if absent. The File field is populated when the entry
// is created so downstream writers can rely on it.
func ensureAggregate(y *wikiYml, name string) *aggregateYml {
	if y.Aggregates == nil {
		y.Aggregates = map[string]*aggregateYml{}
	}
	if a, ok := y.Aggregates[name]; ok && a != nil {
		return a
	}
	a := &aggregateYml{
		File:      name + ".md",
		Status:    pendingStatusPending,
		PromptVer: currentPromptVerFor(name),
	}
	y.Aggregates[name] = a
	return a
}

// currentPromptVerFor returns the canonical prompt version for
// the given aggregate name.
func currentPromptVerFor(name string) int {
	switch name {
	case ArchitectureKey:
		return ArchitecturePromptVer
	case QuickstartKey:
		return QuickstartPromptVersion
	default:
		return 0
	}
}
