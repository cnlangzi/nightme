// Package feishu — usage footer rendering (F-45).
//
// formatSessionFooter composes the SessionContext into a set of
// short markdown lines that the Feishu adapter appends to the
// body of every main-chat OutboundMessage (OutReply / OutResult /
// OutTaskCreate / OutTaskUpdate). See docs/feat/F-45-session-footer.md
// §1.6 / §1.7 for the full rendering rules.
//
// Line 1 (F-45 + F-49):
//
//	🤖 Agent · Model · 🗜 N
//
// Line 2 (F-45 + F-52 token semantics, F-49 + F-52 unified):
//
//	💰:「 in / out · X% · $cost 」
//
// in  = InputTokens + CacheCreationInputTokens + CacheReadInputTokens
//       (the input-side total — Anthropic API exposes these as
//       three independent counters; the "in" stat per
//       https://yb.tencent.com/s/3G6HphjOxM70 is the sum of the
//       three, NOT uncached-only.)
// out = OutputTokens
// X%  = per-turn context-window usage percentage. Computed
//       client-side by AgentSession.AccumulateUsage via the
//       Doc 1 formula: (in + out) / ContextWindow * 100, where
//       ContextWindow is the API-reported model window size
//       (Claude Code: `modelUsage[<model>].contextWindow`).
//       Skipped when ContextWindow == 0 (model didn't report
//       it) or used == 0 — both leave the previous snapshot
//       in place. See F-52 §1.5 / §1.6 for the full rules.
// $cost = API-reported per-turn cost (Claude Code:
//       `result.total_cost_usd`). Forwarded verbatim into
//       agent.UsageEvent.CostUSD — the footer NEVER computes
//       cost client-side (no rate table, no per-model pricing).
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
// Separator inside the 「」 brackets is " · " (middle dot +
// spaces) — visually consistent with F-37 / F-44 footer
// conventions. The 「」 enclosure + "💰:" prefix form a single
// category header for the stats line (vs the previous
// prose-style "💰 " line), so the rendering reads as a compact
// stat bar rather than a free-form sentence.
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
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/gateway"
	"github.com/cnlangzi/nightme/internal/command/gtw"
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
// Line 2: usage stats — 💰:「 in / out · X% · $cost 」. The
// "in / out" convention folds all three input-side counters
// (InputTokens + CacheCreationInputTokens + CacheReadInputTokens)
// into a single number per the Tencent YB doc — the "in" stat
// is the input-side context-window total, not "uncached input
// only". "X%" is the per-turn context-window usage snapshot
// (computed client-side in AccumulateUsage from the
// API-reported ContextWindow + used tokens; see F-52 Doc 1).
// "$cost" is the API-reported total_cost_usd — we never
// compute it client-side.
//
//	💰:「 20.5k / 1.5k · 10.5% · $0.087 」
//
// Each segment is omitted independently:
//   - Line 1: Agent omitted when "". Model omitted when "".
//   - Line 2 segments:
//       in / out: omitted when in == 0 && out == 0 (no usage).
//                 Renders as "<in> / <out>" (zero side shows
//                 "0" — rare in practice, e.g. compaction-only
//                 turn with no new input).
//       X%      : omitted when ctx.ContextWindowPct == 0. The
//                 runtime's three zero-cases (no
//                 EventDone-with-Usage yet / model didn't report
//                 ContextWindow on the latest turn / recent
//                 ResetCumulative or RecordCompaction) all
//                 surface as "no X% segment" rather than a fake
//                 0% (F-52 §1.6).
//       $cost   : omitted when CostUSD == 0 (API didn't report).
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

	// Line 1: identity (🤖 Agent · Model · 🗜 N).
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
	// F-49: append "· 🗜 N" segment when at least one compaction
	// cycle has been observed on this AgentSession. The clamp
	// glyph (U+1F5DC, Unicode "COMPRESSION") is the most literal
	// semantic match — see
	// docs/feat/F-49-compaction-counter.md §1.6 + §1.2. Only
	// rendered when N > 0 (F-45 §1.6 zero-omit convention; new
	// sessions show no compaction segment). The "·" prefix matches
	// the Agent · Model separator rhythm above, so the line reads
	// as one consistent sequence rather than a parenthetical add-on.
	if ctx.CompactionCount > 0 {
		idParts = append(idParts, "·", "🗜", strconv.Itoa(ctx.CompactionCount))
	}
	if len(idParts) > 1 {
		lines = append(lines, strings.Join(idParts, " "))
	}

	// Line 2: usage stats (💰:「 in / out · X% · $cost 」).
	//
	// The "in" stat folds the three input-side counters per the
	// Tencent YB doc convention (uncached + cache_creation +
	// cache_read — i.e. the input-side context-window total).
	// "out" is the generated output. "X%" is the per-turn
	// context-window usage percentage, computed client-side in
	// AccumulateUsage from the API-reported model ContextWindow
	// (F-52 Doc 1 formula). "$cost" is the API-reported per-turn
	// cost (Claude Code: result.total_cost_usd) — forwarded
	// verbatim, NEVER recomputed client-side.
	//
	// Each segment is dropped independently with its owning "·"
	// separator; the final 「」 enclosure is added only when at
	// least one segment is present (F-45 §1.6 zero-omit).
	usageParts := make([]string, 0, 3)
	in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if in > 0 || u.OutputTokens > 0 {
		usageParts = append(usageParts, fmt.Sprintf("%s / %s",
			abbrevTokens(in), abbrevTokens(u.OutputTokens)))
	}
	if ctx.ContextWindowPct > 0 {
		// One decimal place — context-window edges matter
		// (99.5% vs 100.0% is a different signal to the user),
		// and "%.0f%%" would round 99.6 to "100%" misleadingly.
		usageParts = append(usageParts, fmt.Sprintf("%.1f%%", ctx.ContextWindowPct))
	}
	if u.CostUSD > 0 {
		usageParts = append(usageParts, fmt.Sprintf("$%.3f", u.CostUSD))
	}
	if len(usageParts) > 0 {
		lines = append(lines, "💰:「 "+strings.Join(usageParts, " · ")+" 」")
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
