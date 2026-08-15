// F-CLAUDE-PRINT-002: formatStatusBarLines now takes
// *OutboundMessage directly (StatusBar wrapper is gone). The
// renderer reads flat fields (AgentName / Model / SessionID /
// Usage / GitStatus) from the message.
//
// This file covers the public-facing footer contract — every
// "what does the user see" rule from docs/feat/F-45-session-footer.md
// is locked in here. Runtime-side invariants (per-turn Usage,
// chatsession cache, etc.) are tested in internal/runtime.
package feishu

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

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

func TestFormatStatusBarLines_NilOutReturnsNil(t *testing.T) {
	if got := formatStatusBarLines(nil); got != nil {
		t.Fatalf("formatStatusBarLines(nil) = %v, want nil", got)
	}
}

func TestFormatStatusBarLines_AllZeroReturnsNil(t *testing.T) {
	if got := formatStatusBarLines(emptyOut()); got != nil {
		t.Fatalf("empty OutboundMessage should yield nil, got %v", got)
	}
}

func TestFormatStatusBarLines_IdentityOnly(t *testing.T) {
	got := formatStatusBarLines(outWith("claude", "opus-4-5", "", nil, nil))
	want := []string{"🤖: claude · opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

func TestFormatStatusBarLines_IdentityWithSessionID(t *testing.T) {
	// F-56: session id is the trailing identity segment.
	got := formatStatusBarLines(outWith("claude", "opus-4-5", "abc-uuid", nil, nil))
	want := []string{"🤖: claude · opus-4-5 · abc-uuid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

func TestFormatStatusBarLines_SessionIDOnly(t *testing.T) {
	// F-56: SessionID before Agent has been set. "🤖: · <sid>"
	// is acceptable; in production the materialize condition
	// gates the entire StatusBar on at least one of Agent /
	// Model / SessionID / GitStatus / Usage being non-empty.
	got := formatStatusBarLines(outWith("", "", "abc-uuid", nil, nil))
	want := []string{"🤖: · abc-uuid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

func TestFormatStatusBarLines_AgentSessionIDOnly(t *testing.T) {
	// F-56: Model empty, Agent + SessionID set. Middle-dot
	// segment dropped per"each segment omitted independently".
	got := formatStatusBarLines(outWith("claude", "", "abc-uuid", nil, nil))
	want := []string{"🤖: claude · abc-uuid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

func TestFormatStatusBarLines_ModelSessionIDOnly(t *testing.T) {
	// F-56: Model + SessionID but no Agent — leading separator
	// between `🤖:` and `Model`. Symmetric partner to
	// TestFormatStatusBarLines_SessionIDOnly.
	got := formatStatusBarLines(outWith("", "opus-4-5", "abc-uuid", nil, nil))
	want := []string{"🤖: · opus-4-5 · abc-uuid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

func TestFormatStatusBarLines_TokenSegments(t *testing.T) {
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
	got := formatStatusBarLines(outWith("claude", "opus-4-5", "", usage, nil))
	want := []string{"🤖: claude · opus-4-5", "💰:「 12.3k / 8.2k / 1.5k · $0.087 」"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() mismatch:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestFormatStatusBarLines_OmitsZeroSegments(t *testing.T) {
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
			// segment renders, no"0 /" prefix.
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
					InputTokens: 21_100,
					ContextWindow: 200_000,
					ContextWindowPct: 10.55,
				}, nil),
			want: []string{"🤖: claude · opus-4-5", "💰:「 21.1k · 10.6% (200k) 」"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatStatusBarLines(tc.out)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("formatStatusBarLines() mismatch:\n  got:  %v\n  want: %v", got, tc.want)
			}
		})
	}
}

func TestFormatStatusBarLines_ContextWindowFmt(t *testing.T) {
	// X% rendered with 1 decimal place — 10.55% → 10.6%.
	// Pre-F-55.1 used "%.0f%%" which rounded 99.6 to "100%"
	// misleadingly; the 1-decimal rule was the F-55.1 fix.
	out := outWith("claude", "opus-4-5", "",
		&agent.UsageInfo{
			InputTokens: 20_500, OutputTokens: 1_000,
			ContextWindow: 200_000, ContextWindowPct: 10.55,
		}, nil)
	got := formatStatusBarLines(out)
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

func TestFormatStatusBarLines_GitStatusLine(t *testing.T) {
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
	got := formatStatusBarLines(outWith("claude", "opus-4-5", "", nil, gs))
	want := []string{
		"🤖: claude · opus-4-5",
		"📁: " + filepath.FromSlash("code/nightme") + " · ⎇ main · local",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatStatusBarLines() = %v, want %v", got, want)
	}
}

func TestFormatStatusBarLines_GitStatusLine_OmitAll(t *testing.T) {
	// gs == nil → no Line 3.
	got := formatStatusBarLines(outWith("claude", "opus-4-5", "", nil, nil))
	for _, line := range got {
		if strings.HasPrefix(line, "📁:") {
			t.Errorf("unexpected Line 3 with no GitStatus: %q", line)
		}
	}
}

func TestFormatStatusBarLines_GitStatusLine_NoSnapshot(t *testing.T) {
	// gs.Workspace set but Snapshot nil → no Line 3 (non-git
	// workspace, matching the legacy "GitStatus == nil → entire
	// line omitted" contract).
	gs := &messages.GitStatus{Workspace: "code/nightme", Snapshot: nil}
	got := formatStatusBarLines(outWith("claude", "opus-4-5", "", nil, gs))
	for _, line := range got {
		if strings.HasPrefix(line, "📁:") {
			t.Errorf("unexpected Line 3 with no Snapshot: %q", line)
		}
	}
}

func TestFormatStatusBarLines_FullFooter(t *testing.T) {
	// All three lines render: identity + usage + git.
	gs := &messages.GitStatus{
		Workspace: "code/nightme",
		Snapshot:  &messages.GitStatusSnapshot{Branch: "main", AheadOfRemote: 2},
		PullRequest: &messages.PR{Number: 42, URL: "https://x"},
	}
	out := outWith("claude", "opus-4-5", "sess-1",
		&agent.UsageInfo{InputTokens: 12_300, OutputTokens: 1_500, CostUSD: 0.087},
		gs)
	got := formatStatusBarLines(out)
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

func TestFormatStatusBarLines_EmptyUsage_NotPanic(t *testing.T) {
	// Defensive: Usage == nil (zero value) must not panic.
	// Renderer should skip Line 2 silently.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("formatStatusBarLines panicked on empty usage: %v", r)
		}
	}()
	got := formatStatusBarLines(outWith("claude", "opus-4-5", "", nil, nil))
	if len(got) != 1 {
		t.Errorf("expected 1 line (identity only), got %d: %v", len(got), got)
	}
}
