// Package statusbar renders the per-turn session footer as a
// short slice of plain-text lines. Both the Feishu adapter (card
// footer) and the Telegram adapter (per-message trailer) call
// StatusBarLines on the OutboundMessage at send time.
//
// Field layout (all read from *messages.OutboundMessage flat
// fields — StatusBar wrapper is gone after F-CLAUDE-PRINT-002):
//
//	Line 1: 🤖: Agent · Model · SessionID       (Identity)
//	Line 2: 💰:「 new / cache / out · X% (window) · $cost 」   (Usage)
//	Line 3: 📁: ws · ⎇ branch · + N · − N · ± N · ? N · ! N · ⇡ N · [#PR](url)   (GitStatus)
//
// Each segment is omitted when its value is zero / empty /
// unknown. Order is fixed within each line; lines themselves
// are omitted entirely when empty (so a non-git workspace still
// gets lines 1-2, and a brand-new session with no Model yet
// gets no line 2 — but line 1 always surfaces when Agent is
// known). Returns nil when there is nothing meaningful to show
// so callers can skip footer emission cheaply.
//
// Stable across re-renders — same input always produces the
// same slice, so receipt PATCH / Telegram editMessageText diffs
// stay minimal.
//
// See docs/feat/F-45-session-footer.md for the original Feishu
// contract; docs/channel/telegram.md §18 for the Telegram
// adapter's per-message trailer usage.
package statusbar

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/messages"
)

// PanelMaxWidth anchors the top/bottom bars so they never wrap
// on mobile. 2026-08 user feedback on Android client iteration:
//   - 30 → wraps
//   - 15 → wraps
//   - 8  → no wrap but feels too sparse
//   - 16 → current (2026-08-22 user feedback "再增加一倍");
//          still narrow enough for Android chat bubble but
//          visually substantive as a frame marker.
// Content lines render verbatim and may extend past the bars
// (acceptable per "宁愿短点也不要折行" — bars short, content
// can wrap).
const PanelMaxWidth = 16

// RenderPanel wraps the StatusBar lines in a four-corner ASCII
// "frame marker": ┌ ┐ on top, └ ┘ on bottom, with NO
// side-bar `│` characters (open left/right edges). The four
// corners mark "this is session metadata" without forcing the
// content lines to a fixed width — Telegram's proportional
// font wraps at ~35-40 chars on phones, and any forced
// alignment on the sides would visually break when content
// lines wrap.
//
// Width is max(PanelMinWidth, longest_line) clamped to
// PanelMaxWidth — no `│`-side padding overhead. Returns ""
// for an empty lines slice so callers can skip the trailer
// cheaply.
//
// Design choice (left-closed + chevron tail): left side has
// `┌` / `└` square-anchored corners so the panel "starts
// here". Right side ends with `›` chevron tails — not a
// closed `┐` / `┘` corner — because StatusBar content can
// extend right and we don't want to imply a hard boundary
// that doesn't exist. The chevron tail reads as "this status
// info continues to the right" (CLI / editor fold-marker
// convention).
func RenderPanel(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	// Bars (top + bottom) are anchored to PanelMaxWidth so
	// they never wrap on mobile. Content lines render verbatim
	// — long lines may wrap on narrow screens, which is
	// acceptable. Per user preference: bars must not wrap,
	// content can.
	width := PanelMaxWidth
	barLen := width - 2 // 1 (left corner) + barLen dashes + 1 (right chevron)
	top := "┌" + strings.Repeat("─", barLen) + "›"
	bottom := "└" + strings.Repeat("─", barLen) + "›"
	parts := make([]string, 0, len(lines)+2)
	parts = append(parts, top)
	// Each content line gets a 2-space left indent so the icon
	// (🤖/💰/📁) doesn't sit flush against the left edge of
	// the panel — leaves visual whitespace that frames the
	// content as "indented status", not "bordered box". Long
	// lines extend past the bars and wrap on narrow screens.
	for _, line := range lines {
		parts = append(parts, "  "+line)
	}
	parts = append(parts, bottom)
	return strings.Join(parts, "\n")
}

// StatusBarLines renders the per-turn session footer from an
// OutboundMessage's flat fields. Returns nil when there is
// nothing meaningful to show so callers can skip footer
// emission cheaply.
//
// Line 1: identity — 🤖: + Agent + Model + SessionID
//
//	🤖: claude opus-4-5 abc123-...
//
// Line 2: usage stats — 💰:「 new / cache / out · X% (window) · $cost 」
// (F-55.1). F-55.1 splits the original `in` segment into two:
// `new` (tokens not from cache this turn — input_tokens +
// cache_creation_input_tokens) and `cache` (cache hits —
// cache_read_input_tokens). The user sees at a glance whether
// a turn is dominated by cache hits. Each segment is
// independently omitted when zero; positions `new / cache /
// out` are the meaning, no labels.
//
// Per Anthropic docs the three input-side fields are MUTUALLY
// EXCLUSIVE — each token is counted exactly once across them —
// so the split does NOT introduce overlap; `in == new + cache`
// always. Doc 1 pct still uses `(new + cache + out) / contextWindow`.
// F-55 appends "(window)" so the user sees the denominator
// and judges upstream compatibility-layer mismatches
// themselves. nightme does NOT clamp pct > 100%, does NOT
// catalog, does NOT override. "$cost" is the API-reported
// total_cost_usd — we never compute it client-side.
//
//	💰:「 12.3k / 8.2k / 1.5k · 10.5% (200k) · $0.087 」
//
// Line 3: git tracking — 📁: ws · ⎇ branch · + N · − N · ± N · ? N · ! N · ⇡ N · [#PR](url)
//
//	📁: code/nightme · ⎇ main · + 2 · ± 3 · − 1 · ? 4 · ⇡ 5 · [#42](url)
//
// See formatGitLine for the per-segment omit rules.
//
// Token count formatting (F-45 §1.6):
//   - < 1000:   raw number ("234")
//   - >= 1000, < 1_000_000: "X.Xk" with one decimal ("12.3k")
//   - >= 1_000_000: "X.XM" with one decimal ("1.4M")
//
// Stable across re-renders — same input always produces the same
// slice, so the receipt PATCH diff stays minimal.
func StatusBarLines(msg *messages.OutboundMessage) []string {
	if msg == nil {
		return nil
	}
	var lines []string

	// Line 1: identity (🤖: Agent · Model · SessionID).
	if msg.AgentName != "" || msg.Model != "" || msg.SessionID != "" {
		idParts := []string{"🤖:"}
		if msg.AgentName != "" {
			idParts = append(idParts, msg.AgentName)
		}
		if msg.Model != "" {
			idParts = append(idParts, "·", msg.Model)
		}
		// F-56: append the agent's own session id as a trailing
		// identity segment.
		if msg.SessionID != "" {
			idParts = append(idParts, "·", msg.SessionID)
		}
		if len(idParts) > 1 {
			lines = append(lines, strings.Join(idParts, " "))
		}
	}

	// Line 2: usage stats (💰:「 in / out · X% · $cost 」).
	if msg.Usage != nil {
		ub := msg.Usage
		usageParts := make([]string, 0, 3)
		// F-55.1: split `in` into `new` (tokens not from cache
		// this turn: input_tokens + cache_creation_input_tokens)
		// and `cache` (tokens read from existing cache:
		// cache_read_input_tokens).
		newTokens := ub.InputTokens + ub.CacheCreationInputTokens
		cacheRead := ub.CacheReadInputTokens
		tokens := make([]string, 0, 3)
		if newTokens > 0 {
			tokens = append(tokens, abbrevTokens(newTokens))
		}
		if cacheRead > 0 {
			tokens = append(tokens, abbrevTokens(cacheRead))
		}
		if ub.OutputTokens > 0 {
			tokens = append(tokens, abbrevTokens(ub.OutputTokens))
		}
		if len(tokens) > 0 {
			usageParts = append(usageParts, strings.Join(tokens, " / "))
		}
		if ub.ContextWindowPct > 0 {
			// F-55: append `(window)` after X% so the user can
			// see the denominator and judge upstream
			// compatibility-layer mismatches themselves.
			// One decimal place: 99.5% vs 100.0% is a different
			// signal to the user, and "%.0f%%" would round
			// 99.6 to "100%" misleadingly.
			usageParts = append(usageParts, fmt.Sprintf("%.1f%% (%s)", ub.ContextWindowPct, abbrevWindow(ub.ContextWindow)))
		}
		if ub.CostUSD > 0 {
			usageParts = append(usageParts, fmt.Sprintf("$%.3f", ub.CostUSD))
		}
		if len(usageParts) > 0 {
			lines = append(lines, "💰:「 "+strings.Join(usageParts, " · ")+" 」")
		}
	}

	// Line 3 (F-48 + F-49): git tracking — workspace · branch ·
	// dirty counts · PR number. formatGitLine folds the PR /
	// MR number in as its last segment so the footer stays at
	// three lines regardless of PR state. Returns "" when
	// msg.GitStatus is nil OR Workspace is empty OR Snapshot is
	// nil OR all sub-segments omit — caller drops the entire
	// line.
	if gl := formatGitLine(msg.GitStatus); gl != "" {
		lines = append(lines, gl)
	}

	return lines
}

// formatPRSegment renders the trailing `[#N](url)` PR / MR
// segment of the workspace footer line. Reads gs.PullRequest
// and returns "" when nil / Number<=0 / URL empty.
//
// The "no PR yet" case is indistinguishable from "lookup
// failed" by design — chat-side decoration is the wrong place
// to surface a transient network / auth failure.
//
// Markdown link syntax (`[#N](url)`): Feishu's lark_md renders
// just `#N` as the link text (with platform-native link
// colour and click behaviour); the Telegram adapter's
// RenderMarkdown converts the same syntax to `<a href="...">`
// for the restricted-HTML subset.
func formatPRSegment(gs *messages.GitStatus) string {
	if gs == nil {
		return ""
	}
	pr := gs.PullRequest
	if pr == nil || pr.Number <= 0 || pr.URL == "" {
		return ""
	}
	return fmt.Sprintf("[#%d](%s)", pr.Number, pr.URL)
}

// abbrevTokens formats a token count into a compact human-readable
// string. Used by StatusBarLines to render the per-class token
// counters (new / cache / out) in Line 2.
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
// in one place per kind.
//
// F-55: used by StatusBarLines to render the `(window)`
// segment alongside `X%` so the user can read the denominator
// and judge upstream compatibility-layer mismatches themselves.
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
//     parent, period.
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
// that won't fit in narrow channel renderings.
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
// snapshot, with the PR / MR reference folded in as the LAST
// segment — and returns "" when there is nothing meaningful to
// show (no Workspace, no GitStatus, no git segment).
//
// Output (when non-empty, PR present):
//
//	📁: code/nightme · ⎇ main · + 2 · ± 3 · − 1 · ? 4 · ⇡ 5 · [#42](url)
//
// Output (when non-empty, no PR yet / lookup failed):
//
//	📁: code/nightme · ⎇ main · + 2 · ± 3 · − 1 · ? 4 · ⇡ 5
//
// Output (clean worktree, has upstream, no PR):
//
//	📁: code/nightme · ⎇ main
//
// Omit rules (each segment is dropped independently; the line
// itself is dropped when ALL segments would be empty):
//
//   - Workspace == ""           → entire line omitted (PR segment
//     drops with it: PR without a git workspace is a stale
//     cache state we don't surface on its own row)
//   - ⎇ <branch|?>              → always present when line is shown
//   - + N (added, staged A)     → omitted when Added == 0
//   - − N (deleted, X/Y = D)    → omitted when Deleted == 0
//   - ± N (modified, M/R/C) → omitted when Modified == 0
//   - ? N (untracked)           → omitted when Untracked == 0
//   - ! N (conflicts, UU / UA / UD / AU / AA / AD / DU / DA / DD) → omitted when Conflicts == 0
//   - ⇡ N (unpushed)            → omitted when !HasUpstream OR AheadOfRemote == 0
//   - [#N](url) (PR / MR)       → appended as last segment when
//     ctx.PullRequest resolves to a usable shape; see
//     formatPRSegment for the omit rules
//
// Symbol source: `+`, `−` (U+2212 MINUS SIGN), `±` (U+00B1
// PLUS-MINUS SIGN), `?`, `⇡` (U+21E1 UPWARDS WHITE ARROW), `!`
// match iTerm2's git-status-bar conventions; `−` and `±` are
// Unicode rather than ASCII so the rendered widths stay aligned
// with the single-character `+` and `?` segments in fixed-width
// rendering contexts.
//
// Segment order (fixed): workspace → branch → + → − → ± → ?
// → ! → ⇡ → PR. Added → deleted → modified → untracked →
// conflict → unpushed → PR.
//
// Returns "" when gs == nil or gs.Workspace == "" or
// gs.Snapshot == nil. Detached HEAD renders the branch segment
// as "?" — see parsePorcelainBranchStatus.
func formatGitLine(gs *messages.GitStatus) string {
	if gs == nil {
		return ""
	}
	if gs.Workspace == "" {
		return ""
	}
	if gs.Snapshot == nil {
		return ""
	}
	ws := formatWorkspacePath(gs.Workspace)
	if ws == "" {
		return ""
	}

	parts := []string{"📁: " + ws}

	// Branch segment (always present when line is shown).
	branch := "?"
	if gs.Snapshot.Branch != "" {
		branch = gs.Snapshot.Branch
	}
	parts = append(parts, "⎇ "+branch)

	// Working-tree segments, fixed order: added → deleted →
	// modified → untracked.
	dirty := gs.Snapshot
	if dirty.Added > 0 {
		parts = append(parts, fmt.Sprintf("+ %d", dirty.Added))
	}
	if dirty.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("− %d", dirty.Deleted))
	}
	if dirty.Modified > 0 {
		parts = append(parts, fmt.Sprintf("± %d", dirty.Modified))
	}
	if dirty.Untracked > 0 {
		parts = append(parts, fmt.Sprintf("? %d", dirty.Untracked))
	}

	// Conflict marker (`! N`) — emitted when the porcelain scan
	// found unmerged paths (UU / UA / UD / AU / AA / AD / DU / DA / DD).
	// Sits BEFORE the upstream segment so the line reads
	// "branch → counts → conflict → upstream → PR".
	if dirty.Conflicts > 0 {
		parts = append(parts, fmt.Sprintf("! %d", dirty.Conflicts))
	}

	// Upstream relationship: render `⇡ N` when the branch has
	// upstream AND has local unpushed commits. The `⇡ 0` form
	// is intentionally omitted. A clean repo with
	// HasUpstream=true renders as just "📁: <ws> · ⎇ <branch>"
	// (no ⇡), which is fine: the presence of the branch line
	// already tells the reader "we have an upstream branch";
	// the absent number truthfully says "nothing to push".
	if dirty.HasUpstream && dirty.AheadOfRemote > 0 {
		parts = append(parts, fmt.Sprintf("⇡ %d", dirty.AheadOfRemote))
	}

	// PR / MR tail — appended last so the line reads
	// "workspace → branch → dirty counts → upstream → PR".
	if pr := formatPRSegment(gs); pr != "" {
		parts = append(parts, pr)
	}

	// When the branch has no upstream AND the working tree is
	// clean AND no PR is cached, drop a "local" marker so the
	// footer doesn't silently end at "⎇ branch" — the user
	// should see at a glance that this is an untracked branch,
	// not a missing-data bug.
	if !dirty.HasUpstream &&
		gs.PullRequest == nil &&
		dirty.Added == 0 &&
		dirty.Deleted == 0 &&
		dirty.Modified == 0 &&
		dirty.Untracked == 0 &&
		dirty.Conflicts == 0 {
		parts = append(parts, "local")
	}

	return strings.Join(parts, " · ")
}