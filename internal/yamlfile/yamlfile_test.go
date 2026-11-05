package yamlfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_ReadsRootMapping(t *testing.T) {
	root, err := Parse([]byte("lang: zh\nrepos:\n  a/b: {}\n"), "/tmp/x.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if v := MapValue(root, "lang"); v == nil || v.Value != "zh" {
		t.Fatalf("lang = %+v, want zh", v)
	}
}

func TestParse_EmptyDocumentYieldsEmptyMapping(t *testing.T) {
	for _, src := range []string{"", "---\n", "# only a comment\n"} {
		root, err := Parse([]byte(src), "/tmp/x.yml")
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		if root.Kind != 4 { // yaml.MappingNode
			t.Fatalf("Parse(%q) root kind = %v, want mapping", src, root.Kind)
		}
		if len(root.Content) != 0 {
			t.Fatalf("Parse(%q) root has content: %v", src, root.Content)
		}
	}
}

func TestParse_CommentOnlyDocumentKeepsHeader(t *testing.T) {
	root, err := Parse([]byte("# my hooks\n"), "/tmp/x.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(root.HeadComment, "# my hooks") {
		t.Fatalf("HeadComment = %q, want the comment header", root.HeadComment)
	}
}

func TestParse_MalformedDocumentIsAnError(t *testing.T) {
	if _, err := Parse([]byte("lang: [unclosed\n"), "/tmp/x.yml"); err == nil {
		t.Fatal("expected an error for a malformed document")
	}
}

func TestParse_NonMappingRootIsAnError(t *testing.T) {
	if _, err := Parse([]byte("- one\n- two\n"), "/tmp/x.yml"); err == nil {
		t.Fatal("expected an error for a list root")
	}
}

func TestSetMapValue_CreatesAndUpdates(t *testing.T) {
	root, err := Parse([]byte("lang: zh\n"), "/tmp/x.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	SetMapValue(root, "lang", "en")
	if v := MapValue(root, "lang"); v == nil || v.Value != "en" {
		t.Fatalf("lang = %+v, want en", v)
	}
	SetMapValue(root, "agent", "pi")
	if v := MapValue(root, "agent"); v == nil || v.Value != "pi" {
		t.Fatalf("agent = %+v, want pi", v)
	}
}

func TestMapValueOrCreate_CreatesNestedMapping(t *testing.T) {
	root, err := Parse([]byte("lang: zh\n"), "/tmp/x.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	section, err := MapValueOrCreate(root, "review")
	if err != nil {
		t.Fatalf("MapValueOrCreate: %v", err)
	}
	SetMapValue(section, "agent", "pi")

	out, err := Encode(root)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(out), "review:\n  agent: pi") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestMapValueOrCreate_RejectsNonMapping(t *testing.T) {
	root, err := Parse([]byte("review: pi\n"), "/tmp/x.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := MapValueOrCreate(root, "review"); err == nil {
		t.Fatal("expected an error when the key holds a scalar")
	}
}

// TestMapValueOrCreate_ReplacesNullScalar covers the hand-written
// `key:` stub — a key with no value parses as a null scalar. There
// is nothing to lose by turning it into a mapping.
func TestMapValueOrCreate_ReplacesNullScalar(t *testing.T) {
	root, err := Parse([]byte("review:\nprimary: cc\n"), "/tmp/x.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	section, err := MapValueOrCreate(root, "review")
	if err != nil {
		t.Fatalf("MapValueOrCreate: %v", err)
	}
	SetMapValue(section, "agent", "pi")

	out, err := Encode(root)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "review:\n  agent: pi") {
		t.Fatalf("unexpected output:\n%s", got)
	}
	if !strings.Contains(got, "primary: cc") {
		t.Fatalf("the rest of the document was lost:\n%s", got)
	}
}

func TestDeleteMapKey_ReportsRemoval(t *testing.T) {
	root, err := Parse([]byte("a: 1\nb: 2\n"), "/tmp/x.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !DeleteMapKey(root, "a") {
		t.Fatal("DeleteMapKey(a) = false, want true")
	}
	if DeleteMapKey(root, "a") {
		t.Fatal("DeleteMapKey(a) = true on the second call")
	}
	if v := MapValue(root, "b"); v == nil || v.Value != "2" {
		t.Fatalf("b = %+v, want 2", v)
	}
}

func TestEncode_UsesTwoSpaceIndent(t *testing.T) {
	root, err := Parse([]byte(""), "/tmp/x.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	section, err := MapValueOrCreate(root, "review")
	if err != nil {
		t.Fatalf("MapValueOrCreate: %v", err)
	}
	SetMapValue(section, "agent", "pi")

	out, err := Encode(root)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(out), "  agent: pi") {
		t.Fatalf("expected a 2-space indent:\n%s", out)
	}
}

func TestWriteFileAtomic_CreatesParentAndFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "config.yaml")

	if err := WriteFileAtomic(path, []byte("a: 1\n")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "a: 1\n" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %v, want 0600", perm)
	}
}

func TestWriteFileAtomic_ReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("new\n")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new\n" {
		t.Fatalf("content = %q, want new", data)
	}
	// No temp file is left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "config.yaml" {
			t.Errorf("leftover file: %s", e.Name())
		}
	}
}
