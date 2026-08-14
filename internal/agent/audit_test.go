package agent

import "testing"

// TestFormatAuditFields locks in the per-field audit-suffix
// primitives. The bridge-level appendAuditFields helpers compose
// from these; a regression here would silently change every
// bridge's log format. Format is [key=value] bracketed with a
// single leading space, empty inputs return "" — see the per-helper
// doc comments for the exact contract.
func TestFormatAuditFields(t *testing.T) {
	t.Run("FormatSessionID", func(t *testing.T) {
		if got := FormatSessionID(""); got != "" {
			t.Errorf("empty: got %q, want \"\"", got)
		}
		if got := FormatSessionID("sess-abc"); got != " [session_id=sess-abc]" {
			t.Errorf("set: got %q", got)
		}
	})
	t.Run("FormatModel", func(t *testing.T) {
		if got := FormatModel(""); got != "" {
			t.Errorf("empty: got %q", got)
		}
		if got := FormatModel("claude-opus"); got != " [model=claude-opus]" {
			t.Errorf("set: got %q", got)
		}
	})
	t.Run("FormatSubtype", func(t *testing.T) {
		if got := FormatSubtype(""); got != "" {
			t.Errorf("empty: got %q", got)
		}
		if got := FormatSubtype("success"); got != " [subtype=success]" {
			t.Errorf("set: got %q", got)
		}
	})
	t.Run("FormatUsage", func(t *testing.T) {
		if got := FormatUsage(nil); got != "" {
			t.Errorf("nil: got %q", got)
		}
		u := &UsageInfo{InputTokens: 12, OutputTokens: 34, CacheReadInputTokens: 56}
		want := " [usage in=12 out=34 cache_read=56]"
		if got := FormatUsage(u); got != want {
			t.Errorf("set: got %q, want %q", got, want)
		}
	})
}