// Package feishu — F-39 tests for the markdown sanitize pipeline.
//
// Mirrors cc-connect's tests for sanitizeMarkdownURLs /
// preprocessFeishuMarkdown / stripInvalidFeishuCardImages /
// optimizeFeishuCardMarkdown (platform/feishu/feishu.go).
package feishu

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// URL sanitize
// ---------------------------------------------------------------------------

func TestSanitize_URL_HTTPKept(t *testing.T) {
	in := "[click](https://example.com)"
	if got := sanitizeMarkdownURLs(in); got != in {
		t.Errorf("http URL should be kept verbatim, got %q want %q", got, in)
	}
}

func TestSanitize_URL_RelativeStrippedToText(t *testing.T) {
	in := "[click](relative/path)"
	got := sanitizeMarkdownURLs(in)
	if got != "click" {
		t.Errorf("non-http URL should drop wrapper, got %q want %q", got, "click")
	}
}

func TestSanitize_URL_FileSchemeStripped(t *testing.T) {
	in := "[file](file:///etc/hosts)"
	got := sanitizeMarkdownURLs(in)
	if got != "file" {
		t.Errorf("file:// URL should be stripped, got %q want %q", got, "file")
	}
}

func TestSanitize_URL_ImageReferenceSkipped(t *testing.T) {
	// Image references look like links to the regex but must be left
	// alone here — stripInvalidFeishuCardImages handles them.
	in := "![alt](https://example.com/img.png)"
	if got := sanitizeMarkdownURLs(in); got != in {
		t.Errorf("image ref should be passed through URL sanitize, got %q", got)
	}
}

func TestSanitize_URL_MultipleMixed(t *testing.T) {
	in := "[ok](https://a.com) and [bad](relative) and [ok2](http://b.com)"
	got := sanitizeMarkdownURLs(in)
	if !strings.Contains(got, "[ok](https://a.com)") {
		t.Errorf("http URL 1 should be kept: %q", got)
	}
	if !strings.Contains(got, "bad") {
		t.Errorf("relative URL should drop wrapper: %q", got)
	}
	if !strings.Contains(got, "[ok2](http://b.com)") {
		t.Errorf("http URL 2 should be kept: %q", got)
	}
	if strings.Contains(got, "[bad](relative)") {
		t.Errorf("bad URL wrapper should be gone: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Fence newline
// ---------------------------------------------------------------------------

func TestSanitize_FenceAlreadyHasNewline_Kept(t *testing.T) {
	in := "paragraph\n\n```go\nfunc() {}\n```\nafter"
	if got := preprocessFeishuMarkdown(in); got != in {
		t.Errorf("already-newlined fence should pass through, got %q", got)
	}
}

func TestSanitize_FenceMissingNewline_Inserted(t *testing.T) {
	in := "paragraph```go\nfunc() {}\n```\nafter"
	got := preprocessFeishuMarkdown(in)
	// The first ``` should now follow a newline.
	if !strings.Contains(got, "```go\n") || !strings.HasPrefix(got[strings.Index(got, "```go")-1:], "\n") {
		// Robust check: ensure a newline directly precedes ```.
		idx := strings.Index(got, "```")
		if idx < 1 || got[idx-1] != '\n' {
			t.Errorf("expected newline before fence, got %q", got)
		}
	}
}

func TestSanitize_FenceAtStart_Unchanged(t *testing.T) {
	in := "```go\nfunc() {}\n```"
	if got := preprocessFeishuMarkdown(in); got != in {
		t.Errorf("fence at start of text should not get prefix newline: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Image strip
// ---------------------------------------------------------------------------

func TestSanitize_Image_ImgPrefixKept(t *testing.T) {
	in := "![alt](img_abc123)"
	if got := stripInvalidFeishuCardImages(in); got != in {
		t.Errorf("img_ prefix should be kept, got %q", got)
	}
}

func TestSanitize_Image_HTTPStripped(t *testing.T) {
	in := "![alt](https://example.com/img.png)"
	got := stripInvalidFeishuCardImages(in)
	if got != "" {
		t.Errorf("http image URL should be stripped entirely, got %q", got)
	}
}

func TestSanitize_Image_LocalPathStripped(t *testing.T) {
	in := "![alt](./local.png)"
	got := stripInvalidFeishuCardImages(in)
	if got != "" {
		t.Errorf("local image URL should be stripped, got %q", got)
	}
}

func TestSanitize_Image_NoImageUnchanged(t *testing.T) {
	in := "plain text with [link](https://a.com) but no image"
	if got := stripInvalidFeishuCardImages(in); got != in {
		t.Errorf("text with no image should pass through, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Heading demotion
// ---------------------------------------------------------------------------

func TestSanitize_HeadingH2ToH5(t *testing.T) {
	in := "## Heading\nbody"
	got := optimizeFeishuCardMarkdown(in)
	if !strings.Contains(got, "##### Heading") {
		t.Errorf("H2 should demote to H5, got %q", got)
	}
}

func TestSanitize_HeadingH3ToH5(t *testing.T) {
	in := "### Heading\nbody"
	got := optimizeFeishuCardMarkdown(in)
	if !strings.Contains(got, "##### Heading") {
		t.Errorf("H3 should demote to H5, got %q", got)
	}
}

func TestSanitize_HeadingH6ToH5(t *testing.T) {
	in := "###### Heading\nbody"
	got := optimizeFeishuCardMarkdown(in)
	if !strings.Contains(got, "##### Heading") {
		t.Errorf("H6 should demote to H5, got %q", got)
	}
}

func TestSanitize_HeadingH1ToH4(t *testing.T) {
	in := "# Top\nbody"
	got := optimizeFeishuCardMarkdown(in)
	if !strings.Contains(got, "#### Top") {
		t.Errorf("H1 should demote to H4, got %q", got)
	}
}

func TestSanitize_HeadingNotTriggeredWhenNoH1H3(t *testing.T) {
	in := "body\n\n**bold** text only"
	got := optimizeFeishuCardMarkdown(in)
	if strings.Contains(got, "#### body") {
		t.Errorf("no headings → no demotion should run, got %q", got)
	}
}

func TestSanitize_Heading_OrderH2BeforeH1(t *testing.T) {
	// Reorder doesn't false-match H1 as H2.
	in := "# Top\n## Sub\nbody"
	got := optimizeFeishuCardMarkdown(in)
	if !strings.Contains(got, "#### Top") || !strings.Contains(got, "##### Sub") {
		t.Errorf("H1→H4 and H2→H5 should both apply, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Code-block protection
// ---------------------------------------------------------------------------

func TestSanitize_CodeBlockProtectedFromHeadingDemotion(t *testing.T) {
	// Heading inside a code block must NOT be demoted.
	in := "paragraph\n\n```go\n# ThisIsInsideCodeBlock\n```\n## RealHeading\nbody"
	got := optimizeFeishuCardMarkdown(in)
	if !strings.Contains(got, "# ThisIsInsideCodeBlock") {
		t.Errorf("code block content should be untouched, got %q", got)
	}
	if !strings.Contains(got, "##### RealHeading") {
		t.Errorf("external H2 still demoted, got %q", got)
	}
}

func TestSanitize_CodeBlockProtectedFromNewlineCompression(t *testing.T) {
	in := "para\n\n```\n\n\n\nend\n```"
	got := optimizeFeishuCardMarkdown(in)
	// Code block preserves its interior (newlines preserved).
	if !strings.Contains(got, "\n\n\nend") {
		t.Errorf("code block interior should keep consecutive newlines, got %q", got)
	}
}

func TestSanitize_UnclosedFence_KeptVerbatim(t *testing.T) {
	// Unclosed ``` should not crash; rest of text passes through.
	in := "before\n```\nno closing fence here\n\n\n\nbody"
	got := optimizeFeishuCardMarkdown(in)
	if !strings.Contains(got, "no closing fence here") {
		t.Errorf("unclosed fence content should remain, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Newline compression (outside code blocks)
// ---------------------------------------------------------------------------

func TestSanitize_Newlines_3PlusCompressedTo2(t *testing.T) {
	in := "para1\n\n\n\npara2"
	got := optimizeFeishuCardMarkdown(in)
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("3+ newlines outside code block should compress to 2, got %q", got)
	}
	if !strings.Contains(got, "para1\n\npara2") {
		t.Errorf("paragraph separation should remain, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Public wrapper
// ---------------------------------------------------------------------------

func TestSanitizeCardMarkdown_Empty(t *testing.T) {
	if got := SanitizeCardMarkdown(""); got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
}

func TestSanitizeCardMarkdown_FullPipeline(t *testing.T) {
	// A representative input that exercises all four steps:
	//  - URL sanitize (relative link dropped, http kept)
	//  - heading demotion (H2 -> H5)
	//  - newline compression (3 -> 2)
	//  - fence newline injection (missing)
	//  - image strip (http image dropped)
	in := "see [docs](https://docs.io) and [x](relative)\n## Setup\n\n![demo](https://demo.io/p.png)\n\n\n\nbody"
	got := SanitizeCardMarkdown(in)
	if !strings.Contains(got, "[docs](https://docs.io)") {
		t.Errorf("http link should survive: %q", got)
	}
	if strings.Contains(got, "[x](relative)") {
		t.Errorf("relative link wrapper should be gone: %q", got)
	}
	if !strings.Contains(got, "##### Setup") {
		t.Errorf("H2 should be demoted: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("extra newlines should be compressed: %q", got)
	}
	if strings.Contains(got, "![demo]") {
		t.Errorf("non-img_ image should be stripped: %q", got)
	}
}
