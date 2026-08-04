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
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
)

// thinkingPrefix is the sentinel the claudecode bridge prepends to
// every thinking block before emitting EventText. The renderer
// uses this to render thinking as 💭 and a final reply as 💬 even
// though both arrive as the same EventKind.
//
// CONTRACT (v1.3.x): the prefix MUST be present on the EventText
// the receipt receives for thinking entries. The Gateway's
// translate.go strips this prefix when it emits OutThinking; the
// feishu adapter's OutThinking handler re-prepends it before
// calling receipt.Append so receipt_event.go's HasPrefix detection
// continues to fire. See docs/channel/feishu.md §13.1 / §15.3.
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
			// F-34: thinking no longer folds into the receipt
			// card; the adapter routes it to a Feishu thread
			// reply instead (see Adapter.Send). Skip.
			return LogEntry{}, false
		}
		// F-40: no longer truncate to perEntryMaxBytes (600). The
		// full reply text flows into LogEntry.Text; buildReceiptCard
		// splits long entries into multiple `div` elements via
		// splitMarkdownForDivs (≤ divTextCharLimit runes each).
		// Truly oversized replies (runes > perEntryMaxRunes) are
		// diverted to a stand-alone ReplyInThreadAndChat by the
		// Adapter.Send(OutReply) routing logic before this path is
		// reached.
		return LogEntry{
			Time: now,
			Icon: "💬",
			Text: text,
			Kind: "reply",
		}, true

	case agent.EventToolStart:
		// F-34: tool_start is posted as a thread reply with a
		// type-aware summary; the receipt card no longer carries
		// it.
		return LogEntry{}, false

	case agent.EventToolEnd:
		// F-34: tool_end is posted as a thread reply with a
		// type-aware summary (summarizeToolEnd); the receipt
		// card no longer carries it.
		return LogEntry{}, false

	case agent.EventError:
		msg := "unknown error"
		if ae.Error != nil && ae.Error.Err != nil {
			msg = ae.Error.Err.Error()
		}
		return LogEntry{
			Time: now,
			Icon: "❌",
			Text: truncateForLog("error: "+msg, perEntryMaxBytes),
			Kind: "error",
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

	case agent.EventUsage:
		// EventUsage is intentionally NOT rendered as a log
		// entry — the same numbers live in the receipt footer
		// (rendered via OutUsage → SetFooter). Re-emitting the
		// stats inline would just clutter the rolling log with
		// numbers the user already sees at the bottom of the
		// card. Skip; the adapter still extracts the token
		// counts from OutboundMessage.Meta and refreshes the
		// footer.
		return LogEntry{}, false

	case agent.EventCompaction:
		// F-34: compaction is posted as a thread reply ("✶
		// Compacting conversation…"). The receipt card no
		// longer carries it.
		return LogEntry{}, false

	case agent.EventInit:
		// EventInit is intentionally NOT rendered as a log
		// entry — the agent name and model already live in
		// the receipt footer (set on OutInit → SetAgentMeta),
		// and the session_id is rarely useful in the chat
		// surface. Skip.
		return LogEntry{}, false

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

// truncateForLog returns s clipped to max characters with an ellipsis
// suffix when truncation occurred. The returned string is always
// valid UTF-8 — we round at the last rune boundary inside the budget
// so we never slice a multi-byte sequence.
//
// F-37: this is rune-aware (was previously byte-based despite the
// comment). `perEntryMaxBytes` and `perEntryMaxRunes` both call this
// function; the unit is "characters" regardless of which const was
// passed. For Chinese / emoji content (where 1 char = 3-4 bytes),
// the cap now correctly counts chars rather than bytes.
//
// F-39 follow-up: also used by sendResultAsReply (result_render.go
// calls it for the OutResult envelope defense) so the rolling-log
// truncation policy and the result-card truncation policy stay in
// sync. Single source of truth.
func truncateForLog(s string, max int) string {
	if max <= 0 {
		// No room for any content (not even the ellipsis).
		return ""
	}
	if max == 1 {
		// Only the ellipsis fits.
		return "…"
	}
	// Fast path: every UTF-8 rune is 1-4 bytes. If the byte
	// length fits inside 4×max we know the rune count fits too
	// without allocating a []rune slice. This skips the
	// per-event allocation for the common no-truncation path
	// (most events are well under 600 runes).
	if len(s) <= max*4 {
		// Cheap exact check: if the byte length is also <= max
		// (ASCII case), we know the rune count <= max.
		if len(s) <= max {
			return s
		}
		// Still need a precise rune count for non-ASCII text
		// whose byte length is > max but ≤ 4×max.
		if utf8.RuneCountInString(s) <= max {
			return s
		}
	}
	// Slow path: count runes and truncate.
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	// Leave room for the trailing "…" (1 rune).
	return string(runes[:max-1]) + "…"
}
