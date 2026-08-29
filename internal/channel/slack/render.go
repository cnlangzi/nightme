package slack

import (
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/messages"
	slackgo "github.com/slack-go/slack"
)

// taskFieldMaxRunes is Slack's per-chunk ceiling for task_update and
// plan_update fields.
const taskFieldMaxRunes = 256

// splitRunes cuts s into pieces of at most maxRunes runes, preferring
// a paragraph, then line, then space boundary so a split does not
// land inside a word or mid-way through a markdown construct.
//
// Unlike the Feishu splitter this is NOT load-bearing for layout —
// Slack renders the markdown itself and the streaming API has no
// envelope limit. It only exists to respect the 12,000-rune chunk
// ceiling.
func splitRunes(s string, maxRunes int) []string {
	if maxRunes <= 0 {
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return []string{s}
	}
	var out []string
	for len(runes) > maxRunes {
		cut := softCut(runes, maxRunes)
		out = append(out, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}

// softCut finds the best split point at or below maxRunes.
func softCut(runes []rune, maxRunes int) int {
	window := runes[:maxRunes]
	// Paragraph boundary is the least disruptive.
	if idx := lastIndexOfPair(window, '\n', '\n'); idx > 0 {
		return idx + 1
	}
	for i := len(window) - 1; i > 0; i-- {
		if window[i] == '\n' {
			return i + 1
		}
	}
	for i := len(window) - 1; i > 0; i-- {
		if window[i] == ' ' {
			return i + 1
		}
	}
	return maxRunes
}

func lastIndexOfPair(runes []rune, a, b rune) int {
	for i := len(runes) - 2; i >= 0; i-- {
		if runes[i] == a && runes[i+1] == b {
			return i + 1
		}
	}
	return -1
}

// truncateRunes clamps s to maxRunes, appending an ellipsis when it
// actually had to cut.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

// footerBlocks renders the StatusBar lines as chat.stopStream
// finalization blocks: a divider plus one context block, which is
// Slack's muted small-text treatment and the closest analogue to the
// grey <hr>-prefixed footer the Feishu card uses.
//
// Returns nil for empty input so callers can pass it through
// unconditionally.
func footerBlocks(lines []string) []slackgo.Block {
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			cleaned = append(cleaned, line)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	text := strings.Join(cleaned, "\n")
	return []slackgo.Block{
		slackgo.NewDividerBlock(),
		slackgo.NewContextBlock("nightme_statusbar",
			slackgo.NewTextBlockObject(slackgo.MarkdownType, text, false, false),
		),
	}
}

// heartbeatText renders the F-63 progress signal.
//
// Two mutually exclusive forms, matching the Feishu contract
// (docs/feat/F-63-heartbeat.md §3.6): before any activity the bare
// "🤖 Working" placeholder; once the agent has actually thought or
// called a tool, the counters replace it entirely rather than
// prefixing them, because "Working" plus a live counter says the
// same thing twice.
func heartbeatText(hb *messages.HeartbeatSnapshot) string {
	if hb == nil || (hb.ThinkCount == 0 && hb.ToolCount == 0) {
		return "🤖 Working"
	}
	parts := make([]string, 0, 3)
	if hb.ThinkCount > 0 {
		parts = append(parts, fmt.Sprintf("💭 %d", hb.ThinkCount))
	}
	if hb.ToolCount > 0 {
		parts = append(parts, fmt.Sprintf("🔧 %d", hb.ToolCount))
	}
	if !hb.LastBeatAt.IsZero() {
		parts = append(parts, "⏱ "+hb.LastBeatAt.Format("15:04:05"))
	}
	return strings.Join(parts, " · ")
}

// toolTitle renders the one-line label for a tool task card.
func toolTitle(tool *messages.ToolInfo) string {
	if tool == nil {
		return "tool"
	}
	name := strings.TrimSpace(tool.Name)
	if name == "" {
		name = "tool"
	}
	args := strings.TrimSpace(tool.Args)
	if args == "" {
		return name
	}
	return truncateRunes(name+"("+collapseWhitespace(args)+")", taskFieldMaxRunes)
}

// toolTaskID derives the task_update id that ties a tool's start and
// end into a single card. Slack merges chunks sharing an id, so the
// id must be identical across the pair and distinct between
// concurrent tools in the same turn.
//
// messages.ToolInfo carries no bridge-supplied call id (it is
// {Name, Args, Output, Err}), so the pairing is positional: the
// adapter keeps a per-turn FIFO of started tools and pops it on the
// matching end. That is exactly how the Feishu adapter solves the
// same problem (pushToolStart / popToolStart in
// internal/channel/feishu/tool_thread_merge.go).
func toolTaskID(seq int) string {
	return fmt.Sprintf("tool-%d", seq)
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
