// Package yamlfile holds the node-level YAML operations nightme
// uses to edit a configuration file without disturbing the rest of
// it.
//
// The contract every caller depends on: a write touches exactly the
// nodes the caller names. Comments, key order, hand-written hooks
// and keys nightme does not model all survive the round trip
// byte-for-byte. A file that does not parse is reported, never
// rewritten — a broken user config is a bug report, not something
// to overwrite.
//
// The alternative — decoding the file into a struct and re-encoding
// the whole thing — drops comments, materialises every default into
// the file and writes environment overrides back to disk. That shape
// is fine for `nightme config` (an explicit "edit my config" act)
// and wrong for a write triggered from inside a running daemon.
package yamlfile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cnlangzi/nightme/internal/pathutil"
)

// Parse decodes the YAML document in data into its root mapping
// node.
//
// A document with no content — an empty file, a comments-only file,
// a bare `---` — yields a fresh empty mapping; a document whose
// root is something else (a list, a string) is rejected, because
// writing into it would destroy whatever the user meant. path is
// used in error messages only.
func Parse(data []byte, path string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return emptyRoot(data), nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return emptyRoot(data), nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a YAML mapping", path)
	}
	return root, nil
}

// Encode serializes a root mapping back to YAML. The indent is
// pinned to 2 spaces to match the shape users hand-write.
func Encode(root *yaml.Node) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MapValue returns the value node stored under key in a mapping, or
// nil when the mapping has no such key.
func MapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// MapValueOrCreate returns the mapping stored under key, creating an
// empty mapping when the key is absent. It reports an error when the
// key holds something that is not a mapping — writing into it would
// destroy the user's value.
func MapValueOrCreate(m *yaml.Node, key string) (*yaml.Node, error) {
	if v := MapValue(m, key); v != nil {
		if v.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("`%s` is not a mapping", key)
		}
		return v, nil
	}
	created := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		created)
	return created, nil
}

// SetMapValue stores a string scalar under key, creating the key
// when the mapping does not have it yet.
func SetMapValue(m *yaml.Node, key, value string) {
	if v := MapValue(m, key); v != nil {
		v.Kind = yaml.ScalarNode
		v.Tag = "!!str"
		v.Value = value
		v.Content = nil
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

// DeleteMapKey removes key from a mapping. Reports whether the key
// was there.
func DeleteMapKey(m *yaml.Node, key string) bool {
	if m == nil {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// WriteFileAtomic writes data to path via a temp file in the same
// directory followed by a rename. The parent directory is created
// (0700) when missing and the file ends up 0600. A failed write
// leaves the previous file untouched — the user's hand-written
// config never ends up truncated.
func WriteFileAtomic(path string, data []byte) error {
	dir := pathutil.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// emptyRoot builds the fresh mapping a document with no content
// becomes. yaml.v3 drops the comments of a comments-only document —
// they are attached to no node — so the header is recovered from the
// raw bytes and carried over.
func emptyRoot(data []byte) *yaml.Node {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	root.HeadComment = commentHeader(data)
	return root
}

// commentHeader returns the comment lines of a document that holds
// nothing else. It returns "" when the text carries anything that
// is not a comment.
func commentHeader(data []byte) string {
	var lines []string
	for _, raw := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			return ""
		}
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, "\n")
}
