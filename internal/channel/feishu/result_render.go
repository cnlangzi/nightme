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
package feishu

import (
	"encoding/json"
	"fmt"
	"strings"

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
// (lines 2941-2960). Caller (sendResultAsReply) is responsible for the
// 30 KB envelope guard.
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

// truncateRunes is a thin alias for truncateForLog (receipt_event.go)
// kept for caller-clarity in the result_render surface. Single
// source of truth — see receipt_event.go's truncateForLog doc.
func truncateRunes(s string, maxRunes int) string {
	return truncateForLog(s, maxRunes)
}
