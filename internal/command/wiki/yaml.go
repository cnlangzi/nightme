package wiki

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// wikiYml is the in-memory representation of <cwd>/wiki.yml.
//
// v0 schema:
//
//	version:    int
//	last_commit:   string | null   (sha of the wiki's last overall apply)
//	agent:      string | null     (LLM agent used on last apply)
//	include:    []string          (force-include paths, see ignore.go)
//	modules:    []moduleYml       (per-module bookkeeping)
//	pending:    []pendingEntry    (incremental plan produced by Plan phase)
//
// Pointer fields distinguish "absent" from "explicitly null"
// on round-trip.
type wikiYml struct {
	Version    int            `yaml:"version"`
	LastCommit *string        `yaml:"last_commit"`
	Agent      *string        `yaml:"agent"`
	Include    []string       `yaml:"include"`
	Modules    []moduleYml    `yaml:"modules"`
	Pending    []pendingEntry `yaml:"pending,omitempty"`
}

// moduleYml is one entry in wikiYml.Modules.
//
// LastSHA is the git HEAD SHA at the time the module's wiki
// file was last written by Apply (either stub or LLM). Plan
// uses this to compute the diff vs current HEAD and decide
// whether the module needs updating.
//
// Removed marks a path that no longer exists in source.
// Removed modules' wiki files are deleted by Apply; the yml
// entry stays as an audit record (allows future re-introduction
// without surprises).
type moduleYml struct {
	Path     string  `yaml:"path"`
	File     string  `yaml:"file"`
	Language string  `yaml:"language,omitempty"`
	LastSHA  *string `yaml:"last_sha"`
	Removed  bool    `yaml:"removed,omitempty"`
}

// pendingEntry is one item in the incremental plan produced
// by Plan and consumed by Apply. Status semantics:
//
//   - pending   — Apply has not touched this yet
//   - in_progress — Apply started but did not finish (process crash / context cancel)
//   - done      — Apply finished (stub or LLM wrote content + module.LastSHA updated)
//   - failed    — Apply tried but errored (error field populated, retained for retry)
//
// Apply resumes from non-done entries on subsequent runs, so
// `failed` items get retried automatically without losing
// the rest of the plan.
type pendingEntry struct {
	Path         string   `yaml:"path"`
	Action       string   `yaml:"action"` // regenerate | new | delete
	Reason       string   `yaml:"reason"`
	FilesChanged []string `yaml:"files_changed,omitempty"`
	Status       string   `yaml:"status"`
	Error        string   `yaml:"error,omitempty"`
}

// Pending status constants — string literals stay in YAML so
// humans can read the file; we keep the constants here so
// writers don't typo.
const (
	pendingStatusPending     = "pending"
	pendingStatusInProgress = "in_progress"
	pendingStatusDone        = "done"
	pendingStatusFailed      = "failed"

	pendingActionRegenerate = "regenerate"
	pendingActionNew        = "new"
	pendingActionDelete     = "delete"
)

// parseWikiYml decodes wiki.yml data into the structured
// representation. yaml.v3 errors are surfaced verbatim.
func parseWikiYml(data []byte) (*wikiYml, error) {
	var y wikiYml
	if err := yaml.Unmarshal(data, &y); err != nil {
		return nil, fmt.Errorf("parse wiki.yml: %w", err)
	}
	return &y, nil
}

// encodeWikiYml renders y as canonical YAML. Output is
// deterministic for stable diffs.
func encodeWikiYml(y *wikiYml) ([]byte, error) {
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