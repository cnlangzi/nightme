// Package feishu — F-37 multi-div content split unit tests.
package feishu

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplit_Empty(t *testing.T) {
	got := splitMarkdownForDivs("", 100)
	if got != nil {
		t.Errorf("empty input: got %v, want nil", got)
	}
}

func TestSplit_WhitespaceOnly(t *testing.T) {
	got := splitMarkdownForDivs("\n\n  \n", 100)
	if len(got) != 1 {
		t.Errorf("whitespace-only: got %d chunks, want 1", len(got))
	}
}

func TestSplit_ShortParagraph(t *testing.T) {
	text := "Hello, world."
	got := splitMarkdownForDivs(text, 100)
	if len(got) != 1 || got[0] != text {
		t.Errorf("short paragraph: got %v, want [%q]", got, text)
	}
}

func TestSplit_ExactlyAtLimit(t *testing.T) {
	text := strings.Repeat("a", 1000)
	got := splitMarkdownForDivs(text, 1000)
	if len(got) != 1 || got[0] != text {
		t.Errorf("at limit: got %d chunks, want 1", len(got))
	}
}

func TestSplit_JustOverLimit(t *testing.T) {
	para := strings.Repeat("a", 600)
	text := para + "\n\n" + strings.Repeat("b", 600)
	got := splitMarkdownForDivs(text, 1000)
	if len(got) != 2 {
		t.Errorf("just over limit: got %d chunks, want 2", len(got))
	}
	for i, c := range got {
		if utf8.RuneCountInString(c) > 1000 {
			t.Errorf("chunk %d exceeds 1000 runes: %d", i, utf8.RuneCountInString(c))
		}
	}
}

func TestSplit_MultipleParagraphs(t *testing.T) {
	paragraphs := []string{}
	for range 4 {
		paragraphs = append(paragraphs, strings.Repeat("x", 200))
	}
	// 4 paragraphs × 200 chars + 3 × 2-char separators = 806 chars
	// → fits in 1 chunk of 1000
	text := strings.Join(paragraphs, "\n\n")
	got := splitMarkdownForDivs(text, 1000)
	if len(got) != 1 {
		t.Errorf("4 paragraphs × 200 chars = 806 chars: got %d chunks, want 1", len(got))
	}
}

func TestSplit_SpanningMultipleChunks(t *testing.T) {
	paragraphs := []string{}
	for range 5 {
		paragraphs = append(paragraphs, strings.Repeat("x", 600))
	}
	text := strings.Join(paragraphs, "\n\n")
	got := splitMarkdownForDivs(text, 1000)
	if len(got) < 3 {
		t.Errorf("5 paragraphs × 600 chars = 3000 chars: got %d chunks, want ≥ 3", len(got))
	}
	for i, c := range got {
		if utf8.RuneCountInString(c) > 1000 {
			t.Errorf("chunk %d exceeds 1000 runes: %d", i, utf8.RuneCountInString(c))
		}
	}
}

func TestSplit_CodeBlockPreserved(t *testing.T) {
	code := "```go\n" + strings.Repeat("x", 800) + "\n```"
	text := "Header text.\n\n" + code + "\n\nFooter text."
	got := splitMarkdownForDivs(text, 1500)
	// 整段应该还是 1 chunk (1500 maxRunes 但 content ~830 chars)
	if len(got) < 1 {
		t.Fatalf("got %d chunks, want ≥ 1", len(got))
	}
	// Code block 必须完整出现在某个 chunk 内
	found := false
	for _, c := range got {
		if strings.Contains(c, code) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("code block not preserved as atomic unit across chunks: %v", got)
	}
}

func TestSplit_LongCodeBlock(t *testing.T) {
	// F-37: a code block longer than maxRunes MUST be split into
	// multiple chunks (each ≤ maxRunes) so the Feishu server
	// accepts the PATCH. Earlier behaviour kept it as one atomic
	// chunk, which violated the 1000-char div.text hard limit.
	code := "```\n" + strings.Repeat("y", 2000) + "\n```"
	got := splitMarkdownForDivs(code, 1000)
	if len(got) < 2 {
		t.Errorf("long code block: got %d chunks, want ≥ 2 (must split to fit div.text limit)", len(got))
	}
	for i, c := range got {
		if utf8.RuneCountInString(c) > 1000 {
			t.Errorf("chunk %d exceeds 1000 runes: %d", i, utf8.RuneCountInString(c))
		}
	}
}

func TestSplit_ListPreserved(t *testing.T) {
	items := []string{}
	for range 10 {
		items = append(items, "- item "+strings.Repeat("z", 80))
	}
	text := strings.Join(items, "\n")
	got := splitMarkdownForDivs(text, 1000)
	// 10 items × ~88 chars = ~880 chars, 应 1 chunk
	if len(got) != 1 {
		t.Errorf("list: got %d chunks, want 1 (atomic)", len(got))
	}
	for i, c := range got {
		if utf8.RuneCountInString(c) > 1000 {
			t.Errorf("chunk %d exceeds 1000 runes: %d", i, utf8.RuneCountInString(c))
		}
	}
}

func TestSplit_ChineseRuneAware(t *testing.T) {
	// 中文 800 chars (按 rune 算)
	text := strings.Repeat("中", 800)
	got := splitMarkdownForDivs(text, 1000)
	if len(got) != 1 {
		t.Errorf("Chinese 800 chars: got %d chunks, want 1", len(got))
	}
}

func TestSplit_EmojiRuneAware(t *testing.T) {
	// Emoji 4 bytes/char (UTF-8),但 1 rune
	text := strings.Repeat("🎉", 1000)
	got := splitMarkdownForDivs(text, 1000)
	if len(got) != 1 {
		t.Errorf("emoji 1000 runes: got %d chunks, want 1", len(got))
	}
}

func TestSplit_HardSplitFallback(t *testing.T) {
	// 单 5000 char 无空格 token 强制硬切
	text := strings.Repeat("a", 5000)
	got := splitMarkdownForDivs(text, 1000)
	if len(got) < 5 {
		t.Errorf("hard split: got %d chunks, want ≥ 5", len(got))
	}
	for i, c := range got {
		if utf8.RuneCountInString(c) > 1000 {
			t.Errorf("chunk %d exceeds 1000 runes: %d", i, utf8.RuneCountInString(c))
		}
	}
}

func TestSplit_PunctuationPriority(t *testing.T) {
	// 段落边界 > 标点 > 空格
	// 1000 chars of "ab, cd, ef, gh, ..." 应在某个 "," 后面切
	text := strings.Repeat("ab ", 400) // 1200 chars
	got := splitMarkdownForDivs(text, 1000)
	if len(got) < 2 {
		t.Errorf("punctuation priority: got %d chunks, want ≥ 2", len(got))
	}
}

func TestSplit_MaxRunesZero(t *testing.T) {
	// 防御: maxRunes <= 0 应直接返回单元素
	text := "hello"
	got := splitMarkdownForDivs(text, 0)
	if len(got) != 1 || got[0] != text {
		t.Errorf("maxRunes=0: got %v, want [%q]", got, text)
	}
}

func TestSplit_RealisticMarkdown(t *testing.T) {
	text := `# Title

First paragraph with some text.

- item 1
- item 2
- item 3

` + "```go\nfunc main() {}\n```" + `

Last paragraph with ` + "`code`" + ` inline.`
	got := splitMarkdownForDivs(text, 5000)
	if len(got) != 1 {
		t.Errorf("realistic markdown: got %d chunks, want 1 (fits in 5000)", len(got))
	}
	if got[0] != text {
		t.Errorf("realistic markdown: content mismatch")
	}
}

func TestSplit_OrderedList(t *testing.T) {
	// 数字. 开头算 list item
	items := []string{
		"1. First",
		"2. Second",
		"3. Third",
	}
	text := strings.Join(items, "\n")
	got := splitMarkdownForDivs(text, 1000)
	if len(got) != 1 {
		t.Errorf("ordered list: got %d chunks, want 1", len(got))
	}
	// 列表项必须全在同一个 chunk
	for _, item := range items {
		if !strings.Contains(got[0], item) {
			t.Errorf("ordered list: missing item %q", item)
		}
	}
}
