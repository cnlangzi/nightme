package telegram

import "strings"

// buildResultBlocks assembles the rich blocks array for an OutResult
// standalone message. body is walked through markdownToRichBlocks so
// headings / fences / lists / quotes / tables / dividers / inline
// entities all reach the wire as proper rich blocks. When the walker
// rejects (block-cap / char-cap / malformed shape), the body degrades
// to a single paragraph block rather than dropping the message.
//
// When footerLines is non-empty the function tacks on a divider + a
// dedicated footer block (`type: "footer"`). The footer's `text`
// field is a RichText value produced by footerLinesToRichText, so
// PR anchors like `[#42](url)` survive as clickable url entities
// instead of literal text or unparsed `<a href>` HTML — the same
// "plain RichText in caption style" semantics the rich turn's
// footer block uses (see renderRichTurnBlocksLocked).
//
// Returns ok=false only when the body is empty AND the footer is
// empty — equivalent to the existing empty-text silent drop in
// sendOutResultMessage.
func buildResultBlocks(body string, footerLines []string) (string, bool) {
	var blocks []map[string]any

	if body != "" {
		if jsonStr, ok := markdownToRichBlocks(body); ok {
			parsed := mustParseBlocksArray(jsonStr)
			if len(parsed) == 0 {
				return "", false
			}
			blocks = append(blocks, parsed...)
		} else {
			// Walker rejected (block-cap / char-cap / malformed
			// shape). Degrade to one paragraph block with inline
			// entities preserved (PR links / bold / code still
			// render through inlineToRichText).
			blocks = append(blocks, map[string]any{
				"type": "paragraph",
				"text": inlineToRichText(body),
			})
		}
	}

	if len(footerLines) > 0 {
		blocks = append(blocks, map[string]any{"type": "divider"})
		blocks = append(blocks, map[string]any{
			"type": "footer",
			"text": footerLinesToRichText(footerLines),
		})
	}

	if len(blocks) == 0 {
		return "", false
	}
	encoded, err := encodeBlocksArray(blocks)
	if err != nil {
		return "", false
	}
	return encoded, true
}

// footerLinesToRichText flattens N statusbar lines into one RichText
// value suitable for a footer block's `text` field. When every line
// is entity-free the function returns a plain `\n`-joined string (the
// common case); otherwise it returns a `[]any` that interleaves each
// line's inline-rich representation (string or entity array) with
// `\n` plain-string separators.
//
// inlineToRichText returns `string` when no inline entities matched
// (no `[..](url)`, no `**bold**`, etc.) and `[]any` (mix of strings
// + entity maps) when at least one entity matched on the line. The
// git line is the only line that typically carries a `[#N](url)` PR
// anchor, so the mixed path is taken when git is populated.
func footerLinesToRichText(lines []string) any {
	if len(lines) == 0 {
		return ""
	}

	converted := make([]any, 0, len(lines))
	allPlain := true
	for _, line := range lines {
		v := inlineToRichText(line)
		if _, ok := v.(string); !ok {
			allPlain = false
		}
		converted = append(converted, v)
	}

	if allPlain {
		parts := make([]string, len(converted))
		for i, v := range converted {
			parts[i] = v.(string)
		}
		return strings.Join(parts, "\n")
	}

	out := make([]any, 0, 2*len(converted)-1)
	for i, v := range converted {
		if i > 0 {
			out = append(out, "\n")
		}
		if arr, ok := v.([]any); ok {
			out = append(out, arr...)
		} else {
			out = append(out, v)
		}
	}
	return out
}
