// Package feishu — F-45 footer rendering tests.
//
// Covers the formatSessionFooter + abbrevTokens pure functions —
// all rules from docs/feat/F-45-session-footer.md §1.6 are tested
// here so future regressions on the public-facing footer line are
// caught at unit-test time, before a Feishu E2E round-trip.
package feishu

import (
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/gateway"
)

func TestFormatSessionFooter_NilContextReturnsEmpty(t *testing.T) {
	if got := formatSessionFooter(nil); got != "" {
		t.Fatalf("formatSessionFooter(nil) = %q, want \"\"", got)
	}
}

func TestFormatSessionFooter_AllZeroReturnsEmpty(t *testing.T) {
	ctx := &gateway.SessionContext{Agent: "", Model: ""}
	if got := formatSessionFooter(ctx); got != "" {
		t.Fatalf("empty SessionContext should yield \"\", got %q", got)
	}
}

func TestFormatSessionFooter_AgentModelOnly(t *testing.T) {
	ctx := &gateway.SessionContext{Agent: "claude", Model: "opus-4-5"}
	got := formatSessionFooter(ctx)
	want := "claude · opus-4-5"
	if got != want {
		t.Fatalf("formatSessionFooter() = %q, want %q", got, want)
	}
}

func TestFormatSessionFooter_TokenSegments(t *testing.T) {
	ctx := &gateway.SessionContext{
		Agent: "claude",
		Model: "opus-4-5",
		CumulativeUsage: gateway.UsageInfo{
			InputTokens:              11_700,
			OutputTokens:             1_500,
			CacheCreationInputTokens: 600, // counted into "in"
			CacheReadInputTokens:     8_200,
			CostUSD:                  0.087,
		},
	}
	got := formatSessionFooter(ctx)
	want := "claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087"
	if got != want {
		t.Fatalf("formatSessionFooter() mismatch:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestFormatSessionFooter_OmitsZeroSegments(t *testing.T) {
	tests := []struct {
		name string
		ctx  *gateway.SessionContext
		want string
	}{
		{
			name: "no input but has output",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				CumulativeUsage: gateway.UsageInfo{OutputTokens: 234},
			},
			want: "claude · opus-4-5 · ↑ 234 · Total 234",
		},
		{
			name: "only cache hits",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				CumulativeUsage: gateway.UsageInfo{CacheReadInputTokens: 5_600},
			},
			want: "claude · opus-4-5 · ↻ 5.6k cached · Total 5.6k",
		},
		{
			name: "cost only",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				CumulativeUsage: gateway.UsageInfo{CostUSD: 1.245},
			},
			want: "claude · opus-4-5 · $1.245",
		},
		{
			name: "no cost (omitted)",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				CumulativeUsage: gateway.UsageInfo{
					InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200,
				},
			},
			want: "claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatSessionFooter(tc.ctx)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatSessionFooter_LargeNumbers(t *testing.T) {
	ctx := &gateway.SessionContext{
		Agent: "claude", Model: "opus-4-5",
		CumulativeUsage: gateway.UsageInfo{
			InputTokens:              156_000,
			OutputTokens:             18_000,
			CacheCreationInputTokens: 0,
			CacheReadInputTokens:     1_200_000,
			CostUSD:                  1.245,
		},
	}
	got := formatSessionFooter(ctx)
	want := "claude · opus-4-5 · ↓ 156.0k · ↻ 1.2M cached · ↑ 18.0k · Total 1.4M · $1.245"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Sanity: 156000 + 0 + 1200000 + 18000 = 1374000 → 1.4M
	// (Go's %.1f rounds 1.374 to 1.4)
	if !strings.Contains(got, "1.4M") {
		t.Fatalf("expected 1.4M total, got %q", got)
	}
}

func TestFormatSessionFooter_StableAcrossReRenders(t *testing.T) {
	// Same input must always produce the same string — receipt
	// PATCH diffing relies on body equality.
	ctx := &gateway.SessionContext{
		Agent: "claude", Model: "opus-4-5",
		CumulativeUsage: gateway.UsageInfo{
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

func TestAbbrevTokens(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},          // caller is expected to skip, but defensively still formats
		{1, "1"},          // raw integer
		{999, "999"},      // boundary
		{1_000, "1.0k"},   // boundary
		{12_345, "12.3k"}, // one decimal
		{999_999, "1000.0k"}, // one decimal, edge of M threshold
		{1_000_000, "1.0M"},  // boundary
		{1_234_567, "1.2M"},
	}
	for _, tc := range tests {
		got := abbrevTokens(tc.in)
		if got != tc.want {
			t.Errorf("abbrevTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
