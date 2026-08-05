// Package feishu — usage footer rendering (F-45).
//
// formatSessionFooter composes the SessionContext into a single
// short markdown line that the Feishu adapter appends to the body
// of every main-chat OutboundMessage (OutReply / OutResult /
// OutTaskCreate / OutTaskUpdate). See docs/feat/F-45-session-footer.md
// §1.6 for the full rendering rules.
//
// Format C (arrow-based, ASCII only — no emoji labels):
//
//	claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087
//
// Each segment is omitted when its value is zero / empty. Order:
// Agent · Model · in · cached · out · Total · cost. Separator is
// " · " (middle dot + spaces) — visually consistent with F-37 /
// F-44 footer conventions.
package feishu

import (
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/gateway"
)

// formatSessionFooterLines returns the SessionContext footer as a
// slice of non-empty lines suitable for Feishu Card 2.0
// rendering, where each line maps to one plain_text element
// inside a `note` or `div` block (Feishu plain_text does NOT
// honour \n for line breaks within a single element — multi-line
// needs multiple elements). Returns nil when there is nothing
// meaningful to show so callers can skip footer emission cheaply.
//
// Line 1: identity — 🤖 + Agent + Model
//
//	🤖 claude opus-4-5
//
// Line 2: tokens + cost — 💰 + raw numeric segments (no label
// words like "cached" / "Total" — user preference for compact
// display). The arrow glyphs (↓ ↻ ↑) act as inline semantic
// markers without taking label real estate.
//
//	💰 ↓ 12.3k · ↻ 8.2k · ↑ 1.5k · 22.0k · $0.087
//
// Each segment is omitted independently:
//   - Line 1: Agent omitted when "". Model omitted when "".
//   - Line 2 tokens:
//       ↓ in:    InputTokens + CacheCreationInputTokens == 0 → omit
//       ↻ cache: CacheReadInputTokens == 0 → omit
//       ↑ out:   OutputTokens == 0 → omit
//       Total:   omitted when all three token segments above are
//                omitted (i.e. total == 0). Otherwise shows the
//                raw sum so users see the absolute number.
//       $cost:   omitted when CostUSD == 0.
//
// Returns nil when both lines are empty.
//
// Token count formatting:
//   - < 1000:   raw number ("234")
//   - >= 1000, < 1_000_000: "X.Xk" with one decimal ("12.3k")
//   - >= 1_000_000: "X.XM" with one decimal ("1.4M")
//
// Stable across re-renders — same input always produces the same
// slice, so the receipt PATCH diff stays minimal.
func formatSessionFooterLines(ctx *gateway.SessionContext) []string {
	if ctx == nil {
		return nil
	}
	u := ctx.CumulativeUsage
	var lines []string

	// Line 1: identity (🤖 Agent Model).
	idParts := []string{"🤖"}
	if ctx.Agent != "" {
		idParts = append(idParts, ctx.Agent)
	}
	if ctx.Model != "" {
		// Use middle-dot · between Agent and Model — same separator
		// line 2 uses between token segments, so the identity line
		// reads as a consistent footer taxonomy rather than two
		// different rhythms ("🤖 claude opus-4-5" → "🤖 claude ·
		// opus-4-5"). F-37 / F-44 footer convention; matches the
		// rest of the line-2 separator family.
		idParts = append(idParts, "·", ctx.Model)
	}
	if len(idParts) > 1 {
		lines = append(lines, strings.Join(idParts, " "))
	}

	// Line 2: tokens + cost (💰 ↓ X · ↻ X · ↑ X · Total · $X).
	tokParts := make([]string, 0, 6)
	in := u.InputTokens + u.CacheCreationInputTokens
	if in > 0 {
		tokParts = append(tokParts, "↓ "+abbrevTokens(in))
	}
	if u.CacheReadInputTokens > 0 {
		tokParts = append(tokParts, "↻ "+abbrevTokens(u.CacheReadInputTokens))
	}
	if u.OutputTokens > 0 {
		tokParts = append(tokParts, "↑ "+abbrevTokens(u.OutputTokens))
	}
	total := in + u.CacheReadInputTokens + u.OutputTokens
	if total > 0 {
		tokParts = append(tokParts, abbrevTokens(total))
	}
	if u.CostUSD > 0 {
		tokParts = append(tokParts, fmt.Sprintf("$%.3f", u.CostUSD))
	}
	if len(tokParts) > 0 {
		lines = append(lines, "💰 "+strings.Join(tokParts, " · "))
	}

	return lines
}

// formatSessionFooter joins formatSessionFooterLines with "\n"
// for callers that need a single string (OutReply / OutResult
// orphan / overflow paths where the footer is appended to the
// reply text rather than emitted as a card element). Returns ""
// when there is nothing to show.
//
// The string form is useful for plain-text / markdown rendering
// paths where \n is honoured natively; the Feishu receipt card
// path uses formatSessionFooterLines directly because plain_text
// elements do NOT honour \n within a single element.
func formatSessionFooter(ctx *gateway.SessionContext) string {
	return strings.Join(formatSessionFooterLines(ctx), "\n")
}

// abbrevTokens formats a token count into a compact human-readable
// string. Used only by formatSessionFooter; lives here so the
// formatting policy is in one place (test coverage in
// usage_footer_test.go).
//
// Conventions:
//   - n == 0: caller is expected to skip the segment; this function
//     still returns "0" defensively.
//   - n in [1, 999]: integer (no decimal).
//   - n in [1_000, 999_999]: one decimal + "k" (e.g. 12_345 → "12.3k").
//   - n >= 1_000_000: one decimal + "M" (e.g. 1_234_567 → "1.2M").
func abbrevTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
