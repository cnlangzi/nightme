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
	want := []string{"🤖 claude · opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSessionFooterLines() = %v, want %v", got, want)
	}
}

func TestFormatSessionFooterLines_TokenSegments(t *testing.T) {
	ctx := &gateway.SessionContext{
		Agent: "claude",
		Model: "opus-4-5",
		CumulativeUsage: gateway.UsageInfo{
			InputTokens:              11_700,
			OutputTokens:             1_500,
			CacheCreationInputTokens: 600, // counted into "↓ in"
			CacheReadInputTokens:     8_200,
			CostUSD:                  0.087,
		},
	}
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖 claude · opus-4-5", "💰 ↓ 12.3k · ↻ 8.2k · ↑ 1.5k · 22.0k · $0.087"}
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
			name: "no input but has output",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				CumulativeUsage: gateway.UsageInfo{OutputTokens: 234},
			},
			want: []string{"🤖 claude · opus-4-5", "💰 ↑ 234 · 234"},
		},
		{
			name: "only cache hits",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				CumulativeUsage: gateway.UsageInfo{CacheReadInputTokens: 5_600},
			},
			want: []string{"🤖 claude · opus-4-5", "💰 ↻ 5.6k · 5.6k"},
		},
		{
			name: "cost only (no tokens)",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				CumulativeUsage: gateway.UsageInfo{CostUSD: 1.245},
			},
			want: []string{"🤖 claude · opus-4-5", "💰 $1.245"},
		},
		{
			name: "no cost (omitted)",
			ctx: &gateway.SessionContext{
				Agent: "claude", Model: "opus-4-5",
				CumulativeUsage: gateway.UsageInfo{
					InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200,
				},
			},
			want: []string{"🤖 claude · opus-4-5", "💰 ↓ 12.3k · ↻ 8.2k · ↑ 1.5k · 22.0k"},
		},
		{
			name: "tokens but no Agent / Model",
			ctx: &gateway.SessionContext{
				CumulativeUsage: gateway.UsageInfo{
					InputTokens: 5_000, OutputTokens: 200,
				},
			},
			want: []string{"💰 ↓ 5.0k · ↑ 200 · 5.2k"},
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
	got := formatSessionFooterLines(ctx)
	want := []string{"🤖 claude · opus-4-5", "💰 ↓ 156.0k · ↻ 1.2M · ↑ 18.0k · 1.4M · $1.245"}
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
	ctx := &gateway.SessionContext{
		Agent: "claude", Model: "opus-4-5",
		CumulativeUsage: gateway.UsageInfo{
			InputTokens: 12_300, OutputTokens: 1_500, CacheReadInputTokens: 8_200,
		},
	}
	got := formatSessionFooter(ctx)
	want := "🤖 claude · opus-4-5\n💰 ↓ 12.3k · ↻ 8.2k · ↑ 1.5k · 22.0k"
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
