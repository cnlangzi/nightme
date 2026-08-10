// Package feishu — F-45 footer rendering tests.
//
// Covers the formatSessionFooter + abbrevTokens pure functions —
// all rules from docs/feat/F-45-session-footer.md §1.6 are tested
// here so future regressions on the public-facing footer line are
// caught at unit-test time, before a Feishu E2E round-trip.
package feishu

import (
	"reflect"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/gateway"
)

func TestFormatSessionFooterLines_NilContextReturnsNil(t *testing.T) {
	if got := formatSessionFooterLines(nil); got != nil {
		t.Fatalf("formatSessionFooterLines(nil) = %v, want nil", got)
	}
}

func TestFormatSessionFooterLines_AllZeroReturnsNil(t *testing.T) {
	ctx := &gateway.SessionContext{Agent: "", Model: ""}
	if got := formatSessionFooterLines(ctx); got != nil {
		t.Fatalf("empty SessionContext should yield nil, got %v", got)
	}
}

func TestFormatSessionFooterLines_IdentityOnly(t *testing.T) {
	// Agent + Model only, no tokens / cost → just line 1 (🤖 header).
	ctx := &gateway.SessionContext{Agent: "claude", Model: "opus-4-5"}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖: claude · opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSessionFooterLines() = %v, want %v", got, want)
	}
}

// F-56: appends the agent's own session id as the trailing
// identity segment, separated from Model by " · " to match the
// existing Model / usage separator convention. The materialize
// condition in sessionContextInto guarantees SessionID arrives
// alongside at least one of Agent / Model / GitStatus / Usage in
// practice, so the Agent-and-Model-and-SessionID path is the
// production-common case.
func TestFormatSessionFooterLines_IdentityWithSessionID(t *testing.T) {
	ctx := &gateway.SessionContext{
		Agent:     "claude",
		Model:     "opus-4-5",
		SessionID: "abc123-uuid-here",
	}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖: claude · opus-4-5 · abc123-uuid-here"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSessionFooterLines() = %v, want %v", got, want)
	}
}

// F-56: SessionID arrives before Agent has been set (early in the
// session lifecycle, or when the bridge synthesises a uuid like
// ACP does before knowing the agent name). The leading-separator
// rendering ("🤖: · <sid>") is acceptable: in production the
// materialize condition in sessionContextInto gates the entire
// SessionContext on at least one of Agent / Model / SessionID /
// GitStatus / Usage being non-empty, and once any other field
// arrives the leading separator disappears. Documenting the
// edge case here so future "fix the leading dot" PRs know it's
// intentional.
func TestFormatSessionFooterLines_SessionIDOnly(t *testing.T) {
	ctx := &gateway.SessionContext{SessionID: "abc123-uuid-here"}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖: · abc123-uuid-here"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSessionFooterLines() = %v, want %v", got, want)
	}
}

// F-56: Model is empty but Agent and SessionID are both set
// (e.g. bridge hasn't reported the model yet — EventAgentReady
// carries SessionID but Model can arrive later or be omitted
// entirely by the bridge). The middle-dot segment is dropped
// when Model is empty per the "each segment omitted
// independently" convention; Agent · SessionID chain renders
// with no separator gap. Locks the layout so a future
// "always-show-the-middle-dot" PR doesn't silently change the
// visual rhythm.
func TestFormatSessionFooterLines_AgentSessionIDOnly(t *testing.T) {
	ctx := &gateway.SessionContext{
		Agent:     "claude",
		SessionID: "abc123-uuid-here",
	}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖: claude · abc123-uuid-here"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSessionFooterLines() = %v, want %v", got, want)
	}
}

// F-56: Model + SessionID both set but Agent is empty (e.g.
// bridge emits SessionID before AgentName — possible during
// session/new handshake on claudecode / pi). The leading
// separator between `🤖:` and `Model` is the symmetric partner
// of TestFormatSessionFooterLines_SessionIDOnly's `🤖: · <sid>`
// — together they pin the layout when Agent is missing.
// Without this test a future "fix the leading dot" PR could
// silently change the Model+SessionID-no-Agent path because
// the other SessionID tests all have Agent set.
func TestFormatSessionFooterLines_ModelSessionIDOnly(t *testing.T) {
	ctx := &gateway.SessionContext{
		Model:     "opus-4-5",
		SessionID: "abc123-uuid-here",
	}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖: · opus-4-5 · abc123-uuid-here"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSessionFooterLines() = %v, want %v", got, want)
	}
}

// TestFormatSessionFooterLines_CompactionSegment removed: F-49
// compaction tracking was deleted across the runtime. The "· 🗜 N"
// segment is no longer rendered; the entire subtest is gone.

func TestFormatSessionFooterLines_TokenSegments(t *testing.T) {
	// F-52 / new footer convention: "in" folds all three input-side
	// counters (uncached + cache_creation + cache_read) per the
	// Tencent YB doc — see internal/channel/feishu/usage_footer.go
	// §Line 2 doc block. Here in = 11_700 + 600 + 8_200 = 20_500.
	ctx := &gateway.SessionContext{
		Agent: "claude",
		Model: "opus-4-5",
		Usage: &agent.UsageInfo{
			InputTokens:              11_700,
			OutputTokens:             1_500,
			CacheCreationInputTokens: 600, // counted into "in"
			CacheReadInputTokens:     8_200,
			CostUSD:                  0.087,
		},
	}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k · $0.087 」"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSessionFooterLines() mismatch:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestFormatSessionFooterLines_OmitsZeroSegments(t *testing.T) {
	tests := []struct {
		name string
		ctx  *gateway.SessionContext
		want []string
	}{
		{
			// No input-side tokens, only output. Renders as
			// "0 / 234" — zero-side honesty, rare in practice
			// (e.g. compaction-only turn with no new input).
			name: "no input but has output",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{OutputTokens: 234},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 234 」"},
		},
		{
			// F-55.1: only cache hits, no new tokens, no output.
			// Strict zero-omit — single segment renders, no
			// "0 /" prefix.
			name: "only cache hits — single segment renders",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{CacheReadInputTokens: 5_600},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 5.6k 」"},
		},
		{
			// Cost only — tokens are zero, so the entire
			// token segment is omitted; $cost segment stands
			// alone inside the brackets.
			name: "cost only (no tokens)",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{CostUSD: 1.245},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 $1.245 」"},
		},
		{
			// No cost segment when CostUSD == 0.
			name: "no cost (omitted)",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{
					InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200,
				},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k 」"},
		},
		{
			// No Agent/Model → only the usage line renders.
			name: "tokens but no Agent / Model",
			ctx: &gateway.SessionContext{
				Usage: &agent.UsageInfo{
					InputTokens: 5_000, OutputTokens: 200,
				},
			},
			want: []string{"💰:「 5k / 200 」"},
		},
		{
			// F-55.1: cache > 0 but no new and no out — only the
			// cache segment renders, no zero-padding. Rare in
			// practice (turn that hit cache only, no new content
			// and no generation).
			name: "only cache hits — single segment renders",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{CacheReadInputTokens: 5_600},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 5.6k 」"},
		},
		{
			// F-55.1: cache + out > 0, no new — layout shows
			// `cache / out`. Confirms cache doesn't lead the
			// segment when new is 0 (we drop the leading "0").
			name: "cache hits + output, no new tokens",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{
					CacheReadInputTokens: 5_600,
					OutputTokens:         800,
				},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 5.6k / 800 」"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatSessionFooterLines(tc.ctx)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatSessionFooterLines_LargeNumbers(t *testing.T) {
	// in = 156_000 + 0 + 1_200_000 = 1_356_000 → "1.4M" (rounded).
	ctx := &gateway.SessionContext{
		Agent: "claude", Model: "opus-4-5",
		Usage: &agent.UsageInfo{
			InputTokens:              156_000,
			OutputTokens:             18_000,
			CacheCreationInputTokens: 0,
			CacheReadInputTokens:     1_200_000,
			CostUSD:                  1.245,
		},
	}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖: claude · opus-4-5", "💰:「 156k / 1.2M / 18k · $1.245 」"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestFormatSessionFooter_StringForm covers the legacy single-
// string helper used by OutReply / OutResult orphan / overflow
// paths (text concatenation). The string form joins the
// lines with "\n" — plain-text rendering paths honour \n
// natively.
func TestFormatSessionFooter_StringForm(t *testing.T) {
	// in = 12_300 + 0 + 8_200 = 20_500 → "20.5k".
	ctx := &gateway.SessionContext{
		Agent: "claude", Model: "opus-4-5",
		Usage: &agent.UsageInfo{
			InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200,
		},
	}
	got := formatSessionFooter(ctx)
	want := "🤖: claude · opus-4-5\n💰:「 12.3k / 8.2k / 1.5k 」"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Empty input → empty string.
	if got := formatSessionFooter(&gateway.SessionContext{}); got != "" {
		t.Fatalf("empty ctx should yield empty string, got %q", got)
	}
	if got := formatSessionFooter(nil); got != "" {
		t.Fatalf("nil ctx should yield empty string, got %q", got)
	}
}

func TestFormatSessionFooter_StableAcrossReRenders(t *testing.T) {
	// Same input must always produce the same string — receipt
	// PATCH diffing relies on body equality.
	ctx := &gateway.SessionContext{
		Agent: "claude", Model: "opus-4-5",
		Usage: &agent.UsageInfo{
			InputTokens: 12_300, OutputTokens: 1_500,
			CacheReadInputTokens: 8_200, CostUSD: 0.087,
		},
	}
	first := formatSessionFooter(ctx)
	for i := 0; i < 5; i++ {
		if got := formatSessionFooter(ctx); got != first {
			t.Fatalf("non-deterministic footer at iteration %d:\n  first: %q\n  got:   %q", i, first, got)
		}
	}
}

// TestFormatSessionFooterLines_ContextWindowPct (F-52) covers the
// "X%" segment: the per-turn context-window usage percentage
// surfaced from UsageInfo.ContextWindowPct (bridge-computed via
// the Doc 1 formula). The footer just renders it.
//
// Omit rules:
//   - ContextWindowPct == 0 → segment dropped (bridge didn't
//     expose contextWindow this turn, pi protocol doesn't expose
//     it yet, or the model simply didn't report it).
//   - Otherwise renders as "X.Y%" with one decimal place (edge
//     cases like 99.6% matter for context-window tracking —
//     "%.0f%%" would round to misleading "100%").
func TestFormatSessionFooterLines_ContextWindowPct(t *testing.T) {
	tests := []struct {
		name string
		ctx  *gateway.SessionContext
		want []string
	}{
		{
			name: "pct=0 — segment omitted (early turn / no ContextWindow reported)",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{
					InputTokens: 12_300, OutputTokens: 1_500,
					CacheReadInputTokens: 8_200, CostUSD: 0.087,
				},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k · $0.087 」"},
		},
		{
			name: "pct only — typical post-EventAgentDone snapshot",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{
					InputTokens: 20_000, OutputTokens: 1_000,
					ContextWindow:    200_000,
					ContextWindowPct: 10.5, // 21k / 200k * 100
				},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 20k / 1k · 10.5% (200k) 」"},
		},
		{
			name: "pct + cost — full usage line",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{
					InputTokens: 1_200_000, OutputTokens: 80_000,
					CacheReadInputTokens: 800_000, CostUSD: 1.234,
					ContextWindow:    200_000,
					ContextWindowPct: 99.6, // near the ceiling
				},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 1.2M / 800k / 80k · 99.6% (200k) · $1.234 」"},
		},
		{
			name: "pct at the ceiling — 100.0% is honest, not 'full'",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{
					ContextWindow:    200_000,
					ContextWindowPct: 100.0,
				},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 100.0% (200k) 」"},
		},
		{
			name: "pct without identity — segment still emits alone",
			ctx: &gateway.SessionContext{
				Usage: &agent.UsageInfo{
					ContextWindow:    200_000,
					ContextWindowPct: 5.0,
				},
			},
			want: []string{"💰:「 5.0% (200k) 」"},
		},
		{
			// F-55: pct > 100% does NOT trigger clamp / warning —
			// we show the (window) so the user can judge upstream
			// compatibility-layer mismatches themselves (e.g.
			// `101.6% (200k)` against an actual 1M model).
			name: "pct > 100% — not clamped, (window) surfaces the mismatch",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{
					InputTokens: 200_000, OutputTokens: 1_000,
					CacheReadInputTokens: 3_000,
					ContextWindow:        200_000,
					ContextWindowPct:     101.6, // 203_103 / 200_000 * 100
				},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 200k / 3k / 1k · 101.6% (200k) 」"},
		},
		{
			// F-55: 1M-class model window rendered with M unit.
			name: "1M context window — M unit",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-8",
				Usage: &agent.UsageInfo{
					InputTokens: 200_000, OutputTokens: 1_000,
					ContextWindow:    1_000_000,
					ContextWindowPct: 20.1,
				},
			},
			want: []string{"🤖: claude · opus-4-8", "💰:「 200k / 1k · 20.1% (1M) 」"},
		},
		{
			// Defensive: pct==0 drops the entire segment; window
			// alone is not surfaced (zero-omit, F-45 §1.6).
			name: "pct=0 — segment omitted even when window > 0",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				Usage: &agent.UsageInfo{
					InputTokens: 12_300, OutputTokens: 1_500,
					CacheReadInputTokens: 8_200, CostUSD: 0.087,
					ContextWindow: 200_000,
				},
			},
			want: []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k · $0.087 」"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatSessionFooterLines(tc.ctx)
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
		{0, "0"},         // caller is expected to skip, but defensively still formats
		{1, "1"},         // raw integer
		{999, "999"},     // boundary
		{1_000, "1k"},    // boundary, integer multiple drops ".0"
		{12_345, "12.3k"},
		{999_999, "1000k"}, // edge of M threshold, integer multiple drops ".0"
		{1_000_000, "1M"},  // boundary, integer multiple drops ".0"
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
		{1_000, "1k"},        // integer multiple drops ".0"
		{200_000, "200k"},   // canonical MiniMax 200K case (the F-55 motivation)
		{999_999, "1000k"},  // integer multiple drops ".0"
		{1_000_000, "1M"},   // canonical 1M model window, integer multiple drops ".0"
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

func TestFormatGitLine_NilContextReturnsEmpty(t *testing.T) {
	if got := formatGitLine(nil); got != "" {
		t.Fatalf("nil ctx should yield empty, got %q", got)
	}
}

func TestFormatGitLine_NoWorkspaceReturnsEmpty(t *testing.T) {
	ctx := &gateway.SessionContext{
		GitStatus: &gtw.GitStatusSnapshot{Branch: "main"},
	}
	if got := formatGitLine(ctx); got != "" {
		t.Fatalf("Workspace=\"\" should yield empty, got %q", got)
	}
}

// TestFormatGitLine_NoGitStatusOmitsLine (F-48 review fix):
// when Workspace is set but GitStatus is nil (caller couldn't
// collect — non-repo / git error / git timeout), the entire
// footer line must be omitted. Rendering "📁: <ws> · ⎇ ?" would
// imply Git tracking is available when it isn't — the user's
// review caught this as a misleading UI bug. The "⎇ ?" rendering
// is reserved for detached HEAD inside a real git repo
// (Branch=="" + GitStatus!=nil).
func TestFormatGitLine_NoGitStatusOmitsLine(t *testing.T) {
	ctx := &gateway.SessionContext{Workspace: "/home/devin/code/nightme"}
	if got := formatGitLine(ctx); got != "" {
		t.Fatalf("Workspace set + GitStatus nil should omit line, got %q", got)
	}
}

func TestFormatGitLine_FullSnapshot(t *testing.T) {
	// Branch + dirty + untracked + unpushed — all segments.
	ctx := &gateway.SessionContext{
		Workspace: "/home/devin/code/nightme",
		GitStatus: &gtw.GitStatusSnapshot{
			Branch:        "main",
			Uncommitted:   3,
			Untracked:     2,
			AheadOfRemote: 5,
			HasUpstream:   true,
		},
	}
	got := formatGitLine(ctx)
	want := "📁: code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 5"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFormatGitLine_OmitsZeroSegments(t *testing.T) {
	tests := []struct {
		name string
		snap *gtw.GitStatusSnapshot
		want string
	}{
		{
			name: "clean working tree",
			snap: &gtw.GitStatusSnapshot{
				Branch: "main", HasUpstream: true, AheadOfRemote: 0,
			},
			want: "📁: code/nightme · ⎇ main",
		},
		{
			name: "uncommitted only",
			snap: &gtw.GitStatusSnapshot{
				Branch: "feat/x", Uncommitted: 1, HasUpstream: true,
			},
			want: "📁: code/nightme · ⎇ feat/x · ↑ 1",
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
			name: "no upstream — omit unpushed",
			snap: &gtw.GitStatusSnapshot{
				Branch:        "feat/new",
				Uncommitted:   2,
				AheadOfRemote: 0, // parser leaves this 0 when no upstream
				HasUpstream:   false,
			},
			want: "📁: code/nightme · ⎇ feat/new · ↑ 2",
		},
		{
			name: "detached HEAD — branch renders as ?",
			snap: &gtw.GitStatusSnapshot{
				Branch:        "", // empty -> renders "?"
				Uncommitted:   1,
				HasUpstream:   false,
				AheadOfRemote: 0,
			},
			want: "📁: code/nightme · ⎇ ? · ↑ 1",
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
			ctx := &gateway.SessionContext{
				Workspace: "/home/devin/code/nightme",
				GitStatus: tc.snap,
			}
			got := formatGitLine(ctx)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFormatSessionFooterLines_WithGitLine confirms line 3 is
// appended after lines 1+2 when both are populated.
func TestFormatSessionFooterLines_WithGitLine(t *testing.T) {
	// in = 12_300 + 0 + 8_200 = 20_500 → "20.5k".
	ctx := &gateway.SessionContext{
		Agent: "claude", Model: "opus-4-5",
		Usage: &agent.UsageInfo{
			InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200,
		},
		Workspace: "/home/devin/code/nightme",
		GitStatus: &gtw.GitStatusSnapshot{
			Branch:        "feat/x",
			Uncommitted:   2,
			Untracked:     1,
			AheadOfRemote: 3,
			HasUpstream:   true,
		},
	}
	got := formatSessionFooterLines(ctx)
	want := []string{
		"🤖: claude · opus-4-5",
		"💰:「 12.3k / 8.2k / 1.5k 」",
		"📁: code/nightme · ⎇ feat/x · ↑ 2 · ? 1 · ⇡ 3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestFormatSessionFooterLines_GitOnly confirms line 3 appears
// on its own when lines 1+2 are both empty (e.g. first reply on
// a git repo before any usage / model has been captured).
func TestFormatSessionFooterLines_GitOnly(t *testing.T) {
	ctx := &gateway.SessionContext{
		Workspace: "/home/devin/code/nightme",
		GitStatus: &gtw.GitStatusSnapshot{Branch: "main", HasUpstream: true},
	}
	got := formatSessionFooterLines(ctx)
	want := []string{"📁: code/nightme · ⎇ main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestFormatSessionFooterLines_NoGitNoUsage confirms we still
// return nil when nothing meaningful exists — backwards
// compatible with F-45.
func TestFormatSessionFooterLines_NoGitNoUsage(t *testing.T) {
	ctx := &gateway.SessionContext{Agent: "claude", Model: "opus-4-5"}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖: claude · opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
