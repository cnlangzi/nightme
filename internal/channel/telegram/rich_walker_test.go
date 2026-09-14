package telegram

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWalker_Empty(t *testing.T) {
	if _, ok := markdownToRichBlocks(""); ok {
		t.Fatal("empty input should return ok=false")
	}
}

func TestWalker_Paragraph(t *testing.T) {
	out, ok := markdownToRichBlocks("hello world")
	if !ok {
		t.Fatal("plain paragraph should succeed")
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(out), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0]["type"] != "paragraph" {
		t.Errorf("block 0 type=%v, want paragraph", blocks[0]["type"])
	}
	if blocks[0]["text"] != "hello world" {
		t.Errorf("block 0 text=%v, want 'hello world' (plain string when no entities)", blocks[0]["text"])
	}
}

func TestWalker_Heading_AllLevels(t *testing.T) {
	for level := 1; level <= 6; level++ {
		in := strings.Repeat("#", level) + " Title"
		out, ok := markdownToRichBlocks(in)
		if !ok {
			t.Fatalf("h%d: walker failed", level)
		}
		var blocks []map[string]any
		if err := json.Unmarshal([]byte(out), &blocks); err != nil {
			t.Fatalf("h%d: invalid JSON: %v", level, err)
		}
		if len(blocks) != 1 {
			t.Fatalf("h%d: expected 1 block", level)
		}
		if blocks[0]["type"] != "heading" {
			t.Errorf("h%d: type=%v", level, blocks[0]["type"])
		}
		if size, _ := blocks[0]["size"].(float64); int(size) != level {
			t.Errorf("h%d: size=%v, want %d", level, blocks[0]["size"], level)
		}
	}
}

func TestWalker_FencedCode(t *testing.T) {
	in := "```go\nfmt.Println(\"hi\")\n```"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("fenced code should succeed")
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(out), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0]["type"] != "pre" {
		t.Errorf("type=%v, want pre", blocks[0]["type"])
	}
	if blocks[0]["language"] != "go" {
		t.Errorf("language=%v, want go", blocks[0]["language"])
	}
	if blocks[0]["text"] != "fmt.Println(\"hi\")" {
		t.Errorf("text=%v", blocks[0]["text"])
	}
}

func TestWalker_FencedCode_NoLang(t *testing.T) {
	out, ok := markdownToRichBlocks("```\nplain text\n```")
	if !ok {
		t.Fatal("fenced code without lang should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if _, hasLang := blocks[0]["language"]; hasLang {
		t.Errorf("no-lang fence should omit language field, got %v", blocks[0]["language"])
	}
}

func TestWalker_BulletList(t *testing.T) {
	in := "- a\n- b\n- c"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("bullet list should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	list := blocks[0]
	if list["type"] != "list" {
		t.Fatalf("type=%v", list["type"])
	}
	items, _ := list["items"].([]any)
	if len(items) != 3 {
		t.Errorf("items count = %d, want 3", len(items))
	}
}

func TestWalker_Blockquote(t *testing.T) {
	in := "> quoted text\n> more quoted"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("blockquote should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "blockquote" {
		t.Fatalf("expected 1 blockquote, got %d blocks: %+v", len(blocks), blocks)
	}
	inner, _ := blocks[0]["blocks"].([]any)
	if len(inner) != 1 {
		t.Fatalf("blockquote should have 1 inner block, got %d", len(inner))
	}
}

func TestWalker_Divider(t *testing.T) {
	out, ok := markdownToRichBlocks("---")
	if !ok {
		t.Fatal("divider should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if blocks[0]["type"] != "divider" {
		t.Errorf("type=%v, want divider", blocks[0]["type"])
	}
}

func TestWalker_InlineEntities_Bold(t *testing.T) {
	out, ok := markdownToRichBlocks("**bold** plain")
	if !ok {
		t.Fatal("bold inline should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	text := blocks[0]["text"]
	if _, ok := text.(string); ok {
		t.Fatalf("text should be array (entities present), got string: %v", text)
	}
	arr, _ := text.([]any)
	if len(arr) != 2 {
		t.Fatalf("expected 2 elements (bold entity + plain), got %d: %v", len(arr), arr)
	}
	first, _ := arr[0].(map[string]any)
	if first["type"] != "bold" {
		t.Errorf("first entity type=%v, want bold", first["type"])
	}
}

func TestWalker_InlineEntities_Italic(t *testing.T) {
	out, ok := markdownToRichBlocks("_italic_ end")
	if !ok {
		t.Fatal("italic should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	text := blocks[0]["text"]
	arr, _ := text.([]any)
	if len(arr) == 0 {
		t.Fatalf("empty array")
	}
	first, _ := arr[0].(map[string]any)
	if first["type"] != "italic" {
		t.Errorf("first entity type=%v, want italic", first["type"])
	}
}

func TestWalker_InlineEntities_Code(t *testing.T) {
	out, ok := markdownToRichBlocks("use `fmt.Println` here")
	if !ok {
		t.Fatal("inline code should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	text := blocks[0]["text"]
	arr, _ := text.([]any)
	found := false
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m["type"] == "code" {
			found = true
			if m["text"] != "fmt.Println" {
				t.Errorf("code text=%v", m["text"])
			}
		}
	}
	if !found {
		t.Fatalf("expected code entity in array, got %v", arr)
	}
}

func TestWalker_InlineEntities_URL(t *testing.T) {
	out, ok := markdownToRichBlocks("see [docs](https://example.com) here")
	if !ok {
		t.Fatal("url should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	text := blocks[0]["text"]
	arr, _ := text.([]any)
	found := false
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m["type"] == "url" {
			found = true
			if m["text"] != "docs" || m["url"] != "https://example.com" {
				t.Errorf("url entity=%v", m)
			}
		}
	}
	if !found {
		t.Fatalf("expected url entity, got %v", arr)
	}
}

func TestWalker_InlineEntities_PlainStringWhenNoEntities(t *testing.T) {
	out, ok := markdownToRichBlocks("plain text no formatting")
	if !ok {
		t.Fatal("plain should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	text := blocks[0]["text"]
	if _, ok := text.(string); !ok {
		t.Errorf("text without entities should be string, got %T: %v", text, text)
	}
}

func TestWalker_Fallback_OrderedList(t *testing.T) {
	// Ordered lists currently fall back (ok=false) — see walker
	// comments. Caller should use L1 markdown path.
	_, ok := markdownToRichBlocks("1. one\n2. two")
	if ok {
		t.Fatal("ordered list should fall back to L1 markdown (ok=false)")
	}
}

func TestWalker_Fallback_Table(t *testing.T) {
	_, ok := markdownToRichBlocks("| A | B |\n|---|---|\n| 1 | 2 |")
	if ok {
		t.Fatal("table should fall back to L1 markdown")
	}
}

func TestWalker_Fallback_Footnote(t *testing.T) {
	_, ok := markdownToRichBlocks("text[^1] more")
	if ok {
		t.Fatal("footnote ref should fall back")
	}
}

func TestWalker_Fallback_RawHTML(t *testing.T) {
	_, ok := markdownToRichBlocks("paragraph with <raw>html</raw>")
	if ok {
		t.Fatal("raw HTML should fall back")
	}
}

func TestWalker_Fallback_Image(t *testing.T) {
	_, ok := markdownToRichBlocks("text ![alt](https://x.jpg)")
	if ok {
		t.Fatal("image ref should fall back")
	}
}

func TestWalker_Fallback_UnterminatedFence(t *testing.T) {
	_, ok := markdownToRichBlocks("```\nunterminated")
	if ok {
		t.Fatal("unterminated fence should fall back")
	}
}

func TestWalker_CharCap(t *testing.T) {
	// Just over the cap → fall back.
	big := strings.Repeat("x", richWalkerCharCap+1)
	if _, ok := markdownToRichMarkdown(big); ok {
		// note: markdownToRichMarkdown doesn't exist; this
		// exercises markdownToRichBlocks (different fn).
	}
	if _, ok := markdownToRichBlocks(big); ok {
		t.Fatal("over-cap input should fall back")
	}
}

func TestWalker_MixedBlocks(t *testing.T) {
	in := `# Title

First paragraph.

## Subsection

- item 1
- item 2

` + "```go\ncode\n```\n\n" + `> quote`
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("mixed blocks should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	// 1 heading + 1 paragraph + 1 heading + 1 list + 1 pre + 1 blockquote = 6
	if len(blocks) != 6 {
		t.Fatalf("expected 6 blocks, got %d: %v", len(blocks), blocks)
	}
	types := []string{}
	for _, b := range blocks {
		types = append(types, b["type"].(string))
	}
	want := []string{"heading", "paragraph", "heading", "list", "pre", "blockquote"}
	for i, w := range want {
		if types[i] != w {
			t.Errorf("block %d type=%s, want %s", i, types[i], w)
		}
	}
}

// stub function for the CharCap test (which incorrectly references
// markdownToRichMarkdown in an earlier draft).
func markdownToRichMarkdown(rawMD string) (string, bool) { return markdownToRichBlocks(rawMD) }
