// Package feishu — F-39 result reply rendering helpers.
//
// sendResultAsReply (defined in adapter.go) calls into this package to choose
// a Feishu msg_type and produce the corresponding body JSON. Dispatch logic
// mirrors cc-connect `platform/feishu/feishu.go::buildReplyContent` so that
// any caller shipping Claude Code's rich markdown output gets the same
// rendering surface cc-connect ships in production.
//
// Dispatch (after SanitizeCardMarkdown):
//
//   ┌─ no markdown indicators ───────────── MsgTypeText (plain text bubble)
//   │
//   ├─ tables > resultCardTableLimit ───── MsgTypePost + tag:"md"
//   │                                      (GFM rendering, no Card 2.0 table cap)
//   │
//   └─ default ──────────────────────────── MsgTypeInteractive (Card 2.0)
//                                           elements split via
//                                           splitMarkdownForDivs @ ≤ divTextCharLimit
//
// MsgTypeText is rare in practice (Claude Code almost always emits markdown).
// MsgTypePost catches the "many tables" edge case where Card 2.0's 5-table
// hard limit would otherwise return error 11310.
//
// envelopeBudget is a defensive ceiling just below the Feishu 30 KB card body
// envelope (larkim NewPatchMessageReqBody etc. SDK resource.go:1381). OutResult
// is naturally ≤ ~26 KB after the perResultMaxBytes cap; this is a guard for
// adversarial inputs, not a hot path.
//
// F-44 also routes OutReply through this package's dispatch (every chunk
// becomes its own ReplyInThreadAndChat message). The shared Sanitize +
// buildResultPayload + truncateForLog helpers eliminate the per-Kind
// duplication; the only OutReply-vs-OutResult difference is the icon prefix
// (OutReply adds none — it's a stream continuation, not a new entry) and the
// envelope cap constant (perReplyMaxBytes vs perResultMaxBytes).
package feishu

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Tunables.
const (
	// resultCardTableLimit is the Feishu Card 2.0 hard cap on table components
	// (5 tables). Beyond this we fall back to MsgTypePost which has no such
	// limit. Mirrors cc-connect `feishu.go:2980` maxCardTables.
	resultCardTableLimit = 5

	// resultCardEnvelopeBudget is the safe upper byte size for a Card 2.0
	// reply we send. Feishu's envelope is 30 KB; we leave 2 KB headroom for
	// card envelope JSON wrapping + future growth.
	resultCardEnvelopeBudget = 28 * 1024

	// perResultMaxBytes caps individual OutResult text coming into the
	// independent reply. Picked to stay well under resultCardEnvelopeBudget
	// for both ASCII and CJK content while leaving room for envelope
	// wrapping. Mirrors receipt.go's perEntryMaxRunes but applied at the
	// OutResult surface (not the receipt LogEntry surface).
	perResultMaxBytes = 6 * 1024

	// perReplyMaxBytes (F-44) caps individual OutReply text coming into the
	// independent ReplyInThreadAndChat. Same value as perResultMaxBytes —
	// OutReply / OutResult share the same envelope (ReplyInThreadAndChat)
	// and the same 28 KB envelope defense. Independent constant so the
	// per-surface semantics stay explicit; the two budgets can diverge in
	// the future if one surface adopts a stricter cap.
	perReplyMaxBytes = 6 * 1024
)

// containsMarkdown reports whether s contains any of the standard markdown
// indicators used by openclaw-lark / cc-connect. Order does not matter; we
// only care whether markdown rendering would survive the round trip.
//
// If false, sendResultAsReply falls back to MsgTypeText (no markdown, no
// rendering benefit from Card 2.0).
//
// Mirrors cc-connect `feishu.go:3033-3044` markdownIndicators / containsMarkdown.
func containsMarkdown(s string) bool {
	for _, ind := range []string{
		"```", "**", "~~", "`", "\n- ", "\n* ", "\n1. ", "\n# ", "---",
	} {
		if strings.Contains(s, ind) {
			return true
		}
	}
	return false
}

// countMarkdownTables counts distinct pipe-delimited tables in s. A table is
// a run of consecutive lines where each line (trimmed) starts and ends with
// `|`. Returns the count of distinct table groups.
//
// Mirrors cc-connect `feishu.go:2982-2998`.
func countMarkdownTables(s string) int {
	if !strings.Contains(s, "|") {
		return 0
	}
	count := 0
	inTable := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		isTableLine := len(trimmed) > 1 && trimmed[0] == '|' && trimmed[len(trimmed)-1] == '|'
		if isTableLine && !inTable {
			count++
			inTable = true
		} else if !isTableLine {
			inTable = false
		}
	}
	return count
}

// buildPostMdJSON renders a Feishu Post (rich-text) message using the `md`
// content tag. Feishu's `md` tag is the most permissive markdown surface in
// the IM API: GFM tables, task lists, code blocks with language hints, etc.
// with no element-count cap and no 30 KB envelope-style ceiling (post messages
// can be much larger).
//
// Used as fallback when Card 2.0's 5-table hard limit kicks in.
//
// Mirrors cc-connect `feishu.go:3000-3015` buildPostMdJSON.
func buildPostMdJSON(content string) (string, error) {
	post := map[string]any{
		"zh_cn": map[string]any{
			"content": [][]map[string]any{
				{
					{"tag": "md", "text": content},
				},
			},
		},
	}
	b, err := json.Marshal(post)
	if err != nil {
		return "", fmt.Errorf("feishu: encode post body: %w", err)
	}
	return string(b), nil
}

// buildResultCardJSON renders the Final Result Reply as Card 2.0 with one or
// more `tag:"markdown"` elements, each ≤ divTextCharLimit runes. Uses
// splitMarkdownForDivs so code blocks and list items stay atomic, paragraph
// boundaries get preferred as split points.
//
// Single element when content fits in one div; multi-element when not.
//
// reuses encodeCardJSON (with SetEscapeHTML(false)) so any intentional inline
// HTML in the sanitized content survives serialization; cc-connect's
// behavior — the JSON encoder defaults to escape `<`/`>`/`&` but encodeCardJSON
// keeps them literal for Card bodies.
func buildResultCardJSON(content string) (string, error) {
	chunks := splitMarkdownForDivs(content, divTextCharLimit)
	elements := make([]map[string]any, 0, len(chunks))
	for _, c := range chunks {
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": c,
		})
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"body":   map[string]any{"elements": elements},
	}
	b, err := encodeCardJSON(card)
	if err != nil {
		return "", fmt.Errorf("feishu: encode result card: %w", err)
	}
	return string(b), nil
}

// buildResultPayload selects the rendering surface and returns msg_type +
// body bytes ready to hand to SDK. Mirrors cc-connect `buildReplyContent`
// (lines 2941-2960). Caller (sendResultAsReply / sendReplyInThreadAndChat)
// is responsible for the 30 KB envelope guard.
func buildResultPayload(sanitized string) (msgType string, body string, err error) {
	if !containsMarkdown(sanitized) {
		// No markdown → plain text bubble. Feishu still renders inline
		// <at> mentions and 4-style runs.
		b, jerr := json.Marshal(map[string]string{"text": sanitized})
		if jerr != nil {
			return "", "", fmt.Errorf("feishu: encode text: %w", jerr)
		}
		return larkim.MsgTypeText, string(b), nil
	}
	if countMarkdownTables(sanitized) > resultCardTableLimit {
		body, err := buildPostMdJSON(sanitized)
		if err != nil {
			return "", "", err
		}
		return larkim.MsgTypePost, body, nil
	}
	body, err = buildResultCardJSON(sanitized)
	if err != nil {
		return "", "", err
	}
	return larkim.MsgTypeInteractive, body, nil
}

// ---------------------------------------------------------------------------
// F-39 markdown sanitize pipeline (formerly card_sanitize.go; F-44 merged).
//
// SanitizeCardMarkdown is the unified entry point for any markdown content
// entering a Feishu Card 2.0 `tag:"markdown"` element. Apply at every code
// path that ships such content (OutResult via sendResultAsReply; OutReply
// via sendReplyInThreadAndChat; OutCard via buildInteractiveCard).
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
// ---------------------------------------------------------------------------

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
	h2to6Re         = regexp.MustCompile(`(?m)^#{2,6} (.+)$`)
	h1Re            = regexp.MustCompile(`(?m)^# (.+)$`)
	h1to3TriggerRe  = regexp.MustCompile(`(?m)^#{1,3} `)
	multiNewlineRe  = regexp.MustCompile(`\n{3,}`)
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

// ---------------------------------------------------------------------------
// truncateForLog — rune-aware clamp with ellipsis suffix.
// ---------------------------------------------------------------------------
//
// F-44 migration: this helper used to live in receipt_event.go. With
// receipt_event.go deleted, it lives here alongside buildResultPayload so
// every outbound markdown surface (OutReply / OutResult / OutCard) shares
// the same clamp semantics. The original signature is preserved byte-for-byte
// so callers don't need to change.
//
// truncateRunes (former thin alias in result_render.go) is removed — its
// sole caller (sendReplyAsMessage) is deleted in F-44. New callers should
// call truncateForLog directly.

// truncateForLog returns s clipped to max characters with an ellipsis
// suffix when truncation occurred. The returned string is always
// valid UTF-8 — we round at the last rune boundary inside the budget
// so we never slice a multi-byte sequence.
//
// F-37: this is rune-aware (was previously byte-based despite the
// comment). `perEntryMaxBytes` and `perEntryMaxRunes` both call this
// function; the unit is "characters" regardless of which const was
// passed. For Chinese / emoji content (where 1 char = 3-4 bytes),
// the cap now correctly counts chars rather than bytes.
//
// F-39 follow-up: also used by sendResultAsReply / sendReplyInThreadAndChat
// (this file's envelope defense) so all outbound reply surfaces share the
// same truncation policy. Single source of truth.
func truncateForLog(s string, max int) string {
	if max <= 0 {
		// No room for any content (not even the ellipsis).
		return ""
	}
	if max == 1 {
		// Only the ellipsis fits.
		return "…"
	}
	// Fast path: every UTF-8 rune is 1-4 bytes. If the byte
	// length fits inside 4×max we know the rune count fits too
	// without allocating a []rune slice. This skips the
	// per-event allocation for the common no-truncation path
	// (most events are well under 600 runes).
	if len(s) <= max*4 {
		// Cheap exact check: if the byte length is also <= max
		// (ASCII case), we know the rune count <= max.
		if len(s) <= max {
			return s
		}
		// Still need a precise rune count for non-ASCII text
		// whose byte length is > max but ≤ 4×max.
		if utf8.RuneCountInString(s) <= max {
			return s
		}
	}
	// Slow path: count runes and truncate.
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	// Leave room for the trailing "…" (1 rune).
	return string(runes[:max-1]) + "…"
}