// Package feishu — F-37 multi-div content split. Splits long
// entry content into multiple card `div` elements, each ≤ 1000
// runes, so the receipt can render content up to the 30 KB
// envelope ceiling while keeping `lark_md` rendering.
//
// Preservation guarantees:
//   - Code blocks (```...```) are never split internally — a
//     long code block is flushed as its own div, even if it
//     exceeds maxRunes.
//   - List items (single - / 1. / * line) are never split
//     internally — the entire line stays in one div.
//
// Soft split points (in priority order):
//   1. Paragraph boundary (\n\n)         — handled by splitMarkdownForDivs
//   2. Line boundary (\n)                — handled by hardSplitRunes
//   3. Punctuation followed by space     — handled by hardSplitRunes
//   4. Space                             — handled by hardSplitRunes
//
// Fallback: hard split at rune boundary at maxRunes (only applies
// to non-atomic blocks — paragraphs that exceed the limit on
// their own without paragraph boundaries).
//
// Reference: docs/feat/F-37-multi-div-content-split.md.
package feishu

import (
	"strings"
	"unicode/utf8"
)

// block is a unit of markdown content split out by
// splitTopLevelBlocks. atomic=true means the block cannot be
// split internally (code blocks, list blocks); atomic=false
// means the block is a paragraph that can be hard-split if it
// exceeds maxRunes on its own.
type block struct {
	text   string
	atomic bool
}

// splitMarkdownForDivs splits markdown text into chunks, each ≤ maxRunes.
// Each chunk is a paragraph-boundary-respecting markdown fragment that
// renders correctly when wrapped in a Feishu card div element with
// lark_md content.
//
// Empty input returns an empty slice. Input that fits in maxRunes
// returns a single-element slice. Otherwise, the input is split at
// the soft points listed in the package comment, with code blocks
// and list items preserved as atomic units.
//
// Atomic blocks (code / list blocks) that exceed maxRunes are emitted
// as a single chunk regardless — they may be longer than maxRunes.
// This is intentional: a code block in the middle of an explanation
// must stay intact, even if it's a giant stack trace.
func splitMarkdownForDivs(text string, maxRunes int) []string {
	if text == "" {
		return nil
	}
	if maxRunes <= 0 {
		return []string{text}
	}
	// Fast path: byte length is a tight upper bound for short,
	// ASCII-heavy text. Avoid a full rune scan when the input is
	// obviously small (the common case for receipt entries).
	if len(text) <= maxRunes {
		return []string{text}
	}
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}

	var chunks []string
	var current strings.Builder
	currentRunes := 0

	flush := func() {
		if current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
			currentRunes = 0
		}
	}

	// 1. 处理 atomic blocks (code / list): 整块加, 但若超过 maxRunes
	//    则按行拆开, 避免单 chunk 超过 Feishu div.text 1000 char
	//    硬限。代码块按行拆保持行边界, 列表项按行拆保持项边界。
	for _, b := range splitTopLevelBlocks(text) {
		if b.atomic {
			// 段落边界 flush, atomic block 整块加 (fits case)
			if utf8.RuneCountInString(b.text) <= maxRunes {
				flush()
				chunks = append(chunks, b.text)
				continue
			}
			// 超过 maxRunes: 按行逐行加入 (行边界 = soft 切点 #2)
			flush()
			for _, line := range strings.Split(b.text, "\n") {
				lineRunes := utf8.RuneCountInString(line)
				if lineRunes > maxRunes {
					// 单行本身就超长, 硬切
					flush()
					chunks = append(chunks, hardSplitRunes(line, maxRunes)...)
					continue
				}
				// 行间 separator (新行)
				sep := 0
				if current.Len() > 0 {
					sep = 1 // "\n"
				}
				if currentRunes+lineRunes+sep > maxRunes && current.Len() > 0 {
					flush()
				}
				if current.Len() > 0 {
					current.WriteByte('\n')
					currentRunes++
				}
				current.WriteString(line)
				currentRunes += lineRunes
			}
			flush()
			continue
		}
		// 2. 非 atomic (paragraph): 累加 / 拆分
		blockRunes := utf8.RuneCountInString(b.text)
		if blockRunes > maxRunes {
			// 单 paragraph 超长,硬切
			flush()
			chunks = append(chunks, hardSplitRunes(b.text, maxRunes)...)
			continue
		}
		sep := 0
		if current.Len() > 0 {
			sep = 2 // "\n\n"
		}
		if currentRunes+blockRunes+sep > maxRunes && current.Len() > 0 {
			flush()
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
			currentRunes += 2
		}
		current.WriteString(b.text)
		currentRunes += blockRunes
	}
	flush()
	return chunks
}

// hardSplitRunes hard-splits text into chunks of at most maxRunes
// runes each. Soft split priorities 2-4 from the package comment
// are honoured in order: line boundary, punctuation followed by
// space, space. Fallback: rune boundary.
func hardSplitRunes(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		return []string{text}
	}
	if utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	var chunks []string
	for {
		// Fast path: byte length upper-bound check; if the head
		// is small enough, take the whole thing and stop.
		if utf8.RuneCountInString(text) <= maxRunes {
			chunks = append(chunks, text)
			return chunks
		}
		cut, ok := findSoftCut(text, maxRunes)
		if !ok {
			// No soft boundary inside the window: hard cut at
			// rune boundary. Avoids a full []rune() allocation
			// for the common case of small inputs.
			runes := []rune(text)
			if len(runes) <= maxRunes {
				chunks = append(chunks, text)
				return chunks
			}
			chunks = append(chunks, string(runes[:maxRunes]))
			text = string(runes[maxRunes:])
			continue
		}
		// cut is a byte index that sits right after a soft
		// boundary character (newline, " " after punctuation,
		// or plain " "); keep the boundary in the head so the
		// next chunk starts cleanly.
		head := text[:cut]
		// Trim a single trailing space/newline from the head
		// to avoid leaving an empty rune at the start of the
		// next chunk.
		if tail := text[cut-1:cut]; tail == " " || tail == "\n" {
			head = text[:cut-1]
		}
		chunks = append(chunks, head)
		text = text[cut:]
	}
}

// findSoftCut returns a byte index cut (with 0 < cut <= len(s))
// such that text[:cut] fits within maxRunes runes and the cut
// sits immediately after a soft boundary: line break, punctuation
// followed by space, or plain space. The ok result is false when
// no soft boundary exists within the first maxRunes runes —
// caller should hard-cut at the rune boundary.
func findSoftCut(s string, maxRunes int) (int, bool) {
	// Walk runes up to maxRunes+1 (the +1 lets us recognise a
	// soft boundary that lives at the very edge of the window).
	var bestPunct, bestSpace int
	bestPunct, bestSpace = 0, 0
	seen := 0
	var softAfterNewline bool
	lastNewline := 0
	prev := rune(-1)
	for i, r := range s {
		if seen > maxRunes+1 {
			break
		}
		seen++
		switch {
		case r == '\n':
			lastNewline = i + len(string(r))
			softAfterNewline = true
			bestSpace = lastNewline
		case r == ' ' && (prev == '.' || prev == ',' || prev == ';' || prev == ':' || prev == '!' || prev == '?' || prev == ')' || prev == ']' || prev == '}' || prev == '"' || prev == '\''):
			if seen <= maxRunes+1 {
				bestPunct = i + len(string(r))
			}
		case r == ' ':
			if seen <= maxRunes+1 {
				bestSpace = i + len(string(r))
			}
		}
		prev = r
	}
	// Decide which cut to honour. We want the cut to sit inside
	// the maxRunes rune window (so the head chunk is not too
	// long). Walk and pick the highest-priority cut that keeps
	// the head within maxRunes.
	if softAfterNewline && lastNewline > 0 {
		// A newline is always a clean cut; we already have its
		// byte position.
		return lastNewline, true
	}
	if bestPunct > 0 {
		// Verify head fits in runes.
		if utf8.RuneCountInString(s[:bestPunct]) <= maxRunes {
			return bestPunct, true
		}
	}
	if bestSpace > 0 {
		if utf8.RuneCountInString(s[:bestSpace]) <= maxRunes {
			return bestSpace, true
		}
	}
	return 0, false
}

// splitTopLevelBlocks splits text at top-level markdown boundaries,
// preserving code blocks, list blocks, and paragraphs as atomic
// units. Returns a slice of blocks tagged with whether each block
// is atomic (cannot split internally).
func splitTopLevelBlocks(text string) []block {
	lines := strings.Split(text, "\n")
	var blocks []block
	var current strings.Builder
	currentRunes := 0
	currentAtomic := false

	flushParagraph := func() {
		if current.Len() > 0 {
			blocks = append(blocks, block{text: current.String(), atomic: currentAtomic})
			current.Reset()
			currentRunes = 0
			currentAtomic = false
		}
	}

	inCodeBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Fence detection: ``` opens or closes a code block.
		if strings.HasPrefix(trimmed, "```") {
			if !inCodeBlock {
				// 切到 code block 之前先 flush 之前的 paragraph
				flushParagraph()
				currentAtomic = true
			}
			if current.Len() > 0 {
				current.WriteByte('\n')
				currentRunes++
			}
			current.WriteString(line)
			currentRunes += utf8.RuneCountInString(line)
			inCodeBlock = !inCodeBlock
			if !inCodeBlock {
				// code block 结束,flush
				flushParagraph()
			}
			continue
		}
		if inCodeBlock {
			// code block 内部,无脑加
			if current.Len() > 0 {
				current.WriteByte('\n')
				currentRunes++
			}
			current.WriteString(line)
			currentRunes += utf8.RuneCountInString(line)
			continue
		}
		// 列表项
		if isListItem(trimmed) {
			if current.Len() > 0 {
				// 列表接列表,继续累加
				current.WriteByte('\n')
				currentRunes++
			}
			current.WriteString(line)
			currentRunes += utf8.RuneCountInString(line)
			continue
		}
		// 段落边界: 空行
		if trimmed == "" {
			flushParagraph()
			continue
		}
		// 普通段落行
		if current.Len() > 0 {
			current.WriteByte('\n')
			currentRunes++
		}
		current.WriteString(line)
		currentRunes += utf8.RuneCountInString(line)
	}
	flushParagraph()
	return blocks
}

// isListItem returns true if the line looks like a markdown
// list item after indentation is stripped. Supports unordered
// (-, *, +) and ordered (1. 2. ...) forms.
func isListItem(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	t := trimmed
	if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ ") {
		return true
	}
	// Ordered: digits followed by ". "
	for i := 0; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			if i > 0 && t[i] == '.' && i+1 < len(t) && t[i+1] == ' ' {
				return true
			}
			return false
		}
	}
	return false
}
