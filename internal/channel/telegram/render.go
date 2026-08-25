package telegram

import (
	"errors"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/statusbar"
)

var (
	codeSpanPattern = regexp.MustCompile("`([^`\\n]+)`")
	linkPattern     = regexp.MustCompile("\\[([^\\]]+)\\]\\(([^)]+)\\)")
	boldPattern     = regexp.MustCompile("\\*\\*([^*\\n]+)\\*\\*|__([^_\\n]+)__")
	italicPattern   = regexp.MustCompile("(^|[^*])\\*([^*\\n]+)\\*|(^|[^_])_([^_\\n]+)_")
	spoilerPattern  = regexp.MustCompile("\\|\\|([^|\\n]+)\\|\\|")
	headingPattern  = regexp.MustCompile("^#{1,6}\\s+(.+)$")
	bulletPattern   = regexp.MustCompile("^[-*]\\s+(.+)$")
	orderedPattern  = regexp.MustCompile("^([0-9]+)\\.\\s+(.+)$")
	quotePattern    = regexp.MustCompile("^>\\s*(.*)$")
)

func RenderMarkdown(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	var output strings.Builder
	inCodeBlock := false
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				output.WriteString("</pre>")
			} else {
				output.WriteString("<pre>")
			}
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			output.WriteString(escapeHTML(line))
			if index+1 < len(lines) {
				output.WriteByte('\n')
			}
			continue
		}
		if trimmed == "" {
			output.WriteByte('\n')
			continue
		}
		if matches := headingPattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString("<b>")
			output.WriteString(renderInline(matches[1]))
			output.WriteString("</b>")
		} else if matches := bulletPattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString("• ")
			output.WriteString(renderInline(matches[1]))
		} else if matches := orderedPattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString(matches[1] + ". " + renderInline(matches[2]))
		} else if matches := quotePattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString("<blockquote>")
			output.WriteString(renderInline(matches[1]))
			output.WriteString("</blockquote>")
		} else if trimmed == "---" || trimmed == "***" {
			output.WriteString("────────")
		} else if strings.Count(trimmed, "|") >= 2 && index+1 < len(lines) && isTableSeparator(strings.TrimSpace(lines[index+1])) {
			output.WriteString(renderTable(trimmed))
			index++
		} else {
			output.WriteString(renderInline(trimmed))
		}
		if index+1 < len(lines) {
			output.WriteByte('\n')
		}
	}
	if inCodeBlock {
		output.WriteString("</pre>")
	}
	result := output.String()
	if result == "" {
		return "", nil
	}
	return result, nil
}

func renderInline(input string) string {
	placeholders := make([]string, 0)
	protect := func(value string) string {
		placeholder := "\x00PROTECTED" + strconv.Itoa(len(placeholders)) + "\x00"
		placeholders = append(placeholders, value)
		return placeholder
	}
	value := input
	value = codeSpanPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := codeSpanPattern.FindStringSubmatch(match)
		return protect("<code>" + escapeHTML(parts[1]) + "</code>")
	})
	value = spoilerPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := spoilerPattern.FindStringSubmatch(match)
		return protect("<span class=\"tg-spoiler\">" + renderInline(parts[1]) + "</span>")
	})
	value = linkPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := linkPattern.FindStringSubmatch(match)
		if !safeLink(parts[2]) {
			return match
		}
		return protect("<a href=\"" + html.EscapeString(parts[2]) + "\">" + renderInline(parts[1]) + "</a>")
	})
	value = boldPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := boldPattern.FindStringSubmatch(match)
		text := parts[1]
		if text == "" {
			text = parts[2]
		}
		return protect("<b>" + renderInline(text) + "</b>")
	})
	value = italicPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := italicPattern.FindStringSubmatch(match)
		text := parts[2]
		if text == "" {
			text = parts[4]
		}
		prefix := parts[1]
		if prefix == "" {
			prefix = parts[3]
		}
		return protect(prefix + "<i>" + renderInline(text) + "</i>")
	})
	value = escapeHTML(value)
	for index, placeholderValue := range placeholders {
		value = strings.ReplaceAll(value, "\x00PROTECTED"+strconv.Itoa(index)+"\x00", placeholderValue)
	}
	return value
}

func isTableSeparator(line string) bool {
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return false
	}
	start := 0
	end := len(parts)
	if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
		start = 1
	}
	if end > start && strings.TrimSpace(parts[end-1]) == "" {
		end--
	}
	if end-start < 1 {
		return false
	}
	for index := start; index < end; index++ {
		trimmed := strings.TrimSpace(parts[index])
		trimmed = strings.Trim(trimmed, ":")
		if trimmed == "" {
			return false
		}
		if strings.Trim(trimmed, "-") != "" {
			return false
		}
	}
	return true
}

func renderTable(header string) string {
	cells := strings.Split(header, "|")
	var result strings.Builder
	for index, cell := range cells {
		if index > 0 {
			result.WriteString("  ")
		}
		result.WriteString(renderInline(strings.TrimSpace(cell)))
	}
	return result.String()
}

func safeLink(value string) bool {
	return strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "tg://")
}

// escapeInline escapes text destined for Telegram InlineKeyboard
// labels, button text, choice titles, or other UI surfaces that
// ship with parse_mode=HTML. Converts `<`, `>`, `&` to their HTML
// entities so user-supplied text renders as literal characters
// rather than being parsed as HTML by Telegram's renderer.
//
// Distinct from chunkBody.Compose() (Layer 1 / view) which
// routes header / entries / footer through different pipelines —
// escapeInline is for non-chunk-body UI text where the entire
// payload is escape-one-pass and the caller controls no
// surrounding markup. RenderMarkdown paths should NOT use this;
// they handle user markdown internally via escapeHTML.
func escapeInline(value string) string {
	return html.EscapeString(value)
}

// escapeHTML is the internal single-character escape used inside
// RenderMarkdown. It exists as a named function so RenderMarkdown's
// flow is grep-able; production code outside RenderMarkdown should
// prefer escapeInline (UI text) or chunkBody.Compose (chunk bodies).
func escapeHTML(value string) string {
	return html.EscapeString(value)
}

// renderMarkdownSafe is the SOLE place that runs RenderMarkdown +
// escapeHTML fallback. Callers that need "raw markdown → safe HTML
// for Telegram wire" must use this rather than duplicating the
// try-render-or-escape pattern at each call site.
//
// Layers above this primitive (each with its own concern):
//   - RenderForWire: block-level wire entry, used by standalone
//     messages (sendOutResultMessage)
//   - chunkBody.Compose: per-entry loop with isHTML flag routing,
//     used by chain chunks
//   - splitOversizedSegmentLocked / splitOversizedErrorSegmentLocked:
//     pre-render the whole oversized segment once before splitting
//
// RenderMarkdown and escapeHTML remain exported for the rare caller
// (tests, low-level Compose per-entry loop, internal parser fall-
// through) that wants raw escape or raw render.
//
// Future feishu §13.17 / §13.19-style sanitize pipeline (image
// strip, heading demotion, fence newline normalization, non-HTTP
// URL → plain) should be injected here — one site, every consumer
// (RenderForWire, chunkBody.Compose, SPLIT pre-render) inherits
// automatically. See docs/channel/telegram.md §11.12.13 / §11.12.19.
func renderMarkdownSafe(s string) string {
	if s == "" {
		return ""
	}
	out, err := RenderMarkdown(s)
	if err != nil {
		return escapeHTML(s)
	}
	return out
}

// RenderForWire is the SINGLE wire-facing entry point for turning
// raw text (LLM markdown, agent output) into Telegram parse_mode=HTML
// bytes. Outbound code paths that ship plain markdown straight to
// sendMessage / sendTelegramMessage must call this first; the wire
// already sets parse_mode=HTML (see topic.go), but the *content* still
// has to be rendered — otherwise markdown chars leak through as
// literals (e.g. `**bold**` shows as five characters instead of
// rendered bold).
//
// Delegates to renderMarkdownSafe for the actual markdown→HTML pass
// + fallback. This wrapper exists to give callers (sendOutResultMessage
// and any future single-shot message render path) a named "block
// entry" with empty-input short-circuit and a stable signature.
//
// chunkBody.Compose() does NOT route through this — its entries
// flow is per-line with isHTML awareness, and error fallback is
// inline (escapeHTML on miss). It calls renderMarkdownSafe directly
// per entry. A block-level wrapper here would force chunk_body.go
// to thread isHTML/isMarkdown flags through a stringly helper and
// break the per-entry invariant in the public Compose() spec. Keep
// RenderForWire scoped to "raw markdown block → safe HTML block".
func RenderForWire(raw string) string {
	return renderMarkdownSafe(raw)
}

// appendTrailerToBody appends the StatusBar panel to body if
// footerLines is non-empty. Returns body unchanged when footer is
// absent. Sole place where the "body + \n\n + StatusBar frame"
// trailer pattern lives; previously inlined in sendOutResultMessage
// and a candidate for duplication in any future standalone-message
// render path.
//
// The "\n\n" gap (not "\n────\n" — see §11.12.4.1 v9 P2.1) gives the
// StatusBar panel its own visual block below the result body. The
// panel itself uses box-drawing chars (┌──› / └──›) that are
// already safe-HTML — no further RenderMarkdown / escapeHTML call
// here, since either would mangle the frame.
func appendTrailerToBody(body string, footerLines []string) string {
	if len(footerLines) == 0 {
		return body
	}
	return body + "\n\n" + statusbar.RenderPanel(footerLines)
}

// splitTelegramText splits rendered HTML text into chunks of ≤ limit bytes.
//
// Cuts are guaranteed to land at positions that are:
//   - NOT inside an HTML tag (between '<' and the matching '>') —
//     so no chunk starts mid-tag like `<b` / `</b` / `<blockqu`
//   - NOT inside a <pre>...</pre> atomic block — pre blocks are kept
//     whole when possible so the formatting wrapper is preserved
//
// When the natural newline-or-space cut would land inside a tag or
// pre block, the cut walks back to the largest safe position ≤ limit.
// Tiebreaker order: \n > ' ' > first safe position. When no safe
// position exists in the window (rare; only when the entire window
// is one giant tag or one giant pre block), the function falls back
// to byte-cut at limit and accepts unbalanced tags in the resulting
// chunks — Telegram's HTML parser tolerates stray open/close tags.
//
// pre-block-spanning-limits is a known limitation: if a single
// <pre>...</pre> block exceeds `limit`, the hard-cut splits the
// block in two and Telegram will render the two halves differently
// (first half as preformatted, second half as plain text because
// the stray `</pre>` at the end of chunk N and missing `<pre>` at
// the start of chunk N+1 are interpreted literally). Future work
// could insert balanced `</pre>` / `<pre>` pairs around the cut to
// preserve rendering — out of scope for commit A.
func splitTelegramText(rendered string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, errors.New("telegram: invalid message limit")
	}
	if len(rendered) <= limit {
		return []string{rendered}, nil
	}

	unsafe := computeUnsafePositions(rendered)

	var chunks []string
	start := 0
	n := len(rendered)
	for start < n {
		end := start + limit
		if end >= n {
			chunks = append(chunks, rendered[start:])
			return chunks, nil
		}
		cut := findSafeCut(rendered, unsafe, start, end)
		if cut <= start {
			// No safe cut within (start, end]; fall back to byte
			// cut at end. May land inside a tag / pre block —
			// documented known limitation.
			cut = end
		}
		chunks = append(chunks, rendered[start:cut])
		start = cut
	}
	return chunks, nil
}

// computeUnsafePositions walks rendered once and returns a bitmap
// of length n marking every byte position that is unsafe as a cut
// point. A position is unsafe when it falls inside any HTML tag
// (`<...>`) or inside a `<pre>...</pre>` atomic block.
//
// The bitmap is indexable by byte offset; unsafe[i] = true means
// "do not cut immediately before s[i]". Positions just after a
// `>` (end of tag) or just before a `<` (start of next tag) are
// safe, which is the natural cut point.
//
// On malformed input (e.g., '<' with no matching '>') the byte is
// treated as plain text and the scan continues.
func computeUnsafePositions(rendered string) []bool {
	n := len(rendered)
	unsafe := make([]bool, n)

	i := 0
	for i < n {
		if rendered[i] != '<' {
			i++
			continue
		}
		end := strings.IndexByte(rendered[i:], '>')
		if end < 0 {
			// Malformed: lone '<' with no closing '>'. Treat
			// as plain text and advance one byte.
			i++
			continue
		}
		tagStart := i
		tagEnd := i + end + 1
		tagContent := rendered[tagStart+1 : tagEnd-1]

		// Mark every byte inside the tag as unsafe.
		for j := tagStart; j < tagEnd && j < n; j++ {
			unsafe[j] = true
		}

		if isStartTag(tagContent, "pre") {
			// Find matching </pre> to scope the atomic block.
			closeIdx := strings.Index(rendered[tagEnd:], "</pre>")
			if closeIdx >= 0 {
				preEnd := tagEnd + closeIdx + len("</pre>")
				// Mark content between <pre> and </pre> as unsafe.
				for j := tagEnd; j < preEnd && j < n; j++ {
					unsafe[j] = true
				}
				i = preEnd
				continue
			}
			// Unmatched <pre> — every byte from here to EOF is
			// inside an open pre block. Mark them all unsafe.
			for j := tagEnd; j < n; j++ {
				unsafe[j] = true
			}
			return unsafe
		}

		i = tagEnd
	}
	return unsafe
}

// findSafeCut returns the largest position p in (lo, hi] where p is
// safe (not in `unsafe`) and s[p-1] is a preferred break char.
// Tiebreaker order: '\n' first, then ' ', then any safe position.
// Returns 0 when no safe position exists in the window.
func findSafeCut(s string, unsafe []bool, lo, hi int) int {
	n := len(s)
	if hi > n {
		hi = n
	}
	if lo < 0 {
		lo = 0
	}
	if lo >= hi {
		return 0
	}

	// Pass 1: cut right after a '\n'.
	for i := hi; i > lo; i-- {
		if i >= n || unsafe[i] {
			continue
		}
		if i > 0 && s[i-1] == '\n' {
			return i
		}
	}
	// Pass 2: cut right after a ' '.
	for i := hi; i > lo; i-- {
		if i >= n || unsafe[i] {
			continue
		}
		if i > 0 && s[i-1] == ' ' {
			return i
		}
	}
	// Pass 3: any safe position regardless of preceding char.
	for i := hi; i > lo; i-- {
		if i < n && !unsafe[i] {
			return i
		}
	}
	return 0
}

// isStartTag reports whether tagContent is an opening tag for the
// given HTML element name. Whitespace after the name is allowed; the
// name must not be a prefix of a different name (so `isStartTag(t,
// "pre")` matches `<pre>` and `<pre lang>` but NOT `<pretty>`).
func isStartTag(tagContent, name string) bool {
	if !strings.HasPrefix(tagContent, name) {
		return false
	}
	rest := tagContent[len(name):]
	if rest == "" {
		return true
	}
	switch rest[0] {
	case ' ', '\t', '\n', '\r', '/', '>':
		return true
	}
	return false
}
