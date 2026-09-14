package telegram

import (
	"strings"
	"testing"
)

func TestEstimateRichBlocks_Empty(t *testing.T) {
	if got := estimateRichBlocks(""); got != 0 {
		t.Fatalf("empty input: got %d, want 0", got)
	}
}

func TestEstimateRichBlocks_SingleParagraph(t *testing.T) {
	if got := estimateRichBlocks("hello world"); got != 1 {
		t.Fatalf("single line: got %d, want 1", got)
	}
}

func TestEstimateRichBlocks_MultiParagraph(t *testing.T) {
	in := "first paragraph\n\nsecond paragraph\n\nthird paragraph"
	if got := estimateRichBlocks(in); got != 3 {
		t.Fatalf("three paragraphs: got %d, want 3", got)
	}
}

func TestEstimateRichBlocks_Headings(t *testing.T) {
	in := "# H1\n\ntext\n\n## H2\n\ntext\n\n### H3"
	// H1, paragraph, H2, paragraph, H3 = 5
	if got := estimateRichBlocks(in); got != 5 {
		t.Fatalf("headings + paragraphs: got %d, want 5", got)
	}
}

func TestEstimateRichBlocks_Lists(t *testing.T) {
	in := "- a\n- b\n- c"
	if got := estimateRichBlocks(in); got != 3 {
		t.Fatalf("3 bullet items: got %d, want 3", got)
	}
}

func TestEstimateRichBlocks_OrderedList(t *testing.T) {
	in := "1. one\n2. two\n3. three"
	if got := estimateRichBlocks(in); got != 3 {
		t.Fatalf("3 ordered items: got %d, want 3", got)
	}
}

func TestEstimateRichBlocks_FencedCodeBlock(t *testing.T) {
	in := "before\n\n```go\nfmt.Println(\"hi\")\n```\n\nafter"
	// before paragraph + fence block + after paragraph = 3
	if got := estimateRichBlocks(in); got != 3 {
		t.Fatalf("fence with surrounding paragraphs: got %d, want 3", got)
	}
}

func TestEstimateRichBlocks_Blockquote(t *testing.T) {
	in := "> quote line 1\n> quote line 2"
	// 2 quote lines counted as 2 separate blocks (matches server
	// blockquote parser behaviour, which treats consecutive `>`
	// lines as a single block — but each line containing `>` counts).
	if got := estimateRichBlocks(in); got < 1 || got > 3 {
		t.Fatalf("blockquote: got %d, want 1-3", got)
	}
}

func TestEstimateRichBlocks_AtLimit(t *testing.T) {
	// 200 unit "## h\n\np\n\n" entries → 400 blocks (2 per unit: heading + paragraph).
	// 400 = preflight threshold (richBlockCountLimit * 4 / 5). Under it.
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("## h\n\np\n\n")
	}
	if got := estimateRichBlocks(sb.String()); got != 400 {
		t.Fatalf("200-unit input: got %d, want 400", got)
	}
	if got := canUseRichMarkdown(sb.String()); !got {
		t.Fatalf("400-block input should pass preflight (threshold=%d)", blockThresholdForRich())
	}
}

func TestEstimateRichBlocks_OverLimit(t *testing.T) {
	// 201 units → 402 blocks → over threshold (400) → preflight fails.
	var sb strings.Builder
	for i := 0; i < 201; i++ {
		sb.WriteString("## h\n\np\n\n")
	}
	if got := canUseRichMarkdown(sb.String()); got {
		t.Fatalf("402-block input should fail preflight")
	}
}

func TestEstimateRichBlocks_CharLimit(t *testing.T) {
	// One big paragraph of 32K chars exactly → passes.
	big := strings.Repeat("x", richMarkdownCharLimit)
	if !canUseRichMarkdown(big) {
		t.Fatalf("32K-char input should pass char check")
	}
	// 32K + 1 → fails.
	tooBig := strings.Repeat("x", richMarkdownCharLimit+1)
	if canUseRichMarkdown(tooBig) {
		t.Fatalf("32K+1 char input should fail char check")
	}
}

func TestCanUseRichMarkdown_Empty(t *testing.T) {
	if canUseRichMarkdown("") {
		t.Fatal("empty input should fail preflight")
	}
}

func TestBlockThresholdForRich(t *testing.T) {
	// 500 * 4 / 5 = 400. Hard-coded so the relationship between
	// richBlockCountLimit and the preflight margin stays visible
	// (changing one without the other is a regression).
	if got := blockThresholdForRich(); got != 400 {
		t.Fatalf("blockThresholdForRich() = %d, want 400", got)
	}
}
