package wiki

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ModuleMeta is the YAML frontmatter block on every wiki
// module file. See docs/Wiki.md §4.1.
//
// The runtime owns these fields. The Agent writes initial
// values at init time; later regeneration may update last_sha
// and covers.
type ModuleMeta struct {
	Name       string   `yaml:"name"`
	LastSHA    *string  `yaml:"last_sha"`
	PromptVer  int      `yaml:"prompt_version"`
	Covers     []string `yaml:"covers"`
}

// frontmatterDelim is the YAML frontmatter fence. The Agent
// is told to write files with this exact delimiter.
const frontmatterDelim = "---"

// ReadFrontmatter reads path and returns its frontmatter plus
// the body after the closing fence. Returns an error when the
// file has no frontmatter or the frontmatter fails to parse.
func ReadFrontmatter(path string) (ModuleMeta, string, error) {
	var meta ModuleMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return meta, "", err
	}
	fmBytes, body, err := splitFrontmatter(data)
	if err != nil {
		return meta, "", err
	}
	if err := yaml.Unmarshal(fmBytes, &meta); err != nil {
		return meta, "", fmt.Errorf("parse frontmatter in %s: %w", path, err)
	}
	return meta, string(body), nil
}

// WriteFrontmatter writes a module file: frontmatter block
// (closed by ---), a blank line, then body. The file is
// written atomically through a sibling temp file.
//
// Body is written verbatim — the caller is responsible for
// ensuring it already has the H1 and blockquote that the
// frontmatter / contract requires.
func WriteFrontmatter(path string, meta ModuleMeta, body string) error {
	if meta.Name == "" {
		return fmt.Errorf("WriteFrontmatter: empty name in %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	fmBytes, err := yaml.Marshal(&meta)
	if err != nil {
		return fmt.Errorf("marshal frontmatter for %s: %w", path, err)
	}
	var buf bytes.Buffer
	buf.WriteString(frontmatterDelim)
	buf.WriteByte('\n')
	buf.Write(fmBytes)
	buf.WriteString(frontmatterDelim)
	buf.WriteByte('\n')
	buf.WriteByte('\n')
	buf.WriteString(body)
	return atomicWriteFile(path, buf.Bytes())
}

// splitFrontmatter divides a file into the YAML block and the
// body. The file must start with "---" on the first line and
// have a closing "---" line. Returns an error otherwise.
func splitFrontmatter(data []byte) (fm, body []byte, err error) {
	if !bytes.HasPrefix(data, []byte(frontmatterDelim)) {
		return nil, nil, fmt.Errorf("missing leading %q fence", frontmatterDelim)
	}
	rest := data[len(frontmatterDelim):]
	// Skip the newline after the leading fence.
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	} else if len(rest) > 1 && rest[0] == '\r' && rest[1] == '\n' {
		rest = rest[2:]
	}
	// Find the closing fence on its own line.
	idx := indexClosingFence(rest)
	if idx < 0 {
		return nil, nil, fmt.Errorf("missing closing %q fence", frontmatterDelim)
	}
	fm = rest[:idx]
	after := rest[idx+len(frontmatterDelim):]
	if len(after) > 0 && after[0] == '\n' {
		after = after[1:]
	} else if len(after) > 1 && after[0] == '\r' && after[1] == '\n' {
		after = after[2:]
	}
	return fm, after, nil
}

// indexClosingFence returns the byte offset of a "---" line in
// data, or -1 if not found. The match is a line whose entire
// content (after any leading whitespace) is "---".
func indexClosingFence(data []byte) int {
	scan := 0
	for scan < len(data) {
		nl := bytes.IndexByte(data[scan:], '\n')
		var line []byte
		if nl < 0 {
			line = data[scan:]
			scan = len(data)
		} else {
			line = data[scan : scan+nl]
			scan += nl + 1
		}
		trimmed := bytes.TrimLeft(line, " \t\r")
		if bytes.Equal(trimmed, []byte(frontmatterDelim)) {
			return scan - len(line) - 1
		}
	}
	return -1
}
