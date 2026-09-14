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
// rich_message[blocks] array. Supports the 10 common LLM-output
// block types (paragraph / heading / pre / list / blockquote /
// divider / table / footer) plus inline RichText entities (bold /
// italic / code / url / footnote-strip / image-as-url). Inline
// footnote refs, inline image refs, and raw HTML are now handled
// inside the walker (footnote stripped, image downgraded to url
// entity, raw HTML kept as literal text inside a paragraph) so
// none of them trigger ok=false. The only ok=false triggers today
// are block-level shapes the walker can't model — malformed fence,
// table without separator row / mismatched column count, char cap
// exceeded. Callers (`renderRichTurnBlocksLocked`) treat ok=false as
// "emit one paragraph block with the raw body" — still a rich
// payload, never plain sendMessage text.
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
	richHeadingPat   = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
	richBulletPat    = regexp.MustCompile(`^[-*+]\s+(.+)$`)
	richOrderedPat   = regexp.MustCompile(`^\d+\.\s+(.+)$`)
	richQuotePat     = regexp.MustCompile(`^>\s*(.*)$`)
	richDividerPat   = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})\s*$`)
	richTableRowPat  = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	richTableSepCell = regexp.MustCompile(`^[\s:]*:?-+:?[\s:]*$`)
	richFootnotePat  = regexp.MustCompile(`\[\^([^\]]+)\]`)
	richImagePat     = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
)

// inlinePatternOrder lists the inline regexes in priority order. The
// walker applies them in sequence on each line of text. The order is
// load-bearing: code spans (backticks) must run before bold/italic
// because `*foo*` inside a code span must NOT be parsed as italic.
// Image refs must run before plain url refs so `![alt](url)` is not
// misread as a `[!alt](url)` link; footnote refs run before url so
// `[^1]` is not consumed as a url target.
var inlinePatternOrder = []struct {
	pattern *regexp.Regexp
	kind    string // "bold" | "italic" | "code" | "url" | "image" | "footnote"
}{
	{regexp.MustCompile("`([^`\\n]+)`"), "code"},
	{regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`), "bold"},
	{regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*|(^|[^_])_([^_\n]+)_`), "italic"},
	{richFootnotePat, "footnote"},
	{richImagePat, "image"},
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
			case "footnote":
				// GFM footnote ref `[^id]`. Stripped
				// completely — the slot is recorded
				// with empty text and the unwrap pass
				// drops it. Footnote refs rarely carry
				// meaning in Telegram chat context and
				// dropping them keeps prose readable.
				slot := slot{kind: "footnote", text: ""}
				slots = append(slots, slot)
				sentinel := string(rune(puaBase + rune(idx)))
				idx++
				nextIdx = idx
				matched = true
				return sentinel
			case "image":
				// groups: 1 = alt, 2 = url.
				alt := parts[1]
				url := parts[2]
				if !safeLink(url) {
					return match
				}
				// Empty alt → use URL as visible text.
				// Otherwise the url entity has no
				// human-readable label.
				if alt == "" {
					alt = url
				}
				slot := slot{kind: "image", text: alt, url: url}
				slots = append(slots, slot)
				sentinel := string(rune(puaBase + rune(idx)))
				idx++
				nextIdx = idx
				matched = true
				return sentinel
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
		case "image":
			// `![alt](url)` becomes a clickable link.
			// Telegram rich blocks have no inline image
			// entity, so we degrade to the same shape as
			// url — clicking opens the image URL in the
			// user's browser.
			out = append(out, map[string]any{"type": "url", "text": s.text, "url": s.url})
		case "footnote":
			// Stripped at substitution time; nothing to
			// emit here. Falling through drops the
			// sentinel piece silently.
		}
	}
	return out
}

// walkParagraph consumes a run of non-blank non-marker lines and emits
// a paragraph block. Inline footnote refs, image refs, and raw HTML
// are now handled by inlineToRichText (footnote stripped, image
// downgraded to url, raw HTML kept as literal text) — walkParagraph
// only needs to bound the run by block markers.
func walkParagraph(lines []string, start int) (map[string]any, int, bool) {
	var collected []string
	i := start
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isBlockMarker(trimmed) {
			break
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
	case richTableRowPat.MatchString(trimmed):
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

// walkList consumes a contiguous run of bullet or ordered list items.
// Bullet (`-`/`*`/`+`) and ordered (`1.`/`2.`/...) markers both
// produce the same `list` block shape — Telegram Bot API 10.1
// `InputRichBlockList` has no ordered/unordered enum and no per-item
// `type` field, so the rendering client infers the marker style from
// the item text (`1.` prefix → ordered; `-` → bullet). Mixed
// bullet+ordered in one run is treated as a single list; callers
// wanting strict separation should emit a blank line between them.
func walkList(lines []string, start int) (map[string]any, int, bool) {
	items := []map[string]any{}
	i := start
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			break
		}
		var body string
		switch {
		case richBulletPat.MatchString(trimmed):
			body = richBulletPat.FindStringSubmatch(trimmed)[1]
		case richOrderedPat.MatchString(trimmed):
			body = richOrderedPat.FindStringSubmatch(trimmed)[1]
		default:
			goto done
		}
		item := map[string]any{
			"blocks": []map[string]any{
				{"type": "paragraph", "text": inlineToRichText(body)},
			},
		}
		items = append(items, item)
		i++
	}
done:
	if len(items) == 0 {
		return nil, 0, false
	}
	return map[string]any{
		"type":  "list",
		"items": items,
	}, i, true
}

// walkTable consumes a markdown table starting at line start. The
// shape mirrors GFM tables:
//
//	| H1 | H2 |
//	|----|----|       ← separator row required
//	| 1  | 2  |
//
// Each cell is emitted as `{text, is_header?, ...}` per Bot API 10.1
// InputRichBlockTableCell. Alignment is derived from the separator
// row's leading/trailing colon (:--- left, ---: right, :---: center,
// --- default). Columns whose count disagrees across rows cause the
// whole table to be rejected so the caller falls back to a single
// paragraph block — better to degrade than ship a malformed cell
// array the server will reject.
func walkTable(lines []string, start int) (map[string]any, int, bool) {
	if start+1 >= len(lines) {
		return nil, 0, false
	}
	headerLine := strings.TrimSpace(lines[start])
	sepLine := strings.TrimSpace(lines[start+1])
	if !richTableRowPat.MatchString(headerLine) || !richTableRowPat.MatchString(sepLine) {
		return nil, 0, false
	}
	sepCells := splitTableCells(sepLine)
	if len(sepCells) == 0 {
		return nil, 0, false
	}
	aligns := make([]string, len(sepCells))
	for i, cell := range sepCells {
		if !richTableSepCell.MatchString(strings.TrimSpace(cell)) {
			return nil, 0, false
		}
		aligns[i] = parseTableAlign(strings.TrimSpace(cell))
	}
	headerCells := splitTableCells(headerLine)
	if len(headerCells) != len(sepCells) {
		return nil, 0, false
	}
	var rows [][]map[string]any
	rows = append(rows, buildTableRow(headerCells, aligns, true))
	i := start + 2
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			break
		}
		if !richTableRowPat.MatchString(trimmed) {
			break
		}
		cells := splitTableCells(trimmed)
		if len(cells) != len(headerCells) {
			return nil, 0, false
		}
		rows = append(rows, buildTableRow(cells, aligns, false))
		i++
	}
	// GFM requires at least one data row beneath the separator;
	// a header + separator only is not a table.
	if len(rows) < 2 {
		return nil, 0, false
	}
	return map[string]any{
		"type":  "table",
		"cells": rows,
	}, i, true
}

// splitTableCells splits a |...| row into trimmed cell strings.
// The leading/trailing | that frame a row are dropped before split.
func splitTableCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	return strings.Split(row, "|")
}

// parseTableAlign maps a separator cell (":---" / "---:" / ":---:" /
// "---") to the corresponding InputRichBlockTableCell align value.
// Empty string means "default left" — caller omits the `align` field.
func parseTableAlign(sepCell string) string {
	left := strings.HasPrefix(sepCell, ":")
	right := strings.HasSuffix(sepCell, ":")
	switch {
	case left && right:
		return "center"
	case right:
		return "right"
	case left:
		return "left"
	}
	return ""
}

// buildTableRow constructs one row of the cells 2D array.
// isHeader=true marks the row as a header (first row per GFM);
// align strings are dropped when empty so the wire form stays minimal.
func buildTableRow(cells []string, aligns []string, isHeader bool) []map[string]any {
	row := make([]map[string]any, len(cells))
	for j, cell := range cells {
		c := map[string]any{
			"text": inlineToRichText(strings.TrimSpace(cell)),
		}
		if isHeader {
			c["is_header"] = true
		}
		if aligns[j] != "" {
			c["align"] = aligns[j]
		}
		row[j] = c
	}
	return row
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
// rich_message[blocks] array. Returns ok=false only when a block-level
// shape cannot be modelled (malformed fence, table without separator
// row / mismatched column count, char cap exceeded). All inline
// syntax — including footnote refs, image refs, and raw HTML — is
// now handled: footnotes are stripped, images downgrade to url
// entities, raw HTML stays as literal text inside a paragraph. The
// only caller today, `renderRichTurnBlocksLocked`, treats ok=false
// as a request to emit a single paragraph block with the raw body
// — still a rich_message[blocks] payload, never plain text.
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
		case richBulletPat.MatchString(trimmed), richOrderedPat.MatchString(trimmed):
			blk, next, ok := walkList(lines, i)
			if !ok {
				return "", false
			}
			blocks = append(blocks, blk)
			i = next
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
		case richTableRowPat.MatchString(trimmed):
			blk, next, ok := walkTable(lines, i)
			if !ok {
				return "", false
			}
			blocks = append(blocks, blk)
			i = next
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
