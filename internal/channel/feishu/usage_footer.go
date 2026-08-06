// Package feishu — usage footer rendering (F-45).
//
// formatSessionFooter composes the SessionContext into a set of
// short markdown lines that the Feishu adapter appends to the
// body of every main-chat OutboundMessage (OutReply / OutResult /
// OutTaskCreate / OutTaskUpdate). See docs/feat/F-45-session-footer.md
// §1.6 / §1.7 for the full rendering rules.
//
// Line 1 (F-45):
//
//	🤖 Agent · Model
//
// Line 2 (F-45):
//
//	💰 ↓ in · ↻ cached · ↑ out · Total · $cost
//
// Line 3 (F-48 follow-up to F-45):
//
//	📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
//
// Each segment is omitted when its value is zero / empty /
// unknown. Order is fixed within each line; lines themselves are
// omitted entirely when empty (so a non-git workspace still gets
// lines 1-2, and a brand-new session with no Model yet gets no
// line 2 — but line 1 always surfaces when Agent is known).
// Separator is " · " (middle dot + spaces) — visually consistent
// with F-37 / F-44 footer conventions.
//
// F-48 line 3 workspace segment intentionally has NO prefix
// (`~`, `/`, etc.) — the path is just displayed as-is with the
// "≤2 keep, >2 last 2" shortening rule. Adding a `~` for HOME
// paths is misleading when the workspace isn't under HOME
// (different operators, containerised sessions, non-standard
// HOME layout). See docs/feat/F-45-session-footer.md §1.7.
package feishu

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/gtw"
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

	// Line 3 (F-48): git tracking — workspace · branch · dirty counts.
	// formatGitLine returns "" when ctx.Workspace is empty, ctx.GitStatus
	// is nil, or all sub-segments omit — in which case we drop the line.
	if gl := formatGitLine(ctx); gl != "" {
		lines = append(lines, gl)
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

// formatWorkspacePath renders the AgentSession's Cwd into a
// short, human-readable label for footer line 3.
//
// Rules (F-48 §1.7, simplified per Devin feedback 2026-08-06):
//
//   - NO prefix is added — neither "~" for HOME paths nor "/"
//     for absolute paths. The path is just shown as-is.
//     Rationale: a `~` prefix is misleading when the workspace
//     isn't under HOME (different operators, containerised
//     sessions, non-standard HOME layout). Skipping both prefixes
//     keeps the segment unambiguous: "code/nightme" is the leaf
//     + parent, period.
//   - ≤ 2 path components → display all components.
//     ("/home/devin" → "home/devin"; "/tmp/foo" → "tmp/foo").
//   - > 2 path components → keep only the last 2.
//     ("/home/devin/code/nightme" → "code/nightme";
//     "/home/devin/code/nightme/internal" → "nightme/internal";
//     "/tmp/a/b/c" → "b/c").
//   - "" or "." → "" (caller omits the segment).
//   - "/" → "" (no components to show).
//
// The "last two when long" rule lets users see the leaf + its
// parent (enough to disambiguate), without a long absolute path
// that won't fit in the narrow Feishu plain_text element.
func formatWorkspacePath(absPath string) string {
	if absPath == "" || absPath == "." {
		return ""
	}
	cleaned := filepath.Clean(absPath)
	parts := strings.Split(cleaned, string(filepath.Separator))
	// Drop leading empty from the leading separator so the
	// "first component" is the first directory, not "".
	if len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	switch {
	case len(parts) == 0:
		return ""
	case len(parts) <= 2:
		return strings.Join(parts, string(filepath.Separator))
	default:
		return strings.Join(parts[len(parts)-2:], string(filepath.Separator))
	}
}

// formatGitLine renders footer line 3 — the per-stamp git status
// snapshot — and returns "" when there is nothing meaningful to
// show (no Workspace, no GitStatus, no git segment).
//
// Output (when non-empty):
//
//	📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
//
// Omit rules (each segment is dropped independently; the line
// itself is dropped when ALL segments would be empty):
//
//   - Workspace == ""           → entire line omitted
//   - ⎇ <branch|?>              → always present when line is shown
//   - ↑ N (uncommitted)        → omitted when Uncommitted == 0
//   - ? N (untracked)          → omitted when Untracked == 0
//   - ⇡ N (unpushed)           → omitted when !HasUpstream OR AheadOfRemote == 0
//
// Returns "" when ctx == nil or ctx.GitStatus == nil (caller
// drops the line). Detached HEAD renders the branch segment as
// "?" — see CollectStatus / parsePorcelainBranchStatus.
func formatGitLine(ctx *gateway.SessionContext) string {
	if ctx == nil {
		return ""
	}
	if ctx.Workspace == "" {
		return ""
	}
	// Non-Git workspaces (Workspace set, GitStatus nil) must drop
	// the entire line — rendering "📁 <ws> · ⎇ ?" would imply
	// Git tracking is available when it's not (caller couldn't
	// collect because the workspace isn't a git repo, git is
	// missing, or git failed). F-48 documented contract:
	// "Workspace=='' OR GitStatus==nil → entire line omitted."
	// The "⎇ ?" rendering is reserved for detached HEAD inside
	// an actual git repo (Branch=="" + GitStatus!=nil).
	if ctx.GitStatus == nil {
		return ""
	}
	ws := formatWorkspacePath(ctx.Workspace)
	if ws == "" {
		return ""
	}

	parts := []string{"📁 " + ws}

	// Branch segment (always present when line is shown).
	branch := "?"
	if ctx.GitStatus.Branch != "" {
		branch = ctx.GitStatus.Branch
	}
	parts = append(parts, "⎇ "+branch)

	if ctx.GitStatus.Uncommitted > 0 {
		parts = append(parts, fmt.Sprintf("↑ %d", ctx.GitStatus.Uncommitted))
	}
	if ctx.GitStatus.Untracked > 0 {
		parts = append(parts, fmt.Sprintf("? %d", ctx.GitStatus.Untracked))
	}
	if ctx.GitStatus.HasUpstream && ctx.GitStatus.AheadOfRemote > 0 {
		parts = append(parts, fmt.Sprintf("⇡ %d", ctx.GitStatus.AheadOfRemote))
	}

	return strings.Join(parts, " · ")
}

// _ ensures the gtw import is recognised as "used" by the
// compiler: formatGitLine accesses ctx.GitStatus typed as
// *gtw.GitStatusSnapshot but never writes `gtw.X` by name, so
// Go's unused-import check (correctly) rejects the import
// without an explicit reference. Field-type-only access doesn't
// satisfy the import-use check.
var _ = (*gtw.GitStatusSnapshot)(nil)
