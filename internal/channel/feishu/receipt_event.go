// Package feishu — event → LogEntry translator for the rolling-log
// MessageReceipt (F-25 spec §6, v0.3 update). Kept in its own file
// so the receipt struct / rendering code (receipt.go) stays
// focused on the lifecycle, and the type-aware event mapping stays
// grouped here.
package feishu

import (
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// thinkingPrefix is the sentinel the claudecode bridge prepends to
// every thinking block before emitting EventText. The renderer
// uses this to render thinking as 💭 and a final reply as 💬 even
// though both arrive as the same EventKind.
const thinkingPrefix = "[思考] "

// eventToEntry converts one agent.AgentEvent into a LogEntry
// suitable for the rolling reply-message log. Returns
// (entry, true) for events we surface, (_, false) for ones we skip
// (e.g. late events after completion, permission requests — those
// have their own dedicated renderer path).
//
// Caller does NOT hold r.mu; eventToEntry is pure.
func eventToEntry(ev agent.AgentEvent, now time.Time) (LogEntry, bool) {
	ae := ev

	switch ae.Kind {
	case agent.EventText:
		text := strings.TrimSpace(ae.Text)
		if text == "" {
			return LogEntry{}, false
		}
		if strings.HasPrefix(text, thinkingPrefix) {
			return LogEntry{
				Time:  now,
				Icon:  "💭",
				Text:  truncateForLog(strings.TrimPrefix(text, thinkingPrefix), perEntryMaxBytes),
				Kind:  "thinking",
			}, true
		}
		return LogEntry{
			Time:  now,
			Icon:  "💬",
			Text:  truncateForLog(text, perEntryMaxBytes),
			Kind:  "reply",
		}, true

	case agent.EventToolStart:
		if ae.ToolStart == nil || ae.ToolStart.Name == "" {
			return LogEntry{}, false
		}
		text := ae.ToolStart.Name
		if ae.ToolStart.Args != "" {
			text = ae.ToolStart.Name + "(" + truncateForLog(ae.ToolStart.Args, perEntryMaxBytes-len(ae.ToolStart.Name)-2) + ")"
		}
		return LogEntry{
			Time:  now,
			Icon:  "🔧",
			Text:  truncateForLog(text, perEntryMaxBytes),
			Kind:  "tool_start",
		}, true

	case agent.EventToolEnd:
		if ae.ToolEnd == nil || ae.ToolEnd.Name == "" {
			return LogEntry{}, false
		}
		icon := "✅"
		var body string
		if ae.ToolEnd.Err != nil {
			icon = "❌"
			body = fmt.Sprintf("%s failed: %s", ae.ToolEnd.Name, ae.ToolEnd.Err.Error())
		} else if ae.ToolEnd.Output != "" {
			// Result is the most useful signal for the user — show
			// a short summary so they can tell what the agent
			// actually did (e.g. "Read /tmp/main.go → 47 lines").
			body = fmt.Sprintf("%s → %s", ae.ToolEnd.Name, ae.ToolEnd.Output)
		} else {
			// Fallback when the bridge forgot to populate Output.
			body = ae.ToolEnd.Name + " done"
		}
		return LogEntry{
			Time:  now,
			Icon:  icon,
			Text:  truncateForLog(body, perEntryMaxBytes),
			Kind:  "tool_end",
		}, true

	case agent.EventError:
		msg := "unknown error"
		if ae.Error != nil && ae.Error.Err != nil {
			msg = ae.Error.Err.Error()
		}
		return LogEntry{
			Time:  now,
			Icon:  "❌",
			Text:  truncateForLog("error: "+msg, perEntryMaxBytes),
			Kind:  "error",
		}, true

	case agent.EventPermission:
		// Permissions are rendered as their own interactive card by
		// Renderer.renderPermission. The receipt's reply log
		// doesn't carry the question text (the card does). Skip.
		return LogEntry{}, false

	case agent.EventDone:
		// SetCompleted handles the lifecycle transition. No
		// per-event entry needed — the header shows the timestamp.
		return LogEntry{}, false

	case agent.EventResult:
		// Final assistant reply — distinct icon (📝) so the user
		// can tell the "delivered answer" from rolling-log EventText
		// entries (💬). Skip when empty AND not in error state so
		// pure zero-length results don't pad the log.
		if ae.Result == nil {
			return LogEntry{}, false
		}
		text := ae.Result.Text
		if strings.TrimSpace(text) == "" && !ae.Result.IsError {
			return LogEntry{}, false
		}
		icon := "📝"
		if ae.Result.IsError {
			icon = "⚠️"
		}
		return LogEntry{
			Time: now,
			Icon: icon,
			Text: truncateForLog(text, perEntryMaxBytes),
			Kind: "result",
		}, true

	case agent.EventUsage:
		// Footer-style entry. Format carries cache stats + cost
		// when present so the user can spot cache misses / cost
		// anomalies at a glance.
		if ae.Usage == nil {
			return LogEntry{}, false
		}
		body := formatUsageText(ae.Usage)
		if body == "" {
			return LogEntry{}, false
		}
		return LogEntry{
			Time: now,
			Icon: "📊",
			Text: truncateForLog(body, perEntryMaxBytes),
			Kind: "usage",
		}, true

	case agent.EventCompaction:
		// Mid-turn context compaction — surface the same icon as
		// Claude Code's own spinner (✶) so users recognize the
		// pattern from the CLI.
		return LogEntry{
			Time: now,
			Icon: "✶",
			Text: "Compacting conversation…",
			Kind: "compaction",
		}, true

	case agent.EventInit:
		// Session bootstrap — surface session_id + model so users
		// can confirm the session they're talking to. Skip when
		// SessionID is empty (defensive; the bridge always emits
		// one but third-party bridges may not).
		if ae.Init == nil || ae.Init.SessionID == "" {
			return LogEntry{}, false
		}
		return LogEntry{
			Time: now,
			Icon: "🆔",
			Text: truncateForLog(
				fmt.Sprintf("session %s · model %s", ae.Init.SessionID, ae.Init.Model),
				perEntryMaxBytes,
			),
			Kind: "init",
		}, true

	default:
		return LogEntry{}, false
	}
}

// formatUsageText renders a UsageEvent as a single log line. Returns
// "" when all counts are zero so the caller can drop the entry
// entirely (mirrors the zero-count guard in stream.go::decodeUsage).
//
// Shape (v0.3): "<total> tokens (in <n> · out <n>[ · cache read <n>][ · cache create <n>][ · $X.XXXX])"
//
// Cost is shown with 4 decimal places to match Claude Code's own
// reporting. Cache stats are omitted when zero so the common case
// stays short ("1.2k tokens (in 800 · out 400)").
func formatUsageText(u *agent.UsageEvent) string {
	total := u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if total == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d tokens (in %d · out %d", total, u.InputTokens, u.OutputTokens)
	if u.CacheReadInputTokens > 0 {
		fmt.Fprintf(&b, " · cache read %d", u.CacheReadInputTokens)
	}
	if u.CacheCreationInputTokens > 0 {
		fmt.Fprintf(&b, " · cache create %d", u.CacheCreationInputTokens)
	}
	if u.CostUSD > 0 {
		fmt.Fprintf(&b, " · $%.4f", u.CostUSD)
	}
	b.WriteByte(')')
	return b.String()
}

// truncateForLog returns s clipped to max bytes with an ellipsis
// suffix when truncation occurred. The returned string is always
// valid UTF-8 (we round at the last rune boundary inside the
// budget so we never slice a multi-byte sequence).
func truncateForLog(s string, max int) string {
	if max <= 3 {
		// Pathological: caller asked for so few bytes that the
		// ellipsis alone wouldn't fit. Return a single "…" so
		// something still renders.
		return "…"
	}
	if len(s) <= max {
		return s
	}
	// Leave room for the trailing "…" (3 bytes in UTF-8).
	budget := max - 3
	// Walk runes, not bytes, so we never split a codepoint.
	for i := 0; i < len(s); i++ {
		if i > budget {
			return s[:i] + "…"
		}
	}
	return s
}