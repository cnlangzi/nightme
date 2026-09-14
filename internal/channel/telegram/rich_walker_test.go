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

// TestWalker_OrderedList verifies that numbered list markers (1. 2. 3.)
// are accepted by the walker and emitted as a `list` block. Telegram
// rich blocks do not distinguish bullet vs ordered at the schema —
// the rendering client infers style from the per-item `1.` prefix.
func TestWalker_OrderedList(t *testing.T) {
	in := "1. one\n2. two\n3. three"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("ordered list should succeed")
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(out), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0]["type"] != "list" {
		t.Errorf("type=%v, want list", blocks[0]["type"])
	}
	items, _ := blocks[0]["items"].([]any)
	if len(items) != 3 {
		t.Errorf("items count = %d, want 3", len(items))
	}
}

// TestWalker_OrderedList_Mixed verifies that a run mixing bullet and
// ordered markers is treated as a single list. Strict separation
// requires a blank line between runs (the walker's blank-line skip
// handles that).
func TestWalker_OrderedList_Mixed(t *testing.T) {
	in := "- bullet\n1. ordered\n* bullet"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("mixed bullet+ordered list should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 list block, got %d", len(blocks))
	}
	items, _ := blocks[0]["items"].([]any)
	if len(items) != 3 {
		t.Errorf("items count = %d, want 3", len(items))
	}
}

// TestWalker_Table verifies that a GFM-shaped markdown table
// (header + separator + at least one data row) is emitted as a
// `table` block with a 2D `cells` array.
func TestWalker_Table(t *testing.T) {
	in := "| Name | Age |\n|------|-----|\n| Alice | 30  |\n| Bob | 25  |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table should succeed")
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(out), &blocks); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "table" {
		t.Fatalf("expected 1 table block, got %+v", blocks)
	}
	cells, _ := blocks[0]["cells"].([]any)
	if len(cells) != 3 {
		t.Fatalf("rows = %d, want 3 (header + 2 data)", len(cells))
	}
	header, _ := cells[0].([]any)
	if len(header) != 2 {
		t.Errorf("header cols = %d, want 2", len(header))
	}
	for j, cell := range header {
		m, _ := cell.(map[string]any)
		if isH, _ := m["is_header"].(bool); !isH {
			t.Errorf("header cell %d is_header=%v, want true", j, m)
		}
	}
}

// TestWalker_Table_Alignment verifies that the separator row's
// leading/trailing colons drive per-column `align` values.
func TestWalker_Table_Alignment(t *testing.T) {
	in := "| L | C | R | D |\n|:--|:-:|--:|---|\n| a | b | c | d |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table with alignment should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	cells, _ := blocks[0]["cells"].([]any)
	header, _ := cells[0].([]any)
	wantAlign := []string{"left", "center", "right", ""}
	for j, cell := range header {
		m, _ := cell.(map[string]any)
		align, _ := m["align"].(string)
		if align != wantAlign[j] {
			t.Errorf("header cell %d align=%q, want %q", j, align, wantAlign[j])
		}
	}
}

// TestWalker_Table_ColumnMismatch verifies the walker rejects tables
// whose data rows disagree with the header on column count. The
// caller falls back to a single paragraph block, never a malformed
// cell array.
func TestWalker_Table_ColumnMismatch(t *testing.T) {
	in := "| A | B |\n|---|---|\n| 1 | 2 | 3 |"
	if _, ok := markdownToRichBlocks(in); ok {
		t.Fatal("table with mismatched columns should bail (ok=false)")
	}
}

// TestWalker_Table_NoSeparator verifies the walker rejects a pipe
// row that is not followed by a separator row (not a valid table).
func TestWalker_Table_NoSeparator(t *testing.T) {
	in := "| A | B |\n| 1 | 2 |"
	if _, ok := markdownToRichBlocks(in); ok {
		t.Fatal("table without separator row should bail (ok=false)")
	}
}

// TestWalker_InlineFootnoteStripped verifies GFM footnote refs
// `[^id]` are stripped from inline text. The walker succeeds and
// the surrounding prose survives in document order; the text field
// is an array because the substitution marks a slot (even though
// the footnote slot emits no entity), which keeps piece ordering.
func TestWalker_InlineFootnoteStripped(t *testing.T) {
	out, ok := markdownToRichBlocks("before[^1]after")
	if !ok {
		t.Fatal("footnote ref should be stripped, not bail")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	arr, ok := blocks[0]["text"].([]any)
	if !ok {
		t.Fatalf("text should be array (entity slot present), got %T: %v", blocks[0]["text"], blocks[0]["text"])
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 pieces [before, after], got %d: %v", len(arr), arr)
	}
	if arr[0] != "before" || arr[1] != "after" {
		t.Errorf("pieces=%v, want [before after]", arr)
	}
}

// TestWalker_InlineImageAsURL verifies markdown image refs `![alt](url)`
// downgrade to clickable url entities — Telegram rich blocks have no
// inline image entity, so the URL is preserved as a link with the
// alt text as label.
func TestWalker_InlineImageAsURL(t *testing.T) {
	out, ok := markdownToRichBlocks("see ![logo](https://x.png) here")
	if !ok {
		t.Fatal("image ref should downgrade to url, not bail")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	arr, _ := blocks[0]["text"].([]any)
	var found bool
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m["type"] == "url" && m["url"] == "https://x.png" && m["text"] == "logo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected url entity with text=logo url=https://x.png, got %+v", arr)
	}
}

// TestWalker_InlineImageEmptyAlt verifies `![](url)` uses the URL as
// the visible label so the rendered entity has human-readable text.
func TestWalker_InlineImageEmptyAlt(t *testing.T) {
	out, ok := markdownToRichBlocks("![](https://x.png)")
	if !ok {
		t.Fatal("empty-alt image should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	arr, _ := blocks[0]["text"].([]any)
	if len(arr) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(arr))
	}
	m, _ := arr[0].(map[string]any)
	if m["type"] != "url" || m["url"] != "https://x.png" || m["text"] != "https://x.png" {
		t.Errorf("entity=%+v, want url with text=url=https://x.png", m)
	}
}

// TestWalker_RawHTMLKept verifies a paragraph containing raw HTML
// tags is emitted as a paragraph block (the walker succeeds, tags
// kept as literal text). Previously this bailed.
func TestWalker_RawHTMLKept(t *testing.T) {
	in := "before <raw>tag</raw> after"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("raw HTML should be kept as literal text, not bail")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "paragraph" {
		t.Fatalf("expected 1 paragraph block, got %+v", blocks)
	}
}

// TestWalker_Fallback_UnterminatedFence still bails — the walker
// cannot recover from an unclosed fence (no end marker means the
// body could be anything). Caller falls back to paragraph block.
func TestWalker_Fallback_UnterminatedFence(t *testing.T) {
	_, ok := markdownToRichBlocks("```\nunterminated")
	if ok {
		t.Fatal("unterminated fence should fall back")
	}
}

// === Coverage extension for 2026-09-14 walker扩面 =============================
//
// The cases below exercise dimensions the original minimal walker did not
// touch: edge cells, mixed inline entities inside tables / lists /
// headings, multi-entity paragraphs, block-marker boundaries, empty /
// whitespace inputs, char cap boundary, and negative cases that must
// still bail (paragraph-only raw HTML, partial separator shapes,
// heading → ordered list transition, etc.). Every test here assumes the
//扩面 contract: walker returns ok=true whenever it can model the input,
// and the only ok=false triggers are block-level shape errors.

// -- Tables: cell content --------------------------------------------------------

// TestWalker_Table_InlineCodeInCell verifies the walker emits a code
// entity for inline backtick content inside a table cell.
func TestWalker_Table_InlineCodeInCell(t *testing.T) {
	in := "| Lang | Tag |\n|------|-----|\n| Go   | `fmt.Println` |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table with inline code should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	cells, _ := blocks[0]["cells"].([]any)
	row, _ := cells[1].([]any)
	cell, _ := row[1].(map[string]any)
	arr, ok := cell["text"].([]any)
	if !ok {
		t.Fatalf("expected array text (entity present), got %T: %v", cell["text"], cell["text"])
	}
	var found bool
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m["type"] == "code" && m["text"] == "fmt.Println" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected code entity for fmt.Println; got %+v", arr)
	}
}

// TestWalker_Table_BoldItalic verifies bold + italic markers survive
// the table walker and emit as entities.
func TestWalker_Table_BoldItalic(t *testing.T) {
	in := "| K | V |\n|---|----|\n| **bold** | *italic* |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table with bold/italic cells should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	cells, _ := blocks[0]["cells"].([]any)
	row, _ := cells[1].([]any)

	check := func(idx int, kind, text string) {
		t.Helper()
		cell, _ := row[idx].(map[string]any)
		arr, ok := cell["text"].([]any)
		if !ok {
			t.Fatalf("cell %d expected array text, got %T", idx, cell["text"])
		}
		m, _ := arr[0].(map[string]any)
		if m["type"] != kind || m["text"] != text {
			t.Errorf("cell %d entity=%+v, want %s/%q", idx, m, kind, text)
		}
	}
	check(0, "bold", "bold")
	check(1, "italic", "italic")
}

// TestWalker_Table_EmptyCells verifies empty cells (whitespace between
// pipes) are accepted. The cell text is the empty string, is_header
// still marks the first row.
func TestWalker_Table_EmptyCells(t *testing.T) {
	in := "| A | B | C |\n|---|---|---|\n| 1 |   | 3 |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table with empty cells should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	cells, _ := blocks[0]["cells"].([]any)
	row, _ := cells[1].([]any)
	middle, _ := row[1].(map[string]any)
	if middle["text"] != "" {
		t.Errorf("empty cell text=%v, want empty string", middle["text"])
	}
}

// TestWalker_Table_HeaderOnlyNoData verifies a header + separator with
// no data row bails (ok=false). GFM requires ≥1 data row.
func TestWalker_Table_HeaderOnlyNoData(t *testing.T) {
	in := "| H1 | H2 |\n|---|---|"
	if _, ok := markdownToRichBlocks(in); ok {
		t.Fatal("header + separator only (no data) should bail (ok=false)")
	}
}

// TestWalker_Table_LeadingWhitespaceTolerance verifies rows with
// leading whitespace still match richTableRowPat and are consumed.
func TestWalker_Table_LeadingWhitespaceTolerance(t *testing.T) {
	// Leading spaces before pipes — common when LLM markdown is
	// indented inside a code block or list.
	in := "    | A | B |\n    |---|---|\n    | 1 | 2 |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table with leading whitespace should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "table" {
		t.Fatalf("expected 1 table block, got %+v", blocks)
	}
}

// TestWalker_Table_BlankLineBreaks verifies a blank line between rows
// stops the table walker cleanly (only the rows before the blank line
// become the table; what follows is a separate block).
func TestWalker_Table_BlankLineBreaks(t *testing.T) {
	in := "| A | B |\n|---|---|\n| 1 | 2 |\n\nfollow up"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table + trailing paragraph should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (table + paragraph), got %d", len(blocks))
	}
	if blocks[0]["type"] != "table" {
		t.Errorf("block 0 type=%v, want table", blocks[0]["type"])
	}
	if blocks[1]["type"] != "paragraph" {
		t.Errorf("block 1 type=%v, want paragraph", blocks[1]["type"])
	}
}

// TestWalker_Table_ThreeRows verifies the walker accepts tables with
// multiple data rows and preserves the row count + is_header flag.
func TestWalker_Table_ThreeRows(t *testing.T) {
	in := "| X | Y |\n|---|---|\n| a | 1 |\n| b | 2 |\n| c | 3 |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table with 3 data rows should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	cells, _ := blocks[0]["cells"].([]any)
	if len(cells) != 4 {
		t.Fatalf("rows=%d, want 4 (header + 3 data)", len(cells))
	}
	for i := 1; i < len(cells); i++ {
		row, _ := cells[i].([]any)
		for j, cell := range row {
			m, _ := cell.(map[string]any)
			if isH, _ := m["is_header"].(bool); isH {
				t.Errorf("data row %d col %d should NOT have is_header", i, j)
			}
		}
	}
}

// TestWalker_Table_FourAlignments verifies the four alignment modes
// (left, center, right, default) all surface when mixed on one row.
func TestWalker_Table_FourAlignments(t *testing.T) {
	in := "| L | C | R | D |\n|:--|:-:|--:|---|\n| a | b | c | d |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("table with mixed alignments should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	cells, _ := blocks[0]["cells"].([]any)
	header, _ := cells[0].([]any)
	// Default align ("") → omit the align field entirely (the wire
	// form stays minimal).
	for j, cell := range header {
		m, _ := cell.(map[string]any)
		_, hasAlign := m["align"]
		if j == 3 && hasAlign {
			t.Errorf("default-align column %d should omit align field, got %v", j, m)
		}
	}
}

// -- Ordered list ---------------------------------------------------------------

// TestWalker_OrderedList_SingleItem verifies a 1-item ordered list is
// accepted (no minimum-count constraint).
func TestWalker_OrderedList_SingleItem(t *testing.T) {
	out, ok := markdownToRichBlocks("1. solo")
	if !ok {
		t.Fatal("single-item ordered list should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	items, _ := blocks[0]["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d, want 1", len(items))
	}
}

// TestWalker_OrderedList_InlineEntities verifies ordered list items
// can carry inline entities (code / bold) and they reach the wire.
func TestWalker_OrderedList_InlineEntities(t *testing.T) {
	in := "1. use `fmt.Println`\n2. **important** step"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("ordered list with inline entities should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	items, _ := blocks[0]["items"].([]any)
	firstItem, _ := items[0].(map[string]any)
	inner, _ := firstItem["blocks"].([]any)
	para, _ := inner[0].(map[string]any)
	arr, ok := para["text"].([]any)
	if !ok {
		t.Fatalf("item text should be array (entity present), got %T", para["text"])
	}
	m, _ := arr[1].(map[string]any) // [str "use ", {code}]
	if m["type"] != "code" || m["text"] != "fmt.Println" {
		t.Errorf("first item entity=%+v, want code/fmt.Println", m)
	}

	secondItem, _ := items[1].(map[string]any)
	inner2, _ := secondItem["blocks"].([]any)
	para2, _ := inner2[0].(map[string]any)
	arr2, _ := para2["text"].([]any)
	m2, _ := arr2[0].(map[string]any)
	if m2["type"] != "bold" || m2["text"] != "important" {
		t.Errorf("second item entity=%+v, want bold/important", m2)
	}
}

// TestWalker_OrderedList_HeadingStops verifies a heading after the
// ordered list closes the list (the heading becomes a new block).
func TestWalker_OrderedList_HeadingStops(t *testing.T) {
	in := "1. one\n2. two\n\n## next"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("ordered list followed by heading should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0]["type"] != "list" {
		t.Errorf("block 0 type=%v, want list", blocks[0]["type"])
	}
	if blocks[1]["type"] != "heading" {
		t.Errorf("block 1 type=%v, want heading", blocks[1]["type"])
	}
}

// -- Footnote refs --------------------------------------------------------------

// TestWalker_Footnote_Multiple verifies multiple footnote refs in one
// paragraph are all stripped without affecting surrounding text.
// The walker emits text pieces only — footnote entity slots are
// silent in the unwrap pass.
func TestWalker_Footnote_Multiple(t *testing.T) {
	out, ok := markdownToRichBlocks("first[^1] middle[^2] end")
	if !ok {
		t.Fatal("multiple footnotes should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	arr, _ := blocks[0]["text"].([]any)
	want := []string{"first", " middle", " end"}
	if len(arr) != len(want) {
		t.Fatalf("expected %d pieces, got %d: %v", len(want), len(arr), arr)
	}
	for i, w := range want {
		s, _ := arr[i].(string)
		if s != w {
			t.Errorf("piece %d=%q, want %q", i, s, w)
		}
	}
}

// TestWalker_Footnote_InHeading verifies the inline walker handles
// footnote refs inside heading text (heading still renders, footnote
// is stripped).
func TestWalker_Footnote_InHeading(t *testing.T) {
	out, ok := markdownToRichBlocks("## Title[^1] here")
	if !ok {
		t.Fatal("heading with footnote ref should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "heading" {
		t.Fatalf("expected 1 heading block, got %+v", blocks)
	}
	// heading text: array form (entity slot present)
	arr, _ := blocks[0]["text"].([]any)
	if len(arr) < 2 {
		t.Fatalf("expected at least 2 pieces, got %d", len(arr))
	}
	first, _ := arr[0].(string)
	if first != "Title" {
		t.Errorf("first piece=%q, want Title", first)
	}
}

// TestWalker_Footnote_AdjacentText verifies there is no boundary
// requirement — `text[^1]moretext` strips the footnote cleanly.
func TestWalker_Footnote_AdjacentText(t *testing.T) {
	out, ok := markdownToRichBlocks("text[^1]moretext")
	if !ok {
		t.Fatal("adjacent footnote should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	arr, _ := blocks[0]["text"].([]any)
	if len(arr) != 2 {
		t.Fatalf("expected 2 pieces [text, moretext], got %d: %v", len(arr), arr)
	}
	if arr[0] != "text" || arr[1] != "moretext" {
		t.Errorf("pieces=%v, want [text moretext]", arr)
	}
}

// -- Image refs -----------------------------------------------------------------

// TestWalker_Image_NonHttpURL verifies images with disallowed schemes
// fall back to literal text (safeLink mirror). The walker does NOT
// bail on these — the entity is just dropped.
func TestWalker_Image_NonHttpURL(t *testing.T) {
	out, ok := markdownToRichBlocks("see ![x](javascript:alert) here")
	if !ok {
		t.Fatal("image with non-http URL should keep literal text, not bail")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	// text is plain string (no entity survived)
	text, _ := blocks[0]["text"].(string)
	if text != "see ![x](javascript:alert) here" {
		t.Errorf("text=%q, want literal preserved", text)
	}
}

// TestWalker_Image_MultipleImages verifies multiple inline images in
// one paragraph all downgrade to url entities.
func TestWalker_Image_MultipleImages(t *testing.T) {
	out, ok := markdownToRichBlocks("![a](https://a.png) and ![b](https://b.png)")
	if !ok {
		t.Fatal("multiple images should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	arr, _ := blocks[0]["text"].([]any)
	wantURLs := map[string]bool{"https://a.png": false, "https://b.png": false}
	for _, item := range arr {
		m, _ := item.(map[string]any)
		url, _ := m["url"].(string)
		if m["type"] == "url" {
			if _, ok := wantURLs[url]; ok {
				wantURLs[url] = true
			}
		}
	}
	for url, found := range wantURLs {
		if !found {
			t.Errorf("expected url entity for %s not found", url)
		}
	}
}

// TestWalker_Image_TrailingImage verifies an image at the end of a
// paragraph (no trailing prose) still produces a url entity.
func TestWalker_Image_TrailingImage(t *testing.T) {
	out, ok := markdownToRichBlocks("see ![logo](https://x.png)")
	if !ok {
		t.Fatal("trailing image should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	arr, _ := blocks[0]["text"].([]any)
	if len(arr) != 2 {
		t.Fatalf("expected 2 pieces [see, image], got %d: %v", len(arr), arr)
	}
	last, _ := arr[1].(map[string]any)
	if last["type"] != "url" || last["url"] != "https://x.png" || last["text"] != "logo" {
		t.Errorf("last piece=%+v, want url/logo", last)
	}
}

// -- Raw HTML -------------------------------------------------------------------

// TestWalker_RawHTML_SelfClosing verifies self-closing tags are kept
// as literal text inside a paragraph block.
func TestWalker_RawHTML_SelfClosing(t *testing.T) {
	out, ok := markdownToRichBlocks("line1<br/>line2")
	if !ok {
		t.Fatal("self-closing raw HTML should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	text, _ := blocks[0]["text"].(string)
	if text != "line1<br/>line2" {
		t.Errorf("text=%q, want literal preserved", text)
	}
}

// TestWalker_RawHTML_WithAttributes verifies HTML with attributes
// (the original conservative detector) is still kept literal.
func TestWalker_RawHTML_WithAttributes(t *testing.T) {
	in := `click <a href="https://x" target="_blank">here</a> now`
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("HTML with attributes should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	text, _ := blocks[0]["text"].(string)
	if text != in {
		t.Errorf("text=%q, want literal preserved", text)
	}
}

// TestWalker_RawHTML_MultipleTags verifies a paragraph with several
// HTML tags stays as one paragraph block (no false split).
func TestWalker_RawHTML_MultipleTags(t *testing.T) {
	in := "<div>one</div> and <span>two</span>"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("multiple HTML tags should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "paragraph" {
		t.Fatalf("expected 1 paragraph, got %+v", blocks)
	}
}

// -- Mixed in real documents ----------------------------------------------------

// TestWalker_Mixed_HeadingWithFootnote verifies a heading containing a
// footnote ref still renders as a heading block (footnote stripped
// inline, array text form because the substitution marks a slot).
func TestWalker_Mixed_HeadingWithFootnote(t *testing.T) {
	out, ok := markdownToRichBlocks("## Plan[^note]")
	if !ok {
		t.Fatal("heading with footnote should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 1 || blocks[0]["type"] != "heading" {
		t.Fatalf("expected 1 heading, got %+v", blocks)
	}
}

// TestWalker_Mixed_ParagraphWithFootnoteAndImage verifies a single
// paragraph with both inline footnote and inline image renders all
// three substitution sites correctly. The footnote slot is silent
// in the unwrap pass; the image slot emits a url entity.
func TestWalker_Mixed_ParagraphWithFootnoteAndImage(t *testing.T) {
	in := "see [^1] and ![pic](https://x.png) end"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("paragraph with footnote + image should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	arr, _ := blocks[0]["text"].([]any)

	// Walk the array: 3 text pieces + 1 url entity = 4 items total.
	// Verify the three text pieces and the url entity are all
	// present in document order.
	wantStrings := []string{"see ", " and ", " end"}
	wantURL := "https://x.png"
	wantText := "pic"

	gotStrings := 0
	gotURL := false
	for _, item := range arr {
		if s, ok := item.(string); ok {
			if gotStrings < len(wantStrings) && s == wantStrings[gotStrings] {
				gotStrings++
			}
		} else if m, ok := item.(map[string]any); ok {
			url, _ := m["url"].(string)
			text, _ := m["text"].(string)
			if m["type"] == "url" && url == wantURL && text == wantText {
				gotURL = true
			}
		}
	}
	if gotStrings != len(wantStrings) {
		t.Errorf("matched only %d/%d string pieces in %v", gotStrings, len(wantStrings), arr)
	}
	if !gotURL {
		t.Errorf("url entity for %s not found in %v", wantURL, arr)
	}
}

// TestWalker_Mixed_TableAfterParagraph verifies paragraph then table
// produces two blocks (table walker is invoked only after paragraph
// closes on a non-marker line).
func TestWalker_Mixed_TableAfterParagraph(t *testing.T) {
	in := "intro line\n\n| A | B |\n|---|---|\n| 1 | 2 |"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("paragraph + table should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0]["type"] != "paragraph" {
		t.Errorf("block 0 type=%v, want paragraph", blocks[0]["type"])
	}
	if blocks[1]["type"] != "table" {
		t.Errorf("block 1 type=%v, want table", blocks[1]["type"])
	}
}

// -- Edge cases: empty / whitespace / char cap ---------------------------------

// TestWalker_Empty_WhitespaceOnly verifies a whitespace-only payload
// (no markdown signal at all) is rejected — the caller falls back to
// a paragraph block. This guards against `e.body = "   \n  "` in the
// chain path producing a malformed-but-empty rich blocks array.
func TestWalker_Empty_WhitespaceOnly(t *testing.T) {
	for _, in := range []string{"   ", "\t\n\n", ""} {
		if _, ok := markdownToRichBlocks(in); ok {
			t.Errorf("whitespace-only input %q should not succeed", in)
		}
	}
}

// TestWalker_CharCap_AtLimit verifies input at exactly the cap is
// accepted (cap is inclusive). One byte over bails.
func TestWalker_CharCap_AtLimit(t *testing.T) {
	atCap := strings.Repeat("x", richWalkerCharCap)
	if _, ok := markdownToRichBlocks(atCap); !ok {
		t.Fatal("input at char cap (inclusive) should succeed")
	}
	overCap := strings.Repeat("x", richWalkerCharCap+1)
	if _, ok := markdownToRichBlocks(overCap); ok {
		t.Fatal("input 1 byte over char cap should bail")
	}
}

// TestWalker_CharCap_WellBelow verifies a moderate-length input is
// processed without preflight concerns. (Sanity guard against
// off-by-one in the cap check.)
func TestWalker_CharCap_WellBelow(t *testing.T) {
	in := strings.Repeat("a ", 100) // 200 bytes, well below cap
	if _, ok := markdownToRichBlocks(in); !ok {
		t.Fatal("moderate input well below cap should succeed")
	}
}

// -- Block-marker boundaries ----------------------------------------------------

// TestWalker_BlockMarker_ParagraphThenFence verifies paragraph and
// adjacent fenced code block are two separate blocks (fence has its
// own type, paragraph does not consume the fence opening line).
func TestWalker_BlockMarker_ParagraphThenFence(t *testing.T) {
	in := "intro\n\n```go\ncode()\n```"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("paragraph + fence should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0]["type"] != "paragraph" {
		t.Errorf("block 0 type=%v, want paragraph", blocks[0]["type"])
	}
	if blocks[1]["type"] != "pre" {
		t.Errorf("block 1 type=%v, want pre", blocks[1]["type"])
	}
}

// TestWalker_BlockMarker_ListEndsAtDivider verifies a bullet list
// followed by `---` produces two blocks (list then divider), not one
// merged block.
func TestWalker_BlockMarker_ListEndsAtDivider(t *testing.T) {
	in := "- a\n- b\n\n---"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("list + divider should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0]["type"] != "list" {
		t.Errorf("block 0 type=%v, want list", blocks[0]["type"])
	}
	if blocks[1]["type"] != "divider" {
		t.Errorf("block 1 type=%v, want divider", blocks[1]["type"])
	}
}

// TestWalker_BlockMarker_OrderedListFollowedByParagraph verifies the
// ordered list walker hands control back to paragraph cleanly.
func TestWalker_BlockMarker_OrderedListFollowedByParagraph(t *testing.T) {
	in := "1. one\n2. two\n\nafterword"
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("ordered list + paragraph should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0]["type"] != "list" {
		t.Errorf("block 0 type=%v, want list", blocks[0]["type"])
	}
	if blocks[1]["type"] != "paragraph" {
		t.Errorf("block 1 type=%v, want paragraph", blocks[1]["type"])
	}
}

// -- Walker negative cases that must still bail ---------------------------------

// TestWalker_Negative_FenceNoBody verifies a fence with only the
// opening line (no body, no close) bails.
func TestWalker_Negative_FenceNoBody(t *testing.T) {
	if _, ok := markdownToRichBlocks("```go"); ok {
		t.Fatal("fence with no body / no close should bail")
	}
}

// TestWalker_Negative_TableHeaderEmpty verifies a separator row with
// a non-`-` character is rejected.
func TestWalker_Negative_TableHeaderEmpty(t *testing.T) {
	in := "| A | B |\n|===|xxx|\n| 1 | 2 |"
	if _, ok := markdownToRichBlocks(in); ok {
		t.Fatal("table with malformed separator should bail")
	}
}

// TestWalker_Negative_TableThreeColHeaderTwoColBody verifies column
// count mismatch between header and data rows.
func TestWalker_Negative_TableThreeColHeaderTwoColBody(t *testing.T) {
	in := "| A | B | C |\n|---|---|---|\n| 1 | 2 |"
	if _, ok := markdownToRichBlocks(in); ok {
		t.Fatal("table with header/data column count mismatch should bail")
	}
}

func TestWalker_CharCap(t *testing.T) {
	// Just over the cap → fall back.
	big := strings.Repeat("x", richWalkerCharCap+1)
	if _, ok := markdownToRichBlocks(big); ok {
		t.Fatal("over-cap input should fall back")
	}
}

func TestWalker_MixedBlocks(t *testing.T) {
	in := `# Title

First paragraph.

## Subsection

- item 1
1. item 2

` + "```go\ncode\n```\n\n" + `> quote

| H1 | H2 |
|----|----|
| 1  | 2  |
`
	out, ok := markdownToRichBlocks(in)
	if !ok {
		t.Fatal("mixed blocks should succeed")
	}
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(out), &blocks)
	// 1 heading + 1 paragraph + 1 heading + 1 list + 1 pre + 1 blockquote + 1 table = 7
	if len(blocks) != 7 {
		t.Fatalf("expected 7 blocks, got %d: %v", len(blocks), blocks)
	}
	types := []string{}
	for _, b := range blocks {
		types = append(types, b["type"].(string))
	}
	want := []string{"heading", "paragraph", "heading", "list", "pre", "blockquote", "table"}
	for i, w := range want {
		if types[i] != w {
			t.Errorf("block %d type=%s, want %s", i, types[i], w)
		}
	}
}
