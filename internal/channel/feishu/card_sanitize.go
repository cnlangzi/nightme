// Package feishu — F-39 markdown sanitize pipeline.
//
// SanitizeCardMarkdown is the unified entry point for any markdown content
// entering a Feishu Card 2.0 `tag:"markdown"` element. Apply at every code
// path that ships such content (OutResult via sendResultAsReply; receipt
// footer when reintroducing markdown; future lark_md surfaces).
//
// Source: cc-connect `platform/feishu/feishu.go` (functions
// sanitizeMarkdownURLs, preprocessFeishuMarkdown, stripInvalidFeishuCardImages,
// optimizeFeishuCardMarkdown, protectFencedCodeBlocks / restoreFencedCodeBlocks).
// See https://github.com/chenhg5/cc-connect/blob/main/platform/feishu/feishu.go
// (lines 3017-3104, 5978-6075).
//
// Pipeline order:
//   1. URL sanitize     — non-HTTP(S) link → plain text (avoids 230001 invalid href)
//   2. Fence newline    — ``` must be preceded by newline (otherwise lark_md
//                          treats as inline code, not code block)
//   3. Image strip      — drop `![alt](not-img_xxx)`, keep only Feishu image keys
//   4. Heading demotion — H1→H4, H2-H6→H5 (lark_md heading range is narrower
//                          than CommonMark and we want consistent visual scale)
//   5. Code-block protect — ```block``` content survives all transforms
//   6. Newline compression — 3+ consecutive newlines → 2 (paragraph spacing)
//
// We do NOT escape *, _, `, ~, etc. — both openclaw-lark and cc-connect trust
// Claude Code output as legal CommonMark. Escaping them would break agent
// output that genuinely intends bold/italic/code spans.
package feishu

import (
	"fmt"
	"regexp"
	"strings"
)

// SanitizeCardMarkdown runs the full pipeline. Safe to call on any markdown
// string; the result is valid lark_md content suitable for a Feishu Card 2.0
// `tag:"markdown"` element.
//
// Pure function; no allocations beyond intermediate strings. Idempotent for
// inputs that already passed through.
func SanitizeCardMarkdown(text string) string {
	if text == "" {
		return text
	}
	s := sanitizeMarkdownURLs(text)
	s = stripInvalidFeishuCardImages(s)
	s = optimizeFeishuCardMarkdown(s) // includes code-block protect + heading demotion + newline compression
	s = preprocessFeishuMarkdown(s)   // fence newline last so collapsed newlines don't undo it
	return s
}

// ---------------------------------------------------------------------------
// URL sanitize
// ---------------------------------------------------------------------------

var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)

// sanitizeMarkdownURLs rewrites markdown links whose URL is not a recognized
// HTTP(S) scheme into plain text. Feishu's PATCH / create APIs reject non-HTTP
// href values with error code 230001 ("invalid href"). Strip the URL wrapper
// so the link text is still visible.
//
// Image references `![alt](key)` are skipped — those have a separate
// `stripInvalidFeishuCardImages` pass.
//
// Mirrors cc-connect `feishu.go:3122-3148` sanitizeMarkdownURLs.
func sanitizeMarkdownURLs(md string) string {
	var b strings.Builder
	last := 0
	for _, loc := range mdLinkRe.FindAllStringSubmatchIndex(md, -1) {
		if len(loc) < 6 {
			continue
		}
		start, end := loc[0], loc[1]
		// Skip image references — they look like links to the regex but
		// are handled by stripInvalidFeishuCardImages.
		if start > 0 && md[start-1] == '!' {
			continue
		}
		b.WriteString(md[last:start])
		text := md[loc[2]:loc[3]]
		rawHref := strings.TrimSpace(md[loc[4]:loc[5]])
		if isValidFeishuHref(rawHref) {
			// Keep the link as-is.
			b.WriteString(md[start:end])
		} else {
			// Strip the URL wrapper; keep the link text bare.
			b.WriteString(text)
		}
		last = end
	}
	b.WriteString(md[last:])
	return b.String()
}

// isValidFeishuHref returns true when the URL is acceptable to Feishu.
// Currently only http:// and https:// pass; Feishu rejects other schemes
// with error code 230001 ("invalid href").
//
// Mirrors cc-connect `feishu.go:3114-3118`.
func isValidFeishuHref(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// ---------------------------------------------------------------------------
// Fence newline
// ---------------------------------------------------------------------------

// preprocessFeishuMarkdown ensures every OPENING ``` fence is preceded
// by a newline character. Without this, lark_md parses ``` as inline code
// and breaks the surrounding paragraph into inline code spans.
//
// Only opening fences (even-indexed ```) need the newline; injecting one
// before a closing fence would add a spurious trailing newline into the
// code block's body. (cc-connect's implementation has the same bug;
// F-39 follow-up fixes it.)
//
// Reasonable to run last in the pipeline so the newline compression step
// earlier doesn't strip it.
func preprocessFeishuMarkdown(md string) string {
	var b strings.Builder
	b.Grow(len(md) + 32)
	fenceCount := 0 // counts ``` occurrences; even = opening, odd = closing
	for i := 0; i < len(md); i++ {
		if i > 0 && md[i] == '`' && i+2 < len(md) && md[i+1] == '`' && md[i+2] == '`' {
			if fenceCount%2 == 0 && md[i-1] != '\n' {
				// Opening fence without a leading newline — inject one
				// so lark_md parses it as a code block, not inline code.
				b.WriteByte('\n')
			}
			fenceCount++
		}
		b.WriteByte(md[i])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Image strip
// ---------------------------------------------------------------------------

var feishuCardImagePattern = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// stripInvalidFeishuCardImages removes markdown image references whose URL
// is not a Feishu image_key (must start with `img_`). HTTP URLs, local file
// paths, etc. are dropped entirely; valid `img_xxx` keys are kept verbatim.
//
// Feishu card markdown supports `![alt](img_xxx)` as inline image rendering;
// other refs would render as broken or be rejected by the API.
//
// Mirrors cc-connect `feishu.go:5993-6004` stripInvalidFeishuCardImages.
func stripInvalidFeishuCardImages(text string) string {
	if !strings.Contains(text, "![") {
		return text
	}
	return feishuCardImagePattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := feishuCardImagePattern.FindStringSubmatch(match)
		if len(parts) == 3 && strings.HasPrefix(parts[2], "img_") {
			return match
		}
		return ""
	})
}

// ---------------------------------------------------------------------------
// Code-block protect + heading demotion + newline compression
// ---------------------------------------------------------------------------

const fencedCodeBlockPlaceholder = "\x00NM_FEISHU_CODE_BLOCK_%d\x00"

// protectFencedCodeBlocks replaces every ```...``` block with a placeholder
// so transforms (heading demotion, newline compression) don't touch the
// block contents. The original blocks are returned in parallel and must be
// restored via restoreFencedCodeBlocks.
//
// Tolerates unclosed fences (returns the rest as-is) — same behavior as
// cc-connect `feishu.go:6014-6042`.
func protectFencedCodeBlocks(text string) (string, []string) {
	var blocks []string
	var b strings.Builder
	i := 0
	for i < len(text) {
		start := strings.Index(text[i:], "```")
		if start < 0 {
			b.WriteString(text[i:])
			break
		}
		start += i
		end := strings.Index(text[start+3:], "```")
		if end < 0 {
			// Unclosed fence — keep the rest verbatim.
			b.WriteString(text[i:])
			break
		}
		end += start + 6 // include closing ```
		b.WriteString(text[i:start])
		placeholder := fmt.Sprintf(fencedCodeBlockPlaceholder, len(blocks))
		blocks = append(blocks, text[start:end])
		b.WriteString(placeholder)
		i = end
	}
	return b.String(), blocks
}

// restoreFencedCodeBlocks re-substitutes each placeholder with its original
// fenced block. Inverse of protectFencedCodeBlocks.
func restoreFencedCodeBlocks(text string, blocks []string) string {
	for i, block := range blocks {
		text = strings.ReplaceAll(text, fmt.Sprintf(fencedCodeBlockPlaceholder, i), block)
	}
	return text
}

var (
	h2to6Re = regexp.MustCompile(`(?m)^#{2,6} (.+)$`)
	h1Re    = regexp.MustCompile(`(?m)^# (.+)$`)
	h1to3TriggerRe = regexp.MustCompile(`(?m)^#{1,3} `)
	multiNewlineRe = regexp.MustCompile(`\n{3,}`)
)

// optimizeFeishuCardMarkdown runs:
//   - Heading demotion (only if H1-H3 headings exist):
//       H2-H6 → H5 (##### ...)
//       H1    → H4 (####  ...)
//   - 3+ consecutive newlines → 2 (paragraph spacing preservation)
//
// Code blocks are protected via placeholders before transforms and restored
// after, so ```foo``` / ``` # not a heading ``` is never demoted.
//
// Mirrors cc-connect `feishu.go:6045-6063` optimizeFeishuCardMarkdown.
func optimizeFeishuCardMarkdown(text string) string {
	protected, blocks := protectFencedCodeBlocks(text)
	if h1to3TriggerRe.MatchString(protected) {
		// H2-H6 → H5 first; otherwise H1 would catch as H2 (false positive).
		protected = h2to6Re.ReplaceAllString(protected, "##### $1")
		protected = h1Re.ReplaceAllString(protected, "#### $1")
	}
	protected = multiNewlineRe.ReplaceAllString(protected, "\n\n")
	return restoreFencedCodeBlocks(protected, blocks)
}
