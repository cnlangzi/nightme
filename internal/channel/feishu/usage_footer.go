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

// formatSessionFooter renders the SessionContext as one short
// markdown line suitable for the bottom of a main-chat card.
// Returns "" when there is nothing meaningful to show (nil ctx
// or all-zero values), so callers can skip footer concatenation
// cheaply.
//
// Each segment is omitted independently:
//   - Agent: omitted when "". Always first.
//   - Model: omitted when "". Always second (after Agent).
//   - ↓ in: omitted when InputTokens + CacheCreationInputTokens == 0.
//     Value = sum of both — represents the "fresh" input billed at
//     full price (or 1.25x for cache write).
//   - ↻ cached: omitted when CacheReadInputTokens == 0. Suffixed
//     with " cached" so the segment reads naturally even when
//     alone ("↻ 8.2k cached").
//   - ↑ out: omitted when OutputTokens == 0.
//   - Total: omitted when the three token segments above are all
//     omitted (i.e. total == 0). Otherwise shows the sum so users
//     see the absolute number even when individual segments are
//     hidden.
//   - $cost: omitted when CostUSD == 0 (not all bridges report
//     cost; zero means "unknown / not reported"). 3 decimal
//     places to match Anthropic's billing precision.
//
// Token count formatting:
//   - < 1000: raw number ("234")
//   - >= 1000, < 1_000_000: "X.Xk" with one decimal ("12.3k")
//   - >= 1_000_000: "X.XM" with one decimal ("1.4M")
//
// Stable across re-renders — same input always produces the same
// string, so the receipt PATCH diff stays minimal.
func formatSessionFooter(ctx *gateway.SessionContext) string {
	if ctx == nil {
		return ""
	}
	parts := make([]string, 0, 7)

	if ctx.Agent != "" {
		parts = append(parts, ctx.Agent)
	}
	if ctx.Model != "" {
		parts = append(parts, ctx.Model)
	}

	// Token segments.
	u := ctx.CumulativeUsage
	in := u.InputTokens + u.CacheCreationInputTokens
	if in > 0 {
		parts = append(parts, "↓ "+abbrevTokens(in))
	}
	if u.CacheReadInputTokens > 0 {
		parts = append(parts, "↻ "+abbrevTokens(u.CacheReadInputTokens)+" cached")
	}
	if u.OutputTokens > 0 {
		parts = append(parts, "↑ "+abbrevTokens(u.OutputTokens))
	}

	total := in + u.CacheReadInputTokens + u.OutputTokens
	if total > 0 {
		parts = append(parts, fmt.Sprintf("Total %s", abbrevTokens(total)))
	}

	if u.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.3f", u.CostUSD))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
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
