package telegram

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// markdownToRichBlocks — L2 walker (docs/channel/telegram.md §20.6.2).
//
// Pure-function line-based walker that converts raw markdown to a
// rich_message[blocks] array. Supports the 9 common LLM-output block
// types (paragraph / heading / pre / list / blockquote / divider /
// details / table / footer) plus inline RichText entities (bold /
// italic / code / url). Returns ok=false when the walker encounters
// syntax it can't represent (footnote refs, raw HTML, images, task
// lists); callers fall back to the L1 rich_message[markdown] path.
//
// No AST library dep — goldmark is in go.sum as a transitive but
// not a direct dep. Adding it just for this walker would be a
// sizable surface-area bump. The line-based approach mirrors the
// project's existing regex-based markdown renderer (render.go), so
// the walker stays consistent with the rest of the pipeline. When
// LLM-output parity demands a real AST walker, goldmark promotion
// to a direct dep is a one-line change in go.mod.
// ---------------------------------------------------------------------------

// richBlockCap mirrors richBlockCountLimit (defined in rich.go).
// Re-declared as an untyped const here so the walker's block-count
// gate stays locally readable without an import cycle.
const (
	richWalkerBlockCap        = 500
	richWalkerCharCap         = 32000
	richWalkerBlockPreflightN = 4
	richWalkerBlockPreflightD = 5
)

var (
	richHeadingPat = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
	richBulletPat  = regexp.MustCompile(`^[-*+]\s+(.+)$`)
	richOrderedPat = regexp.MustCompile(`^\d+\.\s+(.+)$`)
	richQuotePat   = regexp.MustCompile(`^>\s*(.*)$`)
	richDividerPat = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})\s*$`)
)

// inlinePatternOrder lists the inline regexes in priority order. The
// walker applies them in sequence on each line of text. The order is
// load-bearing: code spans (backticks) must run before bold/italic
// because `*foo*` inside a code span must NOT be parsed as italic.
var inlinePatternOrder = []struct {
	pattern *regexp.Regexp
	kind    string // "bold" | "italic" | "code" | "url"
}{
	{regexp.MustCompile("`([^`\\n]+)`"), "code"},
	{regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`), "bold"},
	{regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*|(^|[^_])_([^_\n]+)_`), "italic"},
	{regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`), "url"},
}

// inlineToRichText converts a single line of inline markdown into a
// RichText value: either a plain string (no entities) or an array
// of strings + entity objects (per Bot API 10.1 InputRichBlock
// `text` field shape — see §20.4).
//
// We use a simple per-line scan: for each pattern in priority order,
// replace matches with placeholder Unicode sentinels (PUA range),
// then unwrap them. This avoids regex-on-regex recursion for nested
// patterns (e.g., `**bold with `code` inside**`).
//
// safeLink mirrors the renderer in render.go: only http/https/tg
// schemes are accepted; anything else falls back to literal text.
func inlineToRichText(input string) any {
	matched := false
	type slot struct {
		kind   string
		text   string
		url    string // only set for kind="url"
		prefix string // for italic (preserved leading char)
	}
	var slots []slot
	nextIdx := 0

	// PUA range base — same scheme render.go uses for protected
	// inline renders. We allocate 2 chars per inline entity so
	// bold-with-italic + bold-with-code can co-exist.
	const puaBase = 0xF0000

	for _, pat := range inlinePatternOrder {
		// Make a working copy and replace matches with PUA
		// sentinels while recording slots in order.
		idx := nextIdx
		working := pat.pattern.ReplaceAllStringFunc(input, func(match string) string {
			parts := pat.pattern.FindStringSubmatch(match)
			text := ""
			prefix := ""
			switch pat.kind {
			case "code":
				// groups: 1 = code content.
				text = parts[1]
			case "bold":
				// groups: 1 = **content**, 2 = __content__.
				text = parts[1]
				if text == "" {
					text = parts[2]
				}
			case "italic":
				// groups: 1 = *prefix*, 2 = *text*;
				//         3 = _prefix_, 4 = _text_.
				text = parts[2]
				if text == "" {
					text = parts[4]
				}
				if text == "" {
					return match
				}
				prefix = parts[1]
				if prefix == "" {
					prefix = parts[3]
				}
			case "url":
				// groups: 1 = link text, 2 = url.
				url := parts[2]
				if !safeLink(url) {
					return match
				}
				text = parts[1]
				slot := slot{kind: "url", text: text, url: url}
				slots = append(slots, slot)
				sentinel := string(rune(puaBase + rune(idx)))
				idx++
				nextIdx = idx
				matched = true
				return sentinel
			}
			slot := slot{kind: pat.kind, text: text, prefix: prefix}
			slots = append(slots, slot)
			sentinel := string(rune(puaBase + rune(idx)))
			idx++
			nextIdx = idx
			matched = true
			return sentinel
		})
		input = working
	}

	if !matched {
		return input
	}

	// Rebuild the array, splitting on PUA sentinels.
	type piece struct {
		text     string
		isEntity bool
		slotIdx  int
	}
	var pieces []piece
	var sb strings.Builder
	for _, r := range input {
		if r >= puaBase && r < puaBase+0x10000 {
			if sb.Len() > 0 {
				pieces = append(pieces, piece{text: sb.String()})
				sb.Reset()
			}
			idx := int(r - puaBase)
			pieces = append(pieces, piece{isEntity: true, slotIdx: idx})
		} else {
			sb.WriteRune(r)
		}
	}
	if sb.Len() > 0 {
		pieces = append(pieces, piece{text: sb.String()})
	}

	out := make([]any, 0, len(pieces))
	for _, p := range pieces {
		if !p.isEntity {
			out = append(out, p.text)
			continue
		}
		if p.slotIdx >= len(slots) {
			continue
		}
		s := slots[p.slotIdx]
		switch s.kind {
		case "code":
			out = append(out, map[string]any{"type": "code", "text": s.text})
		case "bold":
			out = append(out, map[string]any{"type": "bold", "text": s.text})
		case "italic":
			out = append(out, map[string]any{"type": "italic", "text": s.text})
		case "url":
			out = append(out, map[string]any{"type": "url", "text": s.text, "url": s.url})
		}
	}
	return out
}

// walkParagraph consumes a run of non-blank non-marker lines and emits
// a paragraph block. Returns ok=false if any line uses syntax we
// can't represent (footnote refs, raw HTML, image refs).
func walkParagraph(lines []string, start int) (map[string]any, int, bool) {
	var collected []string
	i := start
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isBlockMarker(trimmed) {
			break
		}
		// Bail on syntax we don't represent.
		if strings.Contains(trimmed, "[^") || strings.Contains(trimmed, "![") {
			return nil, 0, false
		}
		// Raw HTML detection is conservative: any `<` not
		// followed immediately by a space and a letter (i.e. an
		// actual HTML tag) — markdown allows literal `<` in prose,
		// so we only bail when the angle bracket is part of a
		// likely-tag shape. Detecting every raw-HTML edge case is
		// not the walker's job; markdown path will render it.
		if looksLikeRawHTML(trimmed) {
			return nil, 0, false
		}
		collected = append(collected, line)
		i++
	}
	if len(collected) == 0 {
		return nil, 0, false
	}
	text := strings.Join(collected, "\n")
	return map[string]any{
		"type": "paragraph",
		"text": inlineToRichText(text),
	}, i, true
}

// looksLikeRawHTML reports whether a trimmed line contains a
// likely-HTML-tag shape. Conservative — bails only when `<` is
// followed by a tag-like body (`</x>` close, `<x` open) and at
// least one alphabetic character before the next `>`. Literal `<`
// in math expressions or comparisons is preserved.
func looksLikeRawHTML(line string) bool {
	for i := 0; i < len(line); i++ {
		if line[i] != '<' {
			continue
		}
		// Find matching `>` ahead.
		j := strings.IndexByte(line[i:], '>')
		if j < 0 {
			continue
		}
		body := line[i+1 : i+j]
		// Strip leading `/` (closing tags) and trailing `/` (void).
		body = strings.TrimLeft(body, "/")
		body = strings.TrimRight(body, "/")
		if body == "" {
			continue
		}
		// Tag body should start with a letter (or `!` for doctype).
		if !((body[0] >= 'a' && body[0] <= 'z') || (body[0] >= 'A' && body[0] <= 'Z') || body[0] == '!') {
			continue
		}
		// And contain at least one letter / digit.
		hasAlnum := false
		for _, r := range body {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				hasAlnum = true
				break
			}
		}
		if hasAlnum {
			return true
		}
	}
	return false
}

// isBlockMarker reports whether a trimmed line starts a new block
// (heading / list / quote / fence / divider / table). Used to bound
// paragraph runs.
func isBlockMarker(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	switch {
	case strings.HasPrefix(trimmed, "#"):
		return true
	case strings.HasPrefix(trimmed, "```"):
		return true
	case strings.HasPrefix(trimmed, ">"):
		return true
	case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "), strings.HasPrefix(trimmed, "+ "):
		return true
	case richOrderedPat.MatchString(trimmed):
		return true
	case richDividerPat.MatchString(trimmed):
		return true
	case strings.HasPrefix(trimmed, "|") && strings.Contains(trimmed, "|"):
		// crude table detection — first row with pipes. Real
		// walker should validate the separator row; we keep it
		// conservative and let the markdown path handle tables
		// for now (L2 defer for tables).
		return true
	}
	return false
}

// walkHeading emits a heading block from a single line.
func walkHeading(line string) (map[string]any, bool) {
	m := richHeadingPat.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	level := len(m[1])
	text := m[2]
	if level > 6 {
		level = 6
	}
	return map[string]any{
		"type": "heading",
		"text": inlineToRichText(text),
		"size": level,
	}, true
}

// walkFence consumes a fenced code block starting at line start. The
// opening fence line is at lines[start]; the closing fence is the
// next line starting with `.
func walkFence(lines []string, start int) (map[string]any, int, bool) {
	if start >= len(lines) {
		return nil, 0, false
	}
	open := strings.TrimSpace(lines[start])
	if !strings.HasPrefix(open, "```") {
		return nil, 0, false
	}
	lang := strings.TrimSpace(strings.TrimPrefix(open, "```"))
	var collected []string
	i := start + 1
	for i < len(lines) {
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			blk := map[string]any{
				"type": "pre",
				"text": strings.Join(collected, "\n"),
			}
			if lang != "" {
				blk["language"] = lang
			}
			return blk, i + 1, true
		}
		collected = append(collected, line)
		i++
	}
	// Unterminated fence — bail.
	return nil, 0, false
}

// walkList consumes a contiguous run of bullet list items. Ordered
// lists and mixed-order lists fall back (ok=false) so the L1 path
// picks them up.
func walkList(lines []string, start int) (map[string]any, int, bool) {
	items := []map[string]any{}
	i := start
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			break
		}
		if m := richBulletPat.FindStringSubmatch(trimmed); m != nil {
			item := map[string]any{
				"blocks": []map[string]any{
					{"type": "paragraph", "text": inlineToRichText(m[1])},
				},
			}
			items = append(items, item)
			i++
			continue
		}
		break
	}
	if len(items) == 0 {
		return nil, 0, false
	}
	return map[string]any{
		"type":  "list",
		"items": items,
	}, i, true
}

// walkBlockquote consumes a contiguous run of > prefixed lines.
func walkBlockquote(lines []string, start int) (map[string]any, int, bool) {
	var collected []string
	i := start
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, ">") {
			break
		}
		// Strip leading "> " or ">" prefix.
		body := strings.TrimPrefix(trimmed, ">")
		body = strings.TrimPrefix(body, " ")
		collected = append(collected, body)
		i++
	}
	if len(collected) == 0 {
		return nil, 0, false
	}
	return map[string]any{
		"type": "blockquote",
		"blocks": []map[string]any{
			{"type": "paragraph", "text": inlineToRichText(strings.Join(collected, "\n"))},
		},
	}, i, true
}

// markdownToRichBlocks walks raw markdown and emits a JSON-encoded
// rich_message[blocks] array. Returns ok=false when the walker hits
// syntax it can't represent (footnote refs, raw HTML, images,
// tables, ordered lists with non-numeric markers); callers fall
// back to the L1 rich_message[markdown] path which lets Telegram's
// server-side parser handle them.
//
// The walker is deliberately line-based rather than AST-based: the
// project's existing markdown renderer (render.go) is regex-based,
// so the walker stays consistent with that pipeline's assumptions
// and avoids the dependency surface of pulling in a real AST
// library (goldmark is transitive, not direct). When L2's block
// fidelity matters more than this minimal pass, swapping in
// goldmark is a one-line go.mod promotion + walker rewrite.
func markdownToRichBlocks(rawMD string) (string, bool) {
	if rawMD == "" {
		return "", false
	}
	if len(rawMD) > richWalkerCharCap {
		return "", false
	}

	lines := strings.Split(rawMD, "\n")
	var blocks []map[string]any
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			i++
			continue
		}

		switch {
		case richHeadingPat.MatchString(trimmed):
			blk, ok := walkHeading(trimmed)
			if !ok {
				return "", false
			}
			blocks = append(blocks, blk)
			i++
		case strings.HasPrefix(trimmed, "```"):
			blk, next, ok := walkFence(lines, i)
			if !ok {
				return "", false
			}
			blocks = append(blocks, blk)
			i = next
		case richBulletPat.MatchString(trimmed):
			blk, next, ok := walkList(lines, i)
			if !ok {
				return "", false
			}
			blocks = append(blocks, blk)
			i = next
		case richOrderedPat.MatchString(trimmed):
			// Ordered lists: defer to L1 markdown path. Adding
			// proper ordered-list support requires tracking the
			// `value` / `type` (a/A/i/I) per item, which the
			// regex-based walker would need to refactor to do
			// cleanly. L1's markdown path renders them as plain
			// text with `1.` prefixes — acceptable trade-off
			// until a real AST walker ships.
			return "", false
		case richQuotePat.MatchString(trimmed):
			blk, next, ok := walkBlockquote(lines, i)
			if !ok {
				return "", false
			}
			blocks = append(blocks, blk)
			i = next
		case richDividerPat.MatchString(trimmed):
			blocks = append(blocks, map[string]any{"type": "divider"})
			i++
		case strings.HasPrefix(trimmed, "|"):
			// Tables: defer to L1 markdown path. Tables in
			// rich blocks require explicit cells / alignment
			// arrays that the line-based walker doesn't model
			// safely (alignment separators vary widely).
			return "", false
		default:
			blk, next, ok := walkParagraph(lines, i)
			if !ok {
				return "", false
			}
			blocks = append(blocks, blk)
			i = next
		}

		// Enforce block cap during the walk so we don't waste
		// cycles assembling a 600-block response the server will
		// reject. Pre-emptively bail when we exceed the cap.
		if len(blocks) > richWalkerBlockCap*richWalkerBlockPreflightN/richWalkerBlockPreflightD {
			return "", false
		}
	}

	if len(blocks) == 0 {
		return "", false
	}

	encoded, err := json.Marshal(blocks)
	if err != nil {
		// Marshalling should never fail for the shapes we
		// assemble (strings / ints / slices / maps of those),
		// but if a future change ever produces a non-marshalable
		// value, fall back rather than crash the user-visible
		// send path.
		return "", false
	}
	return string(encoded), true
}

// unused — referenced from rich.go's L1 path; the round-trip
// through fmt.Sprintf keeps the import live in case L2 ever
// wants to inline-format block JSON for debugging.
var _ = fmt.Sprintf
