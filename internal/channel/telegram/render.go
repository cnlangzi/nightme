package telegram

import (
	"errors"
	"html"
	"regexp"
	"strings"

	"github.com/cnlangzi/nightme/internal/statusbar"
)

var (
	codeSpanPattern  = regexp.MustCompile("`([^`\\n]+)`")
	linkPattern      = regexp.MustCompile("\\[([^\\]]+)\\]\\(([^)]+)\\)")
	boldPattern      = regexp.MustCompile("\\*\\*([^*\\n]+)\\*\\*|__([^_\\n]+)__")
	italicPattern    = regexp.MustCompile("(^|[^*])\\*([^*\\n]+)\\*|(^|[^_])_([^_\\n]+)_")
	spoilerPattern   = regexp.MustCompile("\\|\\|([^|\\n]+)\\|\\|")
	strikePattern    = regexp.MustCompile("~~([^~\\n]+)~~")
	headingPattern   = regexp.MustCompile("^#{1,6}\\s+(.+)$")
	bulletPattern    = regexp.MustCompile("^[-*]\\s+(.+)$")
	orderedPattern   = regexp.MustCompile("^([0-9]+)\\.\\s+(.+)$")
	quotePattern     = regexp.MustCompile("^>\\s*(.*)$")
	fenceLangPattern = regexp.MustCompile("^```([A-Za-z0-9_+\\-]{1,32})$")
)

// placeholderBase is the start of the Unicode Private Use Area
// (U+E000..U+F8FF). renderInline uses a single PUA rune per
// placeholder — PUA chars are reserved for internal use and
// cannot collide with real user text. The previous sentinel
// (`\x00PROTECTED<n>\x00`) was NUL-byte-bounded, which leaked when
// any intermediate layer stripped the NUL bytes (the 2026-08-25
// bug where users saw `**PROTECTED0**` in their Telegram chat).
// PUA chars survive any chain that doesn't actively strip them,
// and the per-rune substitute loop below is more robust than
// strings.ReplaceAll on a multi-character sentinel.
const placeholderBase = 0xE000

// expandableBlockquoteThresholdChars is the cutoff at which `>`
// quote blocks render as `<blockquote expandable>` (collapsible)
// vs. the legacy `<blockquote>` (always visible). Bot API 7.0+
// (2024-03) added the expandable variant; quotes shorter than this
// stay non-expandable because expanding a one-line quote is more
// annoying than seeing it inline.
const expandableBlockquoteThresholdChars = 800

func RenderMarkdown(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	var output strings.Builder
	inCodeBlock := false
	codeLang := ""
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				output.WriteString("</code></pre>")
				codeLang = ""
			} else {
				// Extract language token from ```go, ```python,
				// ```diff, etc. Telegram's official clients do
				// client-side syntax highlighting when the class
				// is set (Bot API 6.0+, 2023-04). lang is
				// validated to a safe alphanumeric+dash subset
				// so a hostile markdown can't break out of the
				// `class="..."` attribute.
				lang := strings.TrimPrefix(trimmed, "```")
				lang = strings.TrimSpace(lang)
				if langMatches := fenceLangPattern.FindStringSubmatch(trimmed); langMatches != nil {
					codeLang = langMatches[1]
					output.WriteString(`<pre><code class="language-`)
					output.WriteString(codeLang)
					output.WriteString(`">`)
				} else {
					output.WriteString("<pre><code>")
				}
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
			// Visual hierarchy: H1 gets a 📌 pin emoji + a blank
			// line of breathing room; H2-H6 get a Unicode underline
			// bar after the bold title. Telegram doesn't render
			// native heading sizes, so this is the cheapest way to
			// distinguish levels without breaking the existing
			// `<b>` rendering.
			level := 0
			for _, r := range matches[0] {
				if r == '#' {
					level++
				} else {
					break
				}
			}
			if level == 1 {
				output.WriteString("<b>📌 ")
				output.WriteString(renderInline(matches[1]))
				output.WriteString("</b>")
			} else {
				output.WriteString("<b>")
				output.WriteString(renderInline(matches[1]))
				output.WriteString("</b>\n")
				output.WriteString(strings.Repeat("─", 8))
			}
		} else if matches := bulletPattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString("• ")
			output.WriteString(renderInline(matches[1]))
		} else if matches := orderedPattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString(matches[1] + ". " + renderInline(matches[2]))
		} else if matches := quotePattern.FindStringSubmatch(trimmed); matches != nil {
			// Long `>` blocks collapse to `<blockquote expandable>`
			// (Bot API 7.0+, 2024-03). Default-collapsed; user taps
			// "▼ Expand" to reveal. Short quotes stay inline
			// because expanding a one-line quote is more annoying
			// than seeing it inline. The threshold uses the
			// rendered text length (post-renderInline) so HTML
			// overhead doesn't push a small quote over the line.
			quoteText := renderInline(matches[1])
			if len(quoteText) > expandableBlockquoteThresholdChars {
				output.WriteString("<blockquote expandable>")
				output.WriteString(quoteText)
				output.WriteString("</blockquote>")
			} else {
				output.WriteString("<blockquote>")
				output.WriteString(quoteText)
				output.WriteString("</blockquote>")
			}
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
		// Unmatched opening fence — close it to keep the HTML
		// well-formed (mirrors the closing-fence branch above).
		output.WriteString("</code></pre>")
	}
	result := output.String()
	if result == "" {
		return "", nil
	}
	return result, nil
}

func renderInline(input string) string {
	return renderInlineWithPH(input, nil)
}

// renderInlineWithPH is the recursive core of renderInline. The
// `ph` argument is the placeholder table — append to it on every
// match and read from it during the final PUA-char walk.
//
// Top-level callers pass `nil`; renderInlineWithPH allocates a
// fresh slice. Recursive callers (inside the bold / italic /
// strike / link / spoiler regex callbacks below) pass the SAME
// slice down so child renders can resolve PUA runes that the
// parent scope's codeSpan pass already protected.
//
// Sharing `ph` across recursion is the bug fix for the inline-
// code-inside-bold drop (PUA slice was per-frame, so children
// couldn't see parents' runes and fell through WriteRune as raw
// Unicode — leaving stray Private Use Area chars in the output).
func renderInlineWithPH(input string, ph *[]string) string {
	if ph == nil {
		s := make([]string, 0, 8)
		ph = &s
	}
	protect := func(value string) string {
		idx := len(*ph)
		if idx >= 0x1000 {
			// PUA has only 6400 code points; reserve some for
			// nested recursion depth. If we hit the cap, fall
			// back to raw value (renders as plain text but
			// doesn't corrupt the output).
			return value
		}
		*ph = append(*ph, value)
		// Sentinel is a single Unicode Private Use Area rune
		// (U+E000 + idx). PUA chars are reserved for internal
		// use, so they cannot collide with real user text. This
		// replaces the legacy `\x00PROTECTED<n>\x00` NUL-byte
		// sentinel, which leaked when any intermediate layer
		// stripped the NUL bytes (see 2026-08-25 incident).
		return string(rune(placeholderBase + idx))
	}
	value := input
	value = codeSpanPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := codeSpanPattern.FindStringSubmatch(match)
		return protect("<code>" + escapeHTML(parts[1]) + "</code>")
	})
	value = spoilerPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := spoilerPattern.FindStringSubmatch(match)
		return protect("<span class=\"tg-spoiler\">" + renderInlineWithPH(parts[1], ph) + "</span>")
	})
	value = strikePattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := strikePattern.FindStringSubmatch(match)
		return protect("<s>" + renderInlineWithPH(parts[1], ph) + "</s>")
	})
	value = linkPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := linkPattern.FindStringSubmatch(match)
		if !safeLink(parts[2]) {
			return match
		}
		return protect("<a href=\"" + html.EscapeString(parts[2]) + "\">" + renderInlineWithPH(parts[1], ph) + "</a>")
	})
	value = boldPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := boldPattern.FindStringSubmatch(match)
		text := parts[1]
		if text == "" {
			text = parts[2]
		}
		return protect("<b>" + renderInlineWithPH(text, ph) + "</b>")
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
		return protect(prefix + "<i>" + renderInlineWithPH(text, ph) + "</i>")
	})
	value = escapeHTML(value)
	// Walk the escaped value rune-by-rune and substitute PUA-char
	// placeholders back with their raw HTML strings. PUA chars in
	// user input (vanishingly rare — reserved range) that don't
	// correspond to a placeholder pass through unchanged.
	var sb strings.Builder
	for _, r := range value {
		if r >= placeholderBase && r < placeholderBase+0x1000 {
			idx := int(r - placeholderBase)
			if idx < len(*ph) {
				sb.WriteString((*ph)[idx])
				continue
			}
		}
		sb.WriteRune(r)
	}
	return sb.String()
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

// expandableFullThresholdChars is the cutoff at which RenderForWire
// wraps the entire rendered block in `<blockquote expandable>` so
// a single OutResult that looks "long" (already within the 4096
// hard limit but visually heavy on Telegram) collapses to a one-
// line "▼ Expand" affordance by default. The wrap is intentional
// only at the BLOCK level — `>` quote sub-blocks already collapse
// individually via expandableBlockquoteThresholdChars inside
// RenderMarkdown, so a long message with many small quotes doesn't
// get double-folded.
const expandableFullThresholdChars = 2000

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
// + fallback. Then wraps the result in `<blockquote expandable>`
// when the rendered body is longer than expandableFullThresholdChars
// so a long result message collapses to "▼ Expand" by default —
// callers that ship a long OutResult no longer have to choose
// between sending a 10-message chain and forcing the user to scroll
// a single wall of text.
//
// chunkBody.Compose() does NOT route through this — its entries
// flow is per-line with isHTML awareness, and error fallback is
// inline (escapeHTML on miss). It calls renderMarkdownSafe directly
// per entry. A block-level wrapper here would force chunk_body.go
// to thread isHTML/isMarkdown flags through a stringly helper and
// break the per-entry invariant in the public Compose() spec. Keep
// RenderForWire scoped to "raw markdown block → safe HTML block".
func RenderForWire(raw string) string {
	rendered := renderMarkdownSafe(raw)
	return maybeWrapFullExpandable(rendered)
}

// maybeWrapFullExpandable wraps `rendered` in `<blockquote expandable>`
// when it crosses the full-message fold threshold. The wrap is
// skipped (and the original returned) when:
//   - The output already contains a `<blockquote expandable>` tag
//     (any sub-quote already opted in via expandableBlockquoteThresholdChars;
//     nesting is illegal and Telegram's parser rejects it).
//   - The rendered length is ≤ expandableFullThresholdChars — short
//     bodies stay inline because expanding a small message is more
//     annoying than reading it.
//   - Wrapping would push the total over Telegram's 4096-char hard
//     limit. We fall back to non-wrapped and let splitTelegramText
//     cut the message into multiple chunks instead.
func maybeWrapFullExpandable(rendered string) string {
	if strings.Contains(rendered, "<blockquote expandable>") {
		return rendered
	}
	if len(rendered) <= expandableFullThresholdChars {
		return rendered
	}
	const openTag = "<blockquote expandable>"
	const closeTag = "</blockquote>"
	wrapped := openTag + rendered + closeTag
	if len(wrapped) > 4096 {
		// Wrap would push over Telegram's hard limit. Fall back
		// to unwrapped; splitTelegramText will chop into multiple
		// messages at the call site (sendOutResultMessage).
		return rendered
	}
	return wrapped
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
//
// Markdown link conversion (§20.7 commit D): footerLines may contain
// markdown link syntax like `[#284](https://github.com/.../pull/284)`
// from statusbar.formatPRSegment. The statusbar package emits this
// for both Feishu (lark_md renders it natively) and Telegram (which
// would too if we ran it through RenderMarkdown) — but the panel
// path bypasses RenderMarkdown to preserve the box-drawing frame,
// so Telegram receives the literal `[#N](url)` text. The user
// observes `[#284](https://...)` as plain text instead of a clickable
// link. Convert via the wire-format link rule (escape URL + text,
// only allow http/https/tg schemes) so Telegram renders it as a
// proper link while Feishu's statusbar consumer (which still passes
// the raw textlines through its own lark_md) sees no change.
func appendTrailerToBody(body string, footerLines []string) string {
	if len(footerLines) == 0 {
		return body
	}
	converted := make([]string, len(footerLines))
	for i, line := range footerLines {
		converted[i] = wireFormatFooterLine(line)
	}
	return body + "\n\n" + statusbar.RenderPanel(converted)
}

// wireFormatFooterLine rewrites markdown link syntax in a single
// footer line into Telegram-safe HTML. Lines without `[...](...)`
// pass through unchanged. Lines with unsafe schemes (file://,
// javascript:, etc.) keep the literal markdown text — the link
// is suppressed, not coerced into a malicious URL.
//
// This is the same rule renderInline applies for inline links;
// duplicating it here keeps the footer path self-contained (no
// need to thread an isHTML flag through statusbar to keep the
// shared statusbar package render-mode-agnostic).
func wireFormatFooterLine(line string) string {
	return linkPattern.ReplaceAllStringFunc(line, func(match string) string {
		parts := linkPattern.FindStringSubmatch(match)
		text := parts[1]
		url := parts[2]
		if !safeLink(url) {
			return match
		}
		return "<a href=\"" + html.EscapeString(url) + "\">" + html.EscapeString(text) + "</a>"
	})
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
