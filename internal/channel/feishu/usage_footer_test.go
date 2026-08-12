// Package feishu — F-45 footer rendering tests.
//
// Covers the formatStatusBar + abbrevTokens pure functions —
// all rules from docs/feat/F-45-session-footer.md §1.6 are tested
// here so future regressions on the public-facing footer line are
// caught at unit-test time, before a Feishu E2E round-trip.
package feishu

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/messages"
)

func TestFormatStatusBarLines_NilContextReturnsNil(t *testing.T) {
	if got := formatStatusBarLines(nil); got != nil {
		t.Fatalf("formatStatusBarLines(nil) = %v, want nil", got)
	}
}

func TestFormatStatusBarLines_AllZeroReturnsNil(t *testing.T) {
	ctx := &messages.StatusBar{}
	if got := formatStatusBarLines(ctx); got != nil {
		t.Fatalf("empty StatusBar should yield nil, got %v", got)
	}
}

func TestFormatStatusBarLines_IdentityOnly(t *testing.T) {
	// Agent + Model only, no tokens / cost → just line 1 (🤖 header).
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
	}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: claude · opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

// F-56: appends the agent's own session id as the trailing
// identity segment, separated from Model by"" to match the
// existing Model / usage separator convention. The materialize
// condition in stampFromAS guarantees SessionID arrives
// alongside at least one of Agent / Model / GitStatus / Usage in
// practice, so the Agent-and-Model-and-SessionID path is the
// production-common case.
func TestFormatStatusBarLines_IdentityWithSessionID(t *testing.T) {
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5", SessionID: "abc123-uuid-here"},
	}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: claude · opus-4-5 · abc123-uuid-here"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

// F-56: SessionID arrives before Agent has been set (early in the
// session lifecycle, or when the bridge synthesises a uuid like
// ACP does before knowing the agent name). The leading-separator
// rendering ("🤖: · <sid>") is acceptable: in production the
// materialize condition in stampFromAS gates the entire
// StatusBar on at least one of Agent / Model / SessionID /
// GitStatus / Usage being non-empty, and once any other field
// arrives the leading separator disappears. Documenting the
// edge case here so future "fix the leading dot" PRs know it's
// intentional.
func TestFormatStatusBarLines_SessionIDOnly(t *testing.T) {
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{SessionID: "abc123-uuid-here"},
	}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: · abc123-uuid-here"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

// F-56: Model is empty but Agent and SessionID are both set
// (e.g. bridge hasn't reported the model yet — EventAgentReady
// carries SessionID but Model can arrive later or be omitted
// entirely by the bridge). The middle-dot segment is dropped
// when Model is empty per the"each segment omitted
// independently" convention; Agent · SessionID chain renders
// with no separator gap. Locks the layout so a future
// "always-show-the-middle-dot" PR doesn't silently change the
// visual rhythm.
func TestFormatStatusBarLines_AgentSessionIDOnly(t *testing.T) {
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", SessionID: "abc123-uuid-here"},
	}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: claude · abc123-uuid-here"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

// F-56: Model + SessionID both set but Agent is empty (e.g.
// bridge emits SessionID before AgentName — possible during
// session/new handshake on claudecode / pi). The leading
// separator between `🤖:` and `Model` is the symmetric partner
// of TestFormatStatusBarLines_SessionIDOnly's `🤖: · <sid>`
// — together they pin the layout when Agent is missing.
// Without this test a future "fix the leading dot" PR could
// silently change the Model+SessionID-no-Agent path because
// the other SessionID tests all have Agent set.
func TestFormatStatusBarLines_ModelSessionIDOnly(t *testing.T) {
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Model: "opus-4-5", SessionID: "abc123-uuid-here"},
	}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: · opus-4-5 · abc123-uuid-here"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

// TestFormatStatusBarLines_CompactionSegment removed: F-49
// compaction tracking was deleted across the runtime. The"· 🗜 N"
// segment is no longer rendered; the entire subtest is gone.

func TestFormatStatusBarLines_TokenSegments(t *testing.T) {
	// F-52 / new footer convention: "in" folds all three input-side
	// counters (uncached + cache_creation + cache_read) per the
	// Tencent YB doc — see internal/channel/feishu/usage_footer.go
	// §Line 2 doc block. Here in = 11_700 + 600 + 8_200 = 20_500.
	ctx := &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
		InputTokens:              11_700,
		OutputTokens:             1_500,
		CacheCreationInputTokens: 600, // counted into"in"
		CacheReadInputTokens:     8_200,
		CostUSD:                  0.087,
	}}}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k · $0.087 」"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() mismatch:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestFormatStatusBarLines_OmitsZeroSegments(t *testing.T) {
	tests := []struct {
		name string
		ctx  *messages.StatusBar
		want []string
	}{
		{
			// No input-side tokens, only output. Renders as
			//"0 / 234" — zero-side honesty, rare in practice
			// (e.g. compaction-only turn with no new input).
			name: "no input but has output",
			ctx: &messages.StatusBar{
				AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
				UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{OutputTokens: 234}},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 234 」"},
		},
		{
			// F-55.1: only cache hits, no new tokens, no output.
			// Strict zero-omit — single segment renders, no
			//"0 /" prefix.
			name: "only cache hits — single segment renders",
			ctx: &messages.StatusBar{
				AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
				UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{CacheReadInputTokens: 5_600}},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 5.6k 」"},
		},
		{
			// Cost only — tokens are zero, so the entire
			// token segment is omitted; $cost segment stands
			// alone inside the brackets.
			name: "cost only (no tokens)",
			ctx: &messages.StatusBar{
				AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
				UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{CostUSD: 1.245}},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 $1.245 」"},
		},
		{
			// No cost segment when CostUSD == 0.
			name: "no cost (omitted)",
			ctx: &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
				InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200}}},
			want: []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k 」"},
		},
		{
			// No Agent/Model → only the usage line renders.
			name: "tokens but no Agent / Model",
			ctx: &messages.StatusBar{UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
				InputTokens: 5_000, OutputTokens: 200}}},
			want: []string{"💰:「 5k / 200 」"},
		},
		{
			// F-55.1: cache > 0 but no new and no out — only the
			// cache segment renders, no zero-padding. Rare in
			// practice (turn that hit cache only, no new content
			// and no generation).
			name: "only cache hits — single segment renders",
			ctx: &messages.StatusBar{
				AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
				UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{CacheReadInputTokens: 5_600}},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 5.6k 」"},
		},
		{
			// F-55.1: cache + out > 0, no new — layout shows
			// `cache / out`. Confirms cache doesn't lead the
			// segment when new is 0 (we drop the leading"0").
			name: "cache hits + output, no new tokens",
			ctx: &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
				CacheReadInputTokens: 5_600,
				OutputTokens:         800}}},
			want: []string{"🤖: claude · opus-4-5", "💰:「 5.6k / 800 」"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatStatusBarLines(tc.ctx)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatStatusBarLines_LargeNumbers(t *testing.T) {
	// in = 156_000 + 0 + 1_200_000 = 1_356_000 →"1.4M" (rounded).
	ctx := &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
		InputTokens:              156_000,
		OutputTokens:             18_000,
		CacheCreationInputTokens: 0,
		CacheReadInputTokens:     1_200_000,
		CostUSD:                  1.245}}}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: claude · opus-4-5", "💰:「 156k / 1.2M / 18k · $1.245 」"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestFormatStatusBar_StringForm covers the legacy single-
// string helper used by OutReply / OutResult orphan / overflow
// paths (text concatenation). The string form joins the
// lines with"\n" — plain-text rendering paths honour \n
// natively.
func TestFormatStatusBar_StringForm(t *testing.T) {
	// in = 12_300 + 0 + 8_200 = 20_500 →"20.5k".
	ctx := &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
		InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200}}}
	got := formatStatusBar(ctx)
	want := "🤖: claude · opus-4-5\n💰:「 12.3k / 8.2k / 1.5k 」"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Empty input → empty string.
	if got := formatStatusBar(&messages.StatusBar{}); got != "" {
		t.Fatalf("empty ctx should yield empty string, got %q", got)
	}
	if got := formatStatusBar(nil); got != "" {
		t.Fatalf("nil ctx should yield empty string, got %q", got)
	}
}

func TestFormatStatusBar_StableAcrossReRenders(t *testing.T) {
	// Same input must always produce the same string — receipt
	// PATCH diffing relies on body equality.
	ctx := &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
		InputTokens: 12_300, OutputTokens: 1_500,
		CacheReadInputTokens: 8_200, CostUSD: 0.087}}}
	first := formatStatusBar(ctx)
	for i := 0; i < 5; i++ {
		if got := formatStatusBar(ctx); got != first {
			t.Fatalf("non-deterministic footer at iteration %d:\n  first: %q\n  got:   %q", i, first, got)
		}
	}
}

// TestFormatStatusBarLines_ContextWindowPct (F-52) covers the
// "X%" segment: the per-turn context-window usage percentage
// surfaced from UsageInfo.ContextWindowPct (bridge-computed via
// the Doc 1 formula). The footer just renders it.
//
// Omit rules:
//   - ContextWindowPct == 0 → segment dropped (bridge didn't
//     expose contextWindow this turn, pi protocol doesn't expose
//     it yet, or the model simply didn't report it).
//   - Otherwise renders as"X.Y%" with one decimal place (edge
//     cases like 99.6% matter for context-window tracking —
//
// "%.0f%%" would round to misleading"100%").
func TestFormatStatusBarLines_ContextWindowPct(t *testing.T) {
	tests := []struct {
		name string
		ctx  *messages.StatusBar
		want []string
	}{
		{
			name: "pct=0 — segment omitted (early turn / no ContextWindow reported)",
			ctx: &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
				InputTokens: 12_300, OutputTokens: 1_500,
				CacheReadInputTokens: 8_200, CostUSD: 0.087}}},
			want: []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k · $0.087 」"},
		},
		{
			name: "pct only — typical post-EventAgentDone snapshot",
			ctx: &messages.StatusBar{
				AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
				UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
					InputTokens:      20_000,
					OutputTokens:     1_000,
					ContextWindow:    200_000,
					ContextWindowPct: 10.5,
				}},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 20k / 1k · 10.5% (200k) 」"},
		},
		{
			name: "pct + cost — full usage line",
			ctx: &messages.StatusBar{
				AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
				UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
					InputTokens:          1_200_000,
					OutputTokens:         80_000,
					CacheReadInputTokens: 800_000,
					ContextWindow:        200_000,
					ContextWindowPct:     99.6,
					CostUSD:              1.234,
				}},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 1.2M / 800k / 80k · 99.6% (200k) · $1.234 」"},
		},
		{
			name: "pct at the ceiling — 100.0% is honest, not 'full'",
			ctx: &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
				ContextWindow:    200_000,
				ContextWindowPct: 100.0}}},
			want: []string{"🤖: claude · opus-4-5", "💰:「 100.0% (200k) 」"},
		},
		{
			name: "pct without identity — segment still emits alone",
			ctx: &messages.StatusBar{UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
				ContextWindow:    200_000,
				ContextWindowPct: 5.0}}},
			want: []string{"💰:「 5.0% (200k) 」"},
		},
		{
			// F-55: pct > 100% does NOT trigger clamp / warning —
			// we show the (window) so the user can judge upstream
			// compatibility-layer mismatches themselves (e.g.
			// `101.6% (200k)` against an actual 1M model).
			name: "pct > 100% — not clamped, (window) surfaces the mismatch",
			ctx: &messages.StatusBar{
				AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
				UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
					InputTokens:          200_000,
					OutputTokens:         1_000,
					CacheReadInputTokens: 3_000,
					ContextWindow:        200_000,
					ContextWindowPct:     101.6,
				}},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 200k / 3k / 1k · 101.6% (200k) 」"},
		},
		{
			// F-55: 1M-class model window rendered with M unit.
			name: "1M context window — M unit",
			ctx: &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-8"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
				InputTokens: 200_000, OutputTokens: 1_000,
				ContextWindow:    1_000_000,
				ContextWindowPct: 20.1}}},
			want: []string{"🤖: claude · opus-4-8", "💰:「 200k / 1k · 20.1% (1M) 」"},
		},
		{
			// Defensive: pct==0 drops the entire segment; window
			// alone is not surfaced (zero-omit, F-45 §1.6).
			name: "pct=0 — segment omitted even when window > 0",
			ctx: &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
				InputTokens: 12_300, OutputTokens: 1_500,
				CacheReadInputTokens: 8_200, CostUSD: 0.087,
				ContextWindow: 200_000}}},
			want: []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k · $0.087 」"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatStatusBarLines(tc.ctx)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAbbrevTokens(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},      // caller is expected to skip, but defensively still formats
		{1, "1"},      // raw integer
		{999, "999"},  // boundary
		{1_000, "1k"}, // boundary, integer multiple drops".0"
		{12_345, "12.3k"},
		{999_999, "1000k"}, // edge of M threshold, integer multiple drops".0"
		{1_000_000, "1M"},  // boundary, integer multiple drops".0"
		{1_234_567, "1.2M"},
	}
	for _, tc := range tests {
		got := abbrevTokens(tc.in)
		if got != tc.want {
			t.Errorf("abbrevTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAbbrevWindow pins F-55's separate helper. abbrevWindow is
// deliberately a duplicate of abbrevTokens (separate symbol so
// the formatting policy lives in one place per kind; see
// usage_footer.go abbrevWindow doc). Without this test, a
// refactor that changes one helper but forgets the other would
// silently diverge — abbrevWindow renders the model
// context-window denominator in `X% (window)`, so a wrong
// abbreviation would mislead the user about the actual window
// size (the very thing F-55 tries to surface honestly).
func TestAbbrevWindow(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"}, // defensive (caller omits the segment when pct==0)
		{1, "1"},
		{999, "999"},
		{1_000, "1k"},      // integer multiple drops".0"
		{200_000, "200k"},  // canonical MiniMax 200K case (the F-55 motivation)
		{999_999, "1000k"}, // integer multiple drops".0"
		{1_000_000, "1M"},  // canonical 1M model window, integer multiple drops".0"
		{1_234_567, "1.2M"},
	}
	for _, tc := range tests {
		got := abbrevWindow(tc.in)
		if got != tc.want {
			t.Errorf("abbrevWindow(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- F-48 footer line 3: git tracking -----------------------------

// TestFormatWorkspacePath exercises the simplified F-48 rule:
// NO prefix, ≤2 components kept whole, >2 truncated to the
// last 2. HOME / non-HOME / macOS / Windows — all the same.
func TestFormatWorkspacePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty -> empty", "", ""},
		{"dot -> empty", ".", ""},
		{"root -> empty", "/", ""},
		{"single dir (1 component)", "/home", "home"},
		{"two dirs (≤2 keep all)", "/home/devin", "home/devin"},
		{"three dirs (>2 last 2)", "/home/devin/code", "devin/code"},
		{"four dirs (>2 last 2)", "/home/devin/code/nightme", "code/nightme"},
		{"five dirs (>2 last 2)", "/home/devin/code/nightme/internal", "nightme/internal"},
		{"deeply nested (>2 last 2)", "/home/devin/a/b/c/d/e", "d/e"},
		{"trailing slash normalised", "/home/devin/", "home/devin"},
		{"non-HOME absolute (1)", "/tmp", "tmp"},
		{"non-HOME absolute (2)", "/tmp/foo", "tmp/foo"},
		{"non-HOME absolute (3)", "/tmp/foo/bar", "foo/bar"},
		{"non-HOME absolute deep", "/tmp/a/b/c", "b/c"},
		{"macOS-style (3)", "/Users/dev/code", "dev/code"},
		{"macOS-style (4)", "/Users/dev/code/nightme", "code/nightme"},
		{"prefix-collision: /home/devin vs /home/devin-other", "/home/devin-other/foo", "devin-other/foo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatWorkspacePath(tc.in)
			if got != tc.want {
				t.Fatalf("formatWorkspacePath(%q) = %q, want %q",
					tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatGitBar_NilContextReturnsEmpty(t *testing.T) {
	if got := formatGitBar(nil); got != "" {
		t.Fatalf("nil ctx should yield empty, got %q", got)
	}
}

// TestFormatGitBar_NoWorkspaceReturnsEmpty (F-48 contract):
// when Workspace=="" the GitBar branch must drop the entire
// line — a non-empty GitStatus without a Workspace implies a
// bug in the runtime stamp path. Pass a populated GitStatus
// specifically to prove the Workspace-empty gate (not the
// nil-GitStatus gate) is what's catching this case.
func TestFormatGitBar_NoWorkspaceReturnsEmpty(t *testing.T) {
	gb := &messages.GitStatusBar{
		Workspace: "",
		GitStatus: &gtw.GitStatusSnapshot{Branch: "main", HasUpstream: true},
	}
	if got := formatGitBar(gb); got != "" {
		t.Fatalf("Workspace=\"\" should yield empty, got %q", got)
	}
}

// TestFormatGitBar_NoGitStatusOmitsLine (F-48 review fix):
// when Workspace is set but GitStatus is nil (caller couldn't
// collect — non-repo / git error / git timeout), the entire
// footer line must be omitted. Rendering"📁: <ws> · ⎇ ?" would
// imply Git tracking is available when it isn't — the user's
// review caught this as a misleading UI bug. The"⎇ ?" rendering
// is reserved for detached HEAD inside a real git repo
// (Branch=="" + GitStatus!=nil). Pass a populated Workspace
// specifically to prove the nil-GitStatus gate is what's
// catching this case.
func TestFormatGitBar_NoGitStatusOmitsLine(t *testing.T) {
	gb := &messages.GitStatusBar{
		Workspace: "/some/path",
		GitStatus: nil,
	}
	if got := formatGitBar(gb); got != "" {
		t.Fatalf("Workspace set + GitStatus nil should omit line, got %q", got)
	}
}

func TestFormatGitBar_FullSnapshot(t *testing.T) {
	// Branch + dirty + untracked + unpushed — all segments.
	ctx := &messages.StatusBar{GitBar: &messages.GitStatusBar{Workspace: "/home/devin/code/nightme", GitStatus: &gtw.GitStatusSnapshot{
		Branch:        "main",
		Modified:      3,
		Untracked:     2,
		AheadOfRemote: 5,
		HasUpstream:   true,
	}}}
	got := formatGitBar(ctx.GitBar)
	want := "📁: code/nightme · ⎇ main · ± 3 · ? 2 · ⇡ 5"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFormatGitBar_OmitsZeroSegments(t *testing.T) {
	tests := []struct {
		name string
		snap *gtw.GitStatusSnapshot
		want string
	}{
		{
			name: "clean working tree, has upstream",
			snap: &gtw.GitStatusSnapshot{
				Branch: "main", HasUpstream: true, AheadOfRemote: 0,
			},
			want: "📁: code/nightme · ⎇ main",
		},
		{
			name: "uncommitted only",
			snap: &gtw.GitStatusSnapshot{
				Branch: "feat/x", Modified: 1, HasUpstream: true,
			},
			want: "📁: code/nightme · ⎇ feat/x · ± 1",
		},
		{
			name: "untracked only",
			snap: &gtw.GitStatusSnapshot{
				Branch: "feat/x", Untracked: 7, HasUpstream: true,
			},
			want: "📁: code/nightme · ⎇ feat/x · ? 7",
		},
		{
			name: "unpushed only",
			snap: &gtw.GitStatusSnapshot{
				Branch: "feat/x", AheadOfRemote: 4, HasUpstream: true,
			},
			want: "📁: code/nightme · ⎇ feat/x · ⇡ 4",
		},
		{
			name: "no upstream + uncommitted — show dirty, no 'local' marker",
			snap: &gtw.GitStatusSnapshot{
				Branch:        "feat/new",
				Modified:      2,
				AheadOfRemote: 0, // parser leaves this 0 when no upstream
				HasUpstream:   false,
			},
			want: "📁: code/nightme · ⎇ feat/new · ± 2",
		},
		{
			name: "clean + no upstream — adds 'local' marker",
			snap: &gtw.GitStatusSnapshot{
				Branch:      "feat/new",
				HasUpstream: false,
			},
			want: "📁: code/nightme · ⎇ feat/new · local",
		},
		{
			name: "detached HEAD — branch renders as ?",
			snap: &gtw.GitStatusSnapshot{
				Branch:        "", // empty -> renders"?"
				Modified:      1,
				HasUpstream:   false,
				AheadOfRemote: 0,
			},
			want: "📁: code/nightme · ⎇ ? · ± 1",
		},
		{
			name: "long path — last 2 components",
			snap: &gtw.GitStatusSnapshot{Branch: "main", HasUpstream: true},
			want: "📁: code/nightme · ⎇ main",
			// (default Workspace is /home/devin/code/nightme → code/nightme)
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Pre-rename this test passed a snapshot directly to
			// formatGitBar(*StatusBar). Post-rename
			// formatGitBar takes a *GitStatusBar — wrap the
			// snapshot in one so the test stays focused on the
			// snapshot-shape rules.
			gb := &messages.GitStatusBar{
				Workspace: "/home/devin/code/nightme",
				GitStatus: tc.snap,
			}
			got := formatGitBar(gb)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFormatGitBar_AllCategoriesInFixedOrder pins the iTerm2-
// aligned segment order: workspace -> branch -> + N -> - N ->
// +- N -> ? N -> #N. Each category is populated with a
// non-zero count so the test fails if any segment is dropped
// or reordered.
func TestFormatGitBar_AllCategoriesInFixedOrder(t *testing.T) {
	gb := &messages.GitStatusBar{
		Workspace: "/home/devin/code/nightme",
		GitStatus: &gtw.GitStatusSnapshot{
			Branch:        "feat/order",
			Added:         2,
			Deleted:       1,
			Modified:      3,
			Untracked:     4,
			AheadOfRemote: 5,
			HasUpstream:   true,
		},
		PullRequest: &gtw.PR{
			Number: 99,
			URL:    "https://example/pr/99",
			State:  "open",
		},
	}
	got := formatGitBar(gb)
	want := "📁: code/nightme · ⎇ feat/order · + 2 · − 1 · ± 3 · ? 4 · ⇡ 5 · [#99](https://example/pr/99)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestFormatGitBar_ConflictMarkerRendersAfterUntracked
// confirms the "! conflict" segment is appended AFTER the
// working-tree counts (+ - +- ?) but BEFORE the upstream (arrow)
// and PR ([#N](url)) segments.
func TestFormatGitBar_ConflictMarkerRendersAfterUntracked(t *testing.T) {
	gb := &messages.GitStatusBar{
		Workspace: "/home/devin/code/nightme",
		GitStatus: &gtw.GitStatusSnapshot{
			Branch:       "feat/conflict",
			Added:        1,
			Modified:     1,
			Untracked:    1,
			Conflicts:    1,
			HasUpstream:  true,
			HasConflicts: true,
		},
	}
	got := formatGitBar(gb)
	want := "📁: code/nightme · ⎇ feat/conflict · + 1 · ± 1 · ? 1 · ! 1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestFormatGitBar_ConflictsSeparateFromModified pins the
// status-bar split invariant: a conflict entry counts as
// Conflicts, NOT Modified. 2 M + 1 UU must render as
// "± 2 · ! 1" (non-overlapping counts), not "± 3 · ! 1"
// (which would double-count the conflict inside Modified).
func TestFormatGitBar_ConflictsSeparateFromModified(t *testing.T) {
	gb := &messages.GitStatusBar{
		Workspace: "/home/devin/code/nightme",
		GitStatus: &gtw.GitStatusSnapshot{
			Branch:       "feat/x",
			Modified:     2,
			Conflicts:    1,
			HasConflicts: true,
		},
	}
	got := formatGitBar(gb)
	want := "📁: code/nightme · ⎇ feat/x · ± 2 · ! 1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestFormatGitBar_LocalSuppressedByPullRequest guards against
// the race window where PullRequest is cached but HasUpstream
// is false (e.g. upstream detached after PR was opened).
// Without this gate the line would read contradictory:
// "... · [#42](url) · local". PullRequest != nil suppresses
// the local marker.
func TestFormatGitBar_LocalSuppressedByPullRequest(t *testing.T) {
	gb := &messages.GitStatusBar{
		Workspace: "/home/devin/code/nightme",
		GitStatus: &gtw.GitStatusSnapshot{
			Branch:      "feat/x",
			HasUpstream: false,
		},
		PullRequest: &gtw.PR{
			Number: 42,
			URL:    "https://example/pr/42",
			State:  "open",
		},
	}
	got := formatGitBar(gb)
	want := "📁: code/nightme · ⎇ feat/x · [#42](https://example/pr/42)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestFormatStatusBarLines_WithGitLine confirms line 3 is
// appended after lines 1+2 when both are populated.
func TestFormatStatusBarLines_WithGitLine(t *testing.T) {
	// in = 12_300 + 0 + 8_200 = 20_500 →"20.5k".
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
		GitBar: &messages.GitStatusBar{
			Workspace: "/home/devin/code/nightme",
			GitStatus: &gtw.GitStatusSnapshot{
				Branch:        "feat/x",
				Modified:      2,
				Untracked:     1,
				AheadOfRemote: 3,
				HasUpstream:   true,
			},
		},
		UsageBar: &messages.UsageStatusBar{UsageInfo: &agent.UsageInfo{
			InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200,
		}},
	}
	got := formatStatusBarLines(ctx)
	want := []string{
		"🤖: claude · opus-4-5",
		"💰:「 12.3k / 8.2k / 1.5k 」",
		"📁: code/nightme · ⎇ feat/x · ± 2 · ? 1 · ⇡ 3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestFormatStatusBarLines_GitOnly confirms line 3 appears
// on its own when lines 1+2 are both empty (e.g. first reply on
// a git repo before any usage / model has been captured).
func TestFormatStatusBarLines_GitOnly(t *testing.T) {
	ctx := &messages.StatusBar{
		GitBar: &messages.GitStatusBar{
			Workspace: "/home/devin/code/nightme",
			GitStatus: &gtw.GitStatusSnapshot{Branch: "main", HasUpstream: true, AheadOfRemote: 0},
		},
	}
	got := formatStatusBarLines(ctx)
	want := []string{"📁: code/nightme · ⎇ main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestFormatStatusBarLines_NoGitNoUsage confirms we still
// return nil when nothing meaningful exists — backwards
// compatible with F-45.
func TestFormatStatusBarLines_NoGitNoUsage(t *testing.T) {
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
	}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: claude · opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestFormatStatusBarLines_PRSegment_AppendedToGitLine
// pins the layout: when a PR has been resolved and a git line
// is rendered, the PR reference folds into the git line as its
// LAST segment, using markdown link syntax `[#N](url)` (no 🔗:
// prefix). Empirically verified on current Feishu (2026-08,
// see pr_render_compare_test.go): lark_md renders `#N` as a
// clickable blue anchor while the rest of the workspace row
// stays in the <font color='grey'> wrap.
//
// PR without git (no Workspace / no GitStatus) drops with
// the git line — see TestFormatStatusBarLines_PRSegment_NoGitLine.
func TestFormatStatusBarLines_PRSegment_AppendedToGitLine(t *testing.T) {
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
		GitBar: &messages.GitStatusBar{
			Workspace:   "/home/devin/code/nightme",
			GitStatus:   &gtw.GitStatusSnapshot{Branch: "fix-x", HasUpstream: true},
			PullRequest: &gtw.PR{Number: 111, URL: "https://github.com/cnlangzi/nightme/pull/111", State: "open"},
		},
	}
	got := formatStatusBarLines(ctx)
	want := []string{
		"🤖: claude · opus-4-5",
		"📁: code/nightme · ⎇ fix-x · [#111](https://github.com/cnlangzi/nightme/pull/111)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

// TestFormatStatusBarLines_PRSegment_NoGitLine confirms
// that the PR number is NOT promoted to its own line when the
// git line drops (no Workspace / no GitStatus / detached-HEAD-
// outside-repo edge cases). A stale PR cache must not surface
// on a row of its own — the footer stays at the canonical
// identity / usage stack. Test reproduces the case by giving
// a PR but no Workspace, which is the standard
// "transient / not in a git repo" configuration.
func TestFormatStatusBarLines_PRSegment_NoGitLine(t *testing.T) {
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
		GitBar: &messages.GitStatusBar{
			PullRequest: &gtw.PR{Number: 111, URL: "https://github.com/cnlangzi/nightme/pull/111", State: "open"},
		},
	}
	got := formatStatusBarLines(ctx)
	want := []string{"🤖: claude · opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	// Stale PR cache must not surface standalone on the
	// identity row. The PR number renders as plain `#N` —
	// check the literal, not an emoji substring.
	if strings.Contains(got[0], "#111") {
		t.Errorf("footer identity line %q absorbed PR #111 with no git line — must not surface standalone", got[0])
	}
}

// TestFormatStatusBarLines_PRSegment_NilOrInvalid covers
// the omit rules for the PR tail: nil PR / Number <= 0 / empty
// URL all leave the git line without the trailing `[#N](url)`
// segment."No PR yet" must look identical to"lookup failed"
// — chat-side decoration is the wrong place to discriminate them.
func TestFormatStatusBarLines_PRSegment_NilOrInvalid(t *testing.T) {
	tests := []struct {
		name string
		pr   *gtw.PR
	}{
		{"nil PR", nil},
		{"zero number", &gtw.PR{Number: 0, URL: "https://x/y"}},
		{"empty URL", &gtw.PR{Number: 5, URL: ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &messages.StatusBar{AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"}, GitBar: &messages.GitStatusBar{Workspace: "/home/devin/code/nightme", GitStatus: &gtw.GitStatusSnapshot{Branch: "fix-x", HasUpstream: true}}}
			got := formatStatusBarLines(ctx)
			want := []string{
				"🤖: claude · opus-4-5",
				"📁: code/nightme · ⎇ fix-x",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v\nwant %v", got, want)
			}
		})
	}
}

// TestFormatStatusBarLines_PRSegment_DirtyCountsBetween
// pins the segment order: workspace → branch → dirty counts
// → PR. The PR must be the last segment so the line reads
// as"where am I → how dirty → what's the open PR", not
// "where am I → what's the PR → how dirty".
func TestFormatStatusBarLines_PRSegment_DirtyCountsBetween(t *testing.T) {
	ctx := &messages.StatusBar{
		AgentBar: &messages.AgentStatusBar{Agent: "claude", Model: "opus-4-5"},
		GitBar: &messages.GitStatusBar{
			Workspace: "/home/devin/code/nightme",
			GitStatus: &gtw.GitStatusSnapshot{
				Branch:        "fix-x",
				Modified:      3,
				Untracked:     2,
				HasUpstream:   true,
				AheadOfRemote: 1,
			},
			PullRequest: &gtw.PR{Number: 42, URL: "https://example/pr/42", State: "open"},
		},
	}
	got := formatStatusBarLines(ctx)
	want := []string{
		"🤖: claude · opus-4-5",
		"📁: code/nightme · ⎇ fix-x · ± 3 · ? 2 · ⇡ 1 · [#42](https://example/pr/42)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
