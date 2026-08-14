package agent

import (
	"strconv"
	"strings"
)

// FormatSessionID returns " [session_id=X]" when s is non-empty,
// otherwise "". Bridges compose this into failure-path audit
// suffixes so operators can grep daemon logs across bridges for
// a given session.
func FormatSessionID(s string) string {
	if s == "" {
		return ""
	}
	return " [session_id=" + s + "]"
}

// FormatModel returns " [model=X]" when model is non-empty.
// Today only the acp bridge populates this on failure paths;
// claudecode / pi surface FormatSubtype / FormatUsage instead
// because they have a captured Result when the failure occurs.
func FormatModel(model string) string {
	if model == "" {
		return ""
	}
	return " [model=" + model + "]"
}

// FormatSubtype returns " [subtype=X]" when s is non-empty.
// The result-event subtype categorises the turn (Claude Code:
// "success" / "error_max_turns" / "refusal" / "compact"; pi:
// "stop" / "toolUse" / "error"; etc.). Heterogeneous across
// bridges — see RunResult.Subtype doc.
func FormatSubtype(s string) string {
	if s == "" {
		return ""
	}
	return " [subtype=" + s + "]"
}

// FormatUsage returns " [usage in=N out=N cache_read=N]" when u
// is non-nil, otherwise "". Captured from the same wire event as
// Result.Text — bridges that surface a typed RunResult on failure
// paths (claudecode / pi) include it; acp doesn't (its failure
// paths happen before Result arrives).
func FormatUsage(u *UsageInfo) string {
	if u == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(" [usage in=")
	b.WriteString(strconv.Itoa(u.InputTokens))
	b.WriteString(" out=")
	b.WriteString(strconv.Itoa(u.OutputTokens))
	b.WriteString(" cache_read=")
	b.WriteString(strconv.Itoa(u.CacheReadInputTokens))
	b.WriteByte(']')
	return b.String()
}