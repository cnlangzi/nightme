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

	default:
		return LogEntry{}, false
	}
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