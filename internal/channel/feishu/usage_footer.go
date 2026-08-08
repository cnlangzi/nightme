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
//
//	(the input-side total — Anthropic API exposes these as
//	three independent counters; the "in" stat per
//	https://yb.tencent.com/s/3G6HphjOxM70 is the sum of the
//	three, NOT uncached-only.)
//
// out = OutputTokens
// X%  = per-turn context-window usage percentage. Bridge-computed
//
//	via the Doc 1 formula:
//	  (InputTokens + OutputTokens + CacheCreation + CacheRead)
//	  / contextWindow * 100
//	where `contextWindow` is a bridge-local value (F-54):
//	claudecode reads it from
//	`modelUsage[<model>].contextWindow`, pi reads it from
//	`get_state.data.model.contextWindow`. The bridge owns this
//	calculation — the runtime does NOT recompute pct, it just
//	passes UsageInfo.ContextWindowPct through to the channel
//	footer. 0 means "not reported" (model didn't expose
//	contextWindow this turn, or pi version lacks the field)
//	and the footer omits X% rather than showing 0%.
//
// $cost = API-reported per-turn cost (Claude Code:
//
//	`result.total_cost_usd`). Forwarded verbatim into
//	agent.UsageInfo.CostUSD — the footer NEVER computes
//	cost client-side (no rate table, no per-model pricing).
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

	"github.com/cnlangzi/nightme/internal/command/gtw"
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
// Line 2: usage stats — 💰:「 new / cache / out · X% (window) · $cost 」
// (F-55.1). F-55.1 splits the original `in` segment into two:
// `new` (tokens not from cache this turn — input_tokens +
// cache_creation_input_tokens) and `cache` (cache hits —
// cache_read_input_tokens). The user sees at a glance whether
// a turn is dominated by cache hits (e.g. an upstream that
// reports cumulative cache_read across the session). Each
// segment is independently omitted when zero; positions
// `new / cache / out` are the meaning, no labels.
//
// Per Anthropic docs the three input-side fields are MUTUALLY
// EXCLUSIVE — each token is counted exactly once across them —
// so the split does NOT introduce overlap; `in == new + cache`
// always. Doc 1 pct (F-52 §1.5 / §1.6) still uses
// `(new + cache + out) / contextWindow`. F-55 appends "(window)"
// so the user sees the denominator and judges upstream
// compatibility-layer mismatches themselves (e.g. `101.6% (200k)`
// against an actual 1M model — the bridge reported 200K, the
// user does the math). nightme does NOT clamp pct > 100%, does
// NOT catalog, does NOT override. "$cost" is the API-reported
// total_cost_usd — we never compute it client-side.
//
//	💰:「 12.3k / 8.2k / 1.5k · 10.5% (200k) · $0.087 」
//
// Each segment is omitted independently:
//   - Line 1: Agent omitted when "". Model omitted when "".
//   - Line 2 segments:
//     new / cache / out: each token class omitted when its
//     count is 0 (F-45 §1.6 zero-omit). The first non-zero
//     segment leads the line; trailing zeros are not
//     padded.
//     X% (window): omitted when Usage.ContextWindowPct == 0. The
//     zero-cases (bridge didn't expose contextWindow
//     this turn, the model simply didn't report it,
//     or pi is older than the get_state contract) all
//     surface as "no X% segment" rather than a fake
//     0% (F-52 §1.6, F-54 §1.4). F-55 appends `(window)`
//     when Usage.ContextWindow > 0 so the user can read
//     the denominator; pct > 100% renders verbatim
//     without clamp or warning (see F-55).
//     $cost   : omitted when CostUSD == 0 (API didn't report).
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
	// F-49 compaction tracking removed: the "· 🗜 N" segment is
	// no longer rendered. The runtime dropped its
	// compactionCount bookkeeping; bridges no longer emit
	// EventAgentCompaction. Line 1 retains Agent · Model only.
	_ = strconv.Itoa // keep import stable during F-49 cleanup
	if len(idParts) > 1 {
		lines = append(lines, strings.Join(idParts, " "))
	}

	// Compaction tracking (🗜 N) was removed when the
	// runtime-side compaction chain was dropped. F-49 §1.6 + §1.2
	// no longer apply; Line 1 retains Agent · Model only.

	// Line 2: usage stats (💰:「 in / out · X% · $cost 」).
	//
	// Renders the per-turn Usage snapshot stamped on the
	// OutboundMessage (ctx.Usage, populated by
	// gateway.Translate from AgentResultEvent.Usage / AgentDoneEvent.Usage
	// on the bridge event). The runtime is a passive
	// pass-through — it does NOT aggregate across turns.
	//
	// The "in" stat folds the three input-side counters per the
	// Tencent YB doc convention (uncached + cache_creation +
	// cache_read — i.e. the input-side context-window total).
	// "out" is the generated output. "X% (window)" is the
	// bridge-computed per-turn context-fill percentage followed
	// by the API-reported denominator (F-55; bridge reads
	// `contextWindow` — claudecode from
	// `modelUsage.contextWindow`, pi from
	// `get_state.data.model.contextWindow` (F-54) — fills
	// UsageInfo.ContextWindowPct AND UsageInfo.ContextWindow
	// verbatim). nightme does NOT clamp pct > 100%, does NOT
	// catalog, does NOT override — the user judges upstream
	// compatibility-layer mismatches themselves. "$cost" is
	// the API-reported per-turn cost (Claude Code:
	// result.total_cost_usd) — forwarded verbatim, NEVER
	// recomputed client-side.
	//
	// When ctx.Usage is nil (e.g. OutReply chunks during
	// streaming have no usage), the entire Line 2 is omitted
	// (F-45 §1.6 zero-omit). Each segment is dropped
	// independently with its owning "·" separator; the final
	// 「」 enclosure is added only when at least one segment is
	// present.
	if u := ctx.Usage; u != nil {
		usageParts := make([]string, 0, 3)
		// F-55.1: split `in` into `new` (tokens not from cache this
		// turn: input_tokens + cache_creation_input_tokens) and
		// `cache` (tokens read from existing cache:
		// cache_read_input_tokens). Per Anthropic docs the three
		// input-side fields are MUTUALLY EXCLUSIVE — each token is
		// counted exactly once. nightme does NOT recompute pct on
		// the split — Doc 1 formula still uses
		// (new + cache + out) / contextWindow.
		//
		// F-55.1 render: three numbers separated by " / ", no
		// labels. The position (new / cache / out) is the meaning.
		// Each segment independently omitted when zero (F-45 §1.6
		// zero-omit); when cache == 0 the layout collapses to
		// `new / out` (the pre-F-55.1 format). User can read at a
		// glance whether a turn is dominated by cache hits (e.g.
		// MiniMax `cache_read_input_tokens` reporting cumulative
		// session totals — see F-55 §1.2).
		new := u.InputTokens + u.CacheCreationInputTokens
		cacheRead := u.CacheReadInputTokens
		tokens := make([]string, 0, 3)
		if new > 0 {
			tokens = append(tokens, abbrevTokens(new))
		}
		if cacheRead > 0 {
			tokens = append(tokens, abbrevTokens(cacheRead))
		}
		if u.OutputTokens > 0 {
			tokens = append(tokens, abbrevTokens(u.OutputTokens))
		}
		if len(tokens) > 0 {
			usageParts = append(usageParts, strings.Join(tokens, " / "))
		}
		if u.ContextWindowPct > 0 {
			// F-55: append `(window)` after X% so the user can see
			// the denominator and judge upstream compatibility-layer
			// mismatches themselves (e.g. `101.6% (200k)` against an
			// actual 1M model). nightme does NOT clamp / catalog /
			// override — CLI Agent 报什么就显示什么,错了让用户
			// 自己计算. One decimal place: 99.5% vs 100.0% is a
			// different signal to the user, and "%.0f%%" would
			// round 99.6 to "100%" misleadingly.
			usageParts = append(usageParts, fmt.Sprintf("%.1f%% (%s)", u.ContextWindowPct, abbrevWindow(u.ContextWindow)))
		}
		if u.CostUSD > 0 {
			usageParts = append(usageParts, fmt.Sprintf("$%.3f", u.CostUSD))
		}
		if len(usageParts) > 0 {
			lines = append(lines, "💰:「 "+strings.Join(usageParts, " · ")+" 」")
		}
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
//     Integer multiples (e.g. 1_000, 200_000, 999_999) drop the
//     trailing `.0` so the rendered string is `1k` / `200k` / `1000k`
//     rather than `1.0k` / `200.0k` / `1000.0k`.
//   - n >= 1_000_000: same rule with `M` suffix (e.g.
//     1_234_567 → "1.2M", 1_000_000 → "1M").
func abbrevTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return abbrevOneDecimal(float64(n)/1_000_000, "M")
	case n >= 1_000:
		return abbrevOneDecimal(float64(n)/1_000, "k")
	default:
		return fmt.Sprintf("%d", n)
	}
}

// abbrevWindow formats a model context-window size into a compact
// human-readable string. Same conventions as abbrevTokens — both
// helpers exist as separate symbols so the formatting policy is
// in one place per kind (test coverage in usage_footer_test.go).
//
// F-55: used by formatSessionFooterLines to render the `(window)`
// segment alongside `X%` so the user can read the denominator and
// judge upstream compatibility-layer mismatches themselves.
//
//   - n in [1, 999]: integer (no decimal).
//   - n in [1_000, 999_999]: one decimal + "k" (e.g. 12_345 → "12.3k",
//     200_000 → "200k" — no trailing `.0`).
//   - n >= 1_000_000: same rule with `M` suffix (e.g.
//     1_234_567 → "1.2M", 1_000_000 → "1M").
func abbrevWindow(n int) string {
	switch {
	case n >= 1_000_000:
		return abbrevOneDecimal(float64(n)/1_000_000, "M")
	case n >= 1_000:
		return abbrevOneDecimal(float64(n)/1_000, "k")
	default:
		return fmt.Sprintf("%d", n)
	}
}

// abbrevOneDecimal formats `v` with one decimal place and appends
// `unit`. Integer multiples (e.g. v=1.0, v=200.0, v=999.999)
// drop the trailing `.0` so the rendered string is `1M` (not
// `1.0M`), `200k` (not `200.0k`), `1000k` (not `1000.0k`).
// Centralises the rule so abbrevTokens and abbrevWindow stay
// byte-for-byte aligned.
//
// Implemented via suffix-trim rather than math.Trunc because
// `math.Trunc(999.999)` ≠ `math.Trunc(999.999)` (obviously), so
// a v==Trunc(v) check doesn't fire for the rounding case
// 999_999 / 1000 → 999.999 which `%.1f` renders as "1000.0".
// Trimming the formatted string is robust to this.
func abbrevOneDecimal(v float64, unit string) string {
	s := fmt.Sprintf("%.1f%s", v, unit)
	zeroSuffix := ".0" + unit
	if strings.HasSuffix(s, zeroSuffix) {
		s = s[:len(s)-len(zeroSuffix)] + unit
	}
	return s
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
//   - parent, period.
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
