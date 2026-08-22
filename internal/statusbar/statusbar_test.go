// Renderer tests for internal/statusbar. The renderer is a
// pure consumer of OutboundMessage flat fields (AgentName /
// Model / SessionID / Usage / GitStatus) — no I/O, no
// dependencies, no channel-specific hooks. Both the Feishu
// adapter (card footer) and the Telegram adapter (per-message
// trailer) reuse the same rendered output.
//
// This file covers the public-facing footer contract — every
// "what does the user see" rule from docs/feat/F-45-session-footer.md
// is locked in here. Runtime-side invariants (per-turn Usage,
// chatsession cache, etc.) are tested in internal/runtime.
package statusbar

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// utf8 is still needed for rune-count assertions on bar widths.

// emptyOut returns a minimal OutboundMessage with all identity
// fields zero — the renderer must treat it as "no footer".
func emptyOut() *messages.OutboundMessage {
	return &messages.OutboundMessage{}
}

// outWith builds an OutboundMessage with the given flat fields.
// Each test names only the fields it cares about; the rest stay
// at their zero value, exercising the renderer's zero-omit rules.
func outWith(agent, model, sess string, usage *agent.UsageInfo, gs *messages.GitStatus) *messages.OutboundMessage {
	return &messages.OutboundMessage{
		AgentName: agent,
		Model:     model,
		SessionID: sess,
		Usage:     usage,
		GitStatus: gs,
	}
}

func TestStatusBarLines_NilOutReturnsNil(t *testing.T) {
	if got := StatusBarLines(nil); got != nil {
		t.Fatalf("StatusBarLines(nil) = %v, want nil", got)
	}
}

func TestStatusBarLines_AllZeroReturnsNil(t *testing.T) {
	if got := StatusBarLines(emptyOut()); got != nil {
		t.Fatalf("empty OutboundMessage should yield nil, got %v", got)
	}
}

func TestStatusBarLines_IdentityOnly(t *testing.T) {
	got := StatusBarLines(outWith("claude", "opus-4-5", "", nil, nil))
	want := []string{"🤖: claude · opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusBarLines() = %v, want %v", got, want)
	}
}

func TestStatusBarLines_IdentityWithSessionID(t *testing.T) {
	// F-56: session id is the trailing identity segment.
	got := StatusBarLines(outWith("claude", "opus-4-5", "abc-uuid", nil, nil))
	want := []string{"🤖: claude · opus-4-5 · abc-uuid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusBarLines() = %v, want %v", got, want)
	}
}

func TestStatusBarLines_SessionIDOnly(t *testing.T) {
	// F-56: SessionID before Agent has been set. "🤖: · <sid>"
	// is acceptable; in production the materialize condition
	// gates the entire StatusBar on at least one of Agent /
	// Model / SessionID / GitStatus / Usage being non-empty.
	got := StatusBarLines(outWith("", "", "abc-uuid", nil, nil))
	want := []string{"🤖: · abc-uuid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusBarLines() = %v, want %v", got, want)
	}
}

func TestStatusBarLines_AgentSessionIDOnly(t *testing.T) {
	// F-56: Model empty, Agent + SessionID set. Middle-dot
	// segment dropped per "each segment omitted independently".
	got := StatusBarLines(outWith("claude", "", "abc-uuid", nil, nil))
	want := []string{"🤖: claude · abc-uuid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusBarLines() = %v, want %v", got, want)
	}
}

func TestStatusBarLines_ModelSessionIDOnly(t *testing.T) {
	// F-56: Model + SessionID but no Agent — leading separator
	// between `🤖:` and `Model`.
	got := StatusBarLines(outWith("", "opus-4-5", "abc-uuid", nil, nil))
	want := []string{"🤖: · opus-4-5 · abc-uuid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusBarLines() = %v, want %v", got, want)
	}
}

func TestStatusBarLines_TokenSegments(t *testing.T) {
	// F-55.1: "in" folds all three input-side counters per
	// Tencent YB doc convention. Here in = 11_700 + 600 + 8_200
	// = 20_500.
	usage := &agent.UsageInfo{
		InputTokens:              11_700,
		OutputTokens:             1_500,
		CacheCreationInputTokens: 600,
		CacheReadInputTokens:     8_200,
		CostUSD:                  0.087,
	}
	got := StatusBarLines(outWith("claude", "opus-4-5", "", usage, nil))
	want := []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k · $0.087 」"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusBarLines() mismatch:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestStatusBarLines_OmitsZeroSegments(t *testing.T) {
	tests := []struct {
		name string
		out  *messages.OutboundMessage
		want []string
	}{
		{
			// No input-side tokens, only output.
			name: "no input but has output",
			out: outWith("claude", "opus-4-5", "",
				&agent.UsageInfo{OutputTokens: 234}, nil),
			want: []string{"🤖: claude · opus-4-5", "💰:「 234 」"},
		},
		{
			// F-55.1: only cache hits, no new / out. Single
			// segment renders, no "0 /" prefix.
			name: "only cache hits",
			out: outWith("claude", "opus-4-5", "",
				&agent.UsageInfo{CacheReadInputTokens: 5_600}, nil),
			want: []string{"🤖: claude · opus-4-5", "💰:「 5.6k 」"},
		},
		{
			// Cost only — tokens zero, cost segment stands
			// alone inside the brackets.
			name: "cost only",
			out: outWith("claude", "opus-4-5", "",
				&agent.UsageInfo{CostUSD: 1.245}, nil),
			want: []string{"🤖: claude · opus-4-5", "💰:「 $1.245 」"},
		},
		{
			// No cost segment when CostUSD == 0.
			name: "no cost",
			out: outWith("claude", "opus-4-5", "",
				&agent.UsageInfo{
					InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200,
				}, nil),
			want: []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k 」"},
		},
		{
			// No Agent / Model → only the usage line renders.
			name: "tokens but no Agent / Model",
			out: outWith("", "", "",
				&agent.UsageInfo{InputTokens: 5_000, OutputTokens: 200}, nil),
			want: []string{"💰:「 5k / 200 」"},
		},
		{
			// F-55.1: cache > 0 but no new and no out — only
			// the cache segment renders, single segment.
			name: "only cache (no in, no out)",
			out: outWith("claude", "opus-4-5", "",
				&agent.UsageInfo{CacheReadInputTokens: 7_800}, nil),
			want: []string{"🤖: claude · opus-4-5", "💰:「 7.8k 」"},
		},
		{
			// X% (window) omitted when ContextWindowPct == 0
			// even if ContextWindow is set.
			name: "window set, pct zero",
			out: outWith("claude", "opus-4-5", "",
				&agent.UsageInfo{
					InputTokens: 100, ContextWindow: 200_000,
				}, nil),
			want: []string{"🤖: claude · opus-4-5", "💰:「 100 」"},
		},
		{
			// X% (window) rendered when both are set.
			name: "X% (window) rendered",
			out: outWith("claude", "opus-4-5", "",
				&agent.UsageInfo{
					InputTokens:        21_100,
					ContextWindow:      200_000,
					ContextWindowPct:   10.55,
				}, nil),
			want: []string{"🤖: claude · opus-4-5", "💰:「 21.1k · 10.6% (200k) 」"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StatusBarLines(tc.out)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StatusBarLines() mismatch:\n  got:  %v\n  want: %v", got, tc.want)
			}
		})
	}
}

func TestStatusBarLines_ContextWindowFmt(t *testing.T) {
	// X% rendered with 1 decimal place — 10.55% → 10.6%.
	// Pre-F-55.1 used "%.0f%%" which rounded 99.6 to "100%"
	// misleadingly; the 1-decimal rule was the F-55.1 fix.
	out := outWith("claude", "opus-4-5", "",
		&agent.UsageInfo{
			InputTokens:        20_500,
			OutputTokens:       1_000,
			ContextWindow:      200_000,
			ContextWindowPct:   10.55,
		}, nil)
	got := StatusBarLines(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[1], "10.6%") {
		t.Errorf("expected '10.6%%' in line 2, got %q", got[1])
	}
	if !strings.Contains(got[1], "(200k)") {
		t.Errorf("expected '(200k)' window suffix, got %q", got[1])
	}
}

func TestStatusBarLines_GitStatusLine(t *testing.T) {
	// Line 3: workspace + branch + counts + PR. F-45 §1.7.
	// A clean local-only branch (no upstream) shows the "· local"
	// marker so users see "this is untracked" vs "missing data".
	//
	// Workspace strings in GitStatus come from the git CLI which
	// always emits forward slashes; formatWorkspacePath feeds them
	// through filepath.Clean + filepath.Separator so the rendered
	// path uses the OS-native separator. We build the expected
	// string with the same transformation so the assertion is
	// stable across Linux/macOS (`/`) and Windows (`\`).
	gs := &messages.GitStatus{
		Workspace: "code/nightme",
		Snapshot:  &messages.GitStatusSnapshot{Branch: "main"},
	}
	got := StatusBarLines(outWith("claude", "opus-4-5", "", nil, gs))
	want := []string{
		"🤖: claude · opus-4-5",
		"📁: " + filepath.FromSlash("code/nightme") + " · ⎇ main · local",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusBarLines() = %v, want %v", got, want)
	}
}

func TestStatusBarLines_GitStatusLine_OmitAll(t *testing.T) {
	// gs == nil → no Line 3.
	got := StatusBarLines(outWith("claude", "opus-4-5", "", nil, nil))
	for _, line := range got {
		if strings.HasPrefix(line, "📁:") {
			t.Errorf("unexpected Line 3 with no GitStatus: %q", line)
		}
	}
}

func TestStatusBarLines_GitStatusLine_NoSnapshot(t *testing.T) {
	// gs.Workspace set but Snapshot nil → no Line 3 (non-git
	// workspace, matching the legacy "GitStatus == nil → entire
	// line omitted" contract).
	gs := &messages.GitStatus{Workspace: "code/nightme", Snapshot: nil}
	got := StatusBarLines(outWith("claude", "opus-4-5", "", nil, gs))
	for _, line := range got {
		if strings.HasPrefix(line, "📁:") {
			t.Errorf("unexpected Line 3 with no Snapshot: %q", line)
		}
	}
}

func TestStatusBarLines_FullFooter(t *testing.T) {
	// All three lines render: identity + usage + git.
	gs := &messages.GitStatus{
		Workspace: "code/nightme",
		Snapshot:  &messages.GitStatusSnapshot{Branch: "main", AheadOfRemote: 2},
		PullRequest: &messages.PR{Number: 42, URL: "https://x"},
	}
	out := outWith("claude", "opus-4-5", "sess-1",
		&agent.UsageInfo{InputTokens: 12_300, OutputTokens: 1_500, CostUSD: 0.087},
		gs)
	got := StatusBarLines(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(got), got)
	}
	if got[0] != "🤖: claude · opus-4-5 · sess-1" {
		t.Errorf("Line 1 = %q", got[0])
	}
	if !strings.HasPrefix(got[1], "💰:「 ") {
		t.Errorf("Line 2 = %q", got[1])
	}
	if !strings.HasPrefix(got[2], "📁: "+filepath.FromSlash("code/nightme")) {
		t.Errorf("Line 3 = %q", got[2])
	}
	if !strings.Contains(got[2], "[#42]") {
		t.Errorf("Line 3 should include PR link, got %q", got[2])
	}
}

func TestRenderPanel_EmptyLines(t *testing.T) {
	// No StatusBar → no panel. Caller should skip trailer
	// cheaply when StatusBarLines returns nil.
	if got := RenderPanel(nil); got != "" {
		t.Errorf("RenderPanel(nil) = %q, want empty", got)
	}
	if got := RenderPanel([]string{}); got != "" {
		t.Errorf("RenderPanel([]) = %q, want empty", got)
	}
}

func TestRenderPanel_Shape(t *testing.T) {
	// Locks the 4-corner design: ┌ ┐ on top, └ ┘ on bottom,
	// NO │ side borders (content sits flush left, free to wrap
	// on narrow screens).
	lines := []string{
		"🤖: claude · opus-4-5",
		"📁: cnlangzi/nightme · ⎇ main",
	}
	got := RenderPanel(lines)
	parts := strings.Split(got, "\n")
	if len(parts) != len(lines)+2 {
		t.Fatalf("expected %d parts (top + %d lines + bottom), got %d:\n%q",
			len(lines)+2, len(lines), len(parts), got)
	}
	// Top: ┌ ──── ›   (left square, right chevron tail —
	// "starts here, continues right")
	if !strings.HasPrefix(parts[0], "┌") {
		t.Errorf("top line must start with ┌; got %q", parts[0])
	}
	if !strings.HasSuffix(parts[0], "›") {
		t.Errorf("top line must end with › chevron tail; got %q", parts[0])
	}
	// Bottom: └ ──── ›  (same shape — left square, right chevron)
	if !strings.HasPrefix(parts[len(parts)-1], "└") {
		t.Errorf("bottom line must start with └; got %q", parts[len(parts)-1])
	}
	if !strings.HasSuffix(parts[len(parts)-1], "›") {
		t.Errorf("bottom line must end with › chevron tail; got %q", parts[len(parts)-1])
	}
	// Middle lines: 2-space left indent + verbatim content,
	// NO │ prefix/suffix.
	for i, line := range parts[1 : len(parts)-1] {
		if strings.HasPrefix(line, "│") || strings.HasSuffix(line, "│") {
			t.Errorf("middle line %d must not have │ side borders; got %q", i, line)
		}
		want := "  " + lines[i]
		if line != want {
			t.Errorf("middle line %d = %q, want %q (with 2-space indent)", i, line, want)
		}
	}
	// Top and bottom widths must match.
	if w1, w2 := utf8.RuneCountInString(parts[0]), utf8.RuneCountInString(parts[len(parts)-1]); w1 != w2 {
		t.Errorf("top width = %d, bottom width = %d (must match)", w1, w2)
	}
}

func TestRenderPanel_BarsAlwaysAtMaxWidth(t *testing.T) {
	// Bars anchor at PanelMaxWidth (30) regardless of content
	// length. This is the conservative contract: top/bottom
	// borders must NEVER wrap on mobile, content is free to.
	lines := []string{"x"} // very short content
	got := RenderPanel(lines)
	parts := strings.Split(got, "\n")
	if w := utf8.RuneCountInString(parts[0]); w != PanelMaxWidth {
		t.Errorf("top width = %d runes, want PanelMaxWidth=%d (short content)", w, PanelMaxWidth)
	}
	if w := utf8.RuneCountInString(parts[len(parts)-1]); w != PanelMaxWidth {
		t.Errorf("bottom width = %d runes, want PanelMaxWidth=%d", w, PanelMaxWidth)
	}
}

func TestRenderPanel_ContentNotTruncated(t *testing.T) {
	// Conservative contract: bars are at PanelMaxWidth (no wrap),
	// but content lines render VERBATIM and may extend past the
	// bars. Wrapping on mobile is acceptable — truncation is NOT.
	long := "🤖: claude · opus-4-5 · 61c4ec9d-dbb0-418c-bbe7-8d4bfbc1a135"
	got := RenderPanel([]string{long})
	parts := strings.Split(got, "\n")
	// Bar width is bounded.
	if w := utf8.RuneCountInString(parts[0]); w != PanelMaxWidth {
		t.Errorf("top width = %d runes, want %d", w, PanelMaxWidth)
	}
	// Content is preserved verbatim (with the 2-space indent).
	want := "  " + long
	if parts[1] != want {
		t.Errorf("content truncated/modified:\n  got:  %q\n  want: %q", parts[1], want)
	}
	// No ellipsis — content is full.
	if strings.Contains(parts[1], "…") {
		t.Errorf("content must not have `…` suffix; got %q", parts[1])
	}
}

func TestRenderPanel_LeftIndent(t *testing.T) {
	// Locks the 2-space left indent that gives the icon a
	// visual margin from the panel's left edge — design choice
	// per user feedback ("左边栏流出一点留白"). Content
	// renders verbatim AFTER the indent; long content extends
	// past the bars (acceptable).
	lines := []string{
		"🤖: claude · opus-4-5",
		"💰:「 12.3k / 200 · $0.087 」",
		"📁: cnlangzi/nightme · ⎇ main",
	}
	got := RenderPanel(lines)
	parts := strings.Split(got, "\n")
	if len(parts) != len(lines)+2 {
		t.Fatalf("expected %d parts, got %d", len(lines)+2, len(parts))
	}
	for i, line := range parts[1 : len(parts)-1] {
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("middle line %d must start with 2-space indent; got %q", i, line)
		}
		// Strip the indent and check the rest matches verbatim.
		stripped := strings.TrimPrefix(line, "  ")
		if stripped != lines[i] {
			t.Errorf("middle line %d content = %q, want verbatim %q", i, stripped, lines[i])
		}
	}
}

func TestStatusBarLines_EmptyUsage_NotPanic(t *testing.T) {
	// Defensive: Usage == nil (zero value) must not panic.
	// Renderer should skip Line 2 silently.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("StatusBarLines panicked on empty usage: %v", r)
		}
	}()
	got := StatusBarLines(outWith("claude", "opus-4-5", "", nil, nil))
	if len(got) != 1 {
		t.Errorf("expected 1 line (identity only), got %d: %v", len(got), got)
	}
}