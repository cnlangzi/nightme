package wiki

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// wikiYml is the in-memory representation of <cwd>/wiki.yml.
// v0 schema:
//
//	version:    int
//	last_commit: string | null   (sha of last /wiki run that produced content)
//	agent:      string | null     (default agent recorded on first successful run)
//	include:    []string          (force-include paths, see ignore.go)
//	modules:    []moduleYml       (per-module bookkeeping)
//
// Pointer fields distinguish "absent" from "explicitly null"
// via yaml.v3's unmarshalling behaviour — important because
// we round-trip the file and want to preserve the user's
// existing field set on rewrite.
type wikiYml struct {
	Version    int         `yaml:"version"`
	LastCommit *string     `yaml:"last_commit"`
	Agent      *string     `yaml:"agent"`
	Include    []string    `yaml:"include"`
	Modules    []moduleYml `yaml:"modules"`
}

// moduleYml is one entry in wikiYml.Modules.
//
// LastSHA stays null until the LLM-driven path (future) sets
// it on commit; /wiki treats null LastSHA as "stub-eligible"
// (overwrite with moduleDocStub) and non-null as "preserve
// what the LLM wrote" (don't touch the wiki file).
//
// Removed is set by reconcile when a previously-listed path
// no longer exists in source. The wiki file is preserved on
// disk; user can `git rm` if they want it gone for good.
// Removed modules are NOT listed in llms.txt.
//
// Language is reserved for the future provider refactor;
// init does not write it (we don't classify by language —
// see ignore.go's "language is the LLM's job").
type moduleYml struct {
	Path     string  `yaml:"path"`
	File     string  `yaml:"file"`
	Language string  `yaml:"language,omitempty"`
	LastSHA  *string `yaml:"last_sha"`
	Removed  bool    `yaml:"removed,omitempty"`
}

// parseWikiYml decodes wiki.yml data into the structured
// representation. yaml.v3 errors are surfaced verbatim —
// callers wrap with file context.
func parseWikiYml(data []byte) (*wikiYml, error) {
	var y wikiYml
	if err := yaml.Unmarshal(data, &y); err != nil {
		return nil, fmt.Errorf("parse wiki.yml: %w", err)
	}
	return &y, nil
}

// encodeWikiYml renders y as canonical YAML. Output is
// deterministic and stable for diffs: yaml.v3 emits the
// struct fields in declaration order, and we use a single
// literal style for scalars to avoid surprises.
//
// v0 callers (only /wiki's reconcile path) pass a fully-
// populated struct; this helper is here so the write side
// is symmetric with parseWikiYml.
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
