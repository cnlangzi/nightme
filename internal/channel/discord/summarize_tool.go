package discord

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cnlangzi/nightme/internal/agent"
)

const (
	// toolCallArgsMaxBytes caps args shown in the call line
	// (`● Tool(args)`) so a long Edit/Write replacement or a
	// sprawling Bash command collapse with `...` instead of
	// overflowing the chain chunk width. Mirrors Claude Code's
	// terminal UX where long tool args truncate to one line
	// with `…`. Rune-safe via truncate().
	toolCallArgsMaxBytes = 100

	// toolOutputPreviewBytes is intentionally unused — the
	// default-branch fallback no longer dumps output (PII risk).
	// Kept as a named constant for callers that want to opt in.
	toolOutputPreviewBytes = 200

	// taskListBudgetRunes is the maximum total length of the
	// rendered task checklist (rune count). Picked to comfortably
	// fit inside Discord's 2000-char message body limit with
	// plenty of slack for the header emoji and a handful of tasks.
	// Long lists are truncated (in-progress first, then pending,
	// then completed).
	taskListBudgetRunes = 1500

	// taskListHeader is the visual header Discord renders at the
	// top of an OutTaskCreate / OutTaskUpdate bubble. Mirrors
	// feishu's `**📋 Tasks**` shape minus the markdown bold —
	// Discord's native renderer treats `**…**` literally.
	taskListHeader = "📋 Tasks"

	// taskListMore is appended to the last visible line when the
	// input did not fit within the budget.
	taskListMore = "…"

	// taskListOverflowPlaceholder is the single-line fallback when
	// the renderer has to drop every line to fit the budget.
	taskListOverflowPlaceholder = "1. [ ] …"
)

// formatToolStartCall produces the "call" line for chain entry,
// matching Claude Code's terminal UX:
//
//	● Bash(go build ./... 2>&1; echo "EXIT=$?")
//	● Read(/tmp/foo.go)
//
// Long args collapse with "..." so the line stays scannable in
// the Discord chat. Args are truncated rune-safely (see truncate)
// so CJK paths / emoji filenames never get sliced mid-codepoint.
//
// Mirrors internal/channel/telegram/summarize_tool.go verbatim —
// kept package-local so each channel can evolve independently
// without leaking types into shared packages.
func formatToolStartCall(name, args string) string {
	if args == "" {
		return "● " + name
	}
	return "● " + name + "(" + displayToolArgs(args) + ")"
}

// displayToolArgs is the call-line args body. JSON objects with a
// file_path (Read/Edit/Write) compact to basename + offset/limit so
// a long absolute path cannot eat the 100-byte budget and hide the
// only field that distinguishes two calls of the same tool.
func displayToolArgs(args string) string {
	if compact := compactJSONToolArgs(args); compact != "" {
		return compact
	}
	return truncate(args, toolCallArgsMaxBytes)
}

func compactJSONToolArgs(args string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil || len(m) == 0 {
		return ""
	}
	path, _ := m["file_path"].(string)
	if path == "" {
		path, _ = m["path"].(string)
	}
	if path == "" {
		return ""
	}
	shown := filepath.Base(path)
	var extra []string
	for _, k := range []string{"offset", "limit"} {
		if v, ok := m[k]; ok {
			extra = append(extra, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(extra) > 0 {
		shown = shown + " " + strings.Join(extra, " ")
	}
	return truncate(shown, toolCallArgsMaxBytes)
}

// summarizeToolResult produces the "result" line for chain entry,
// matching Claude Code's `⎿  …` continuation:
//
//	⎿  📄 Read → 47 lines
//	⎿  ✏️  applied
//	⎿  💻 Bash → 3 lines
//	⎿  ❌ Bash failed: exit code 1
//
// Args are NOT included — they live on the preceding call line.
// This matches Claude Code's UX where the `⎿` lines are result
// details under a tool call.
//
// The default branch reports only byte size, NOT output bytes
// (PII protection for un-classified tools whose output may
// contain secrets). Per-tool-type heuristics give the user the
// signal (file path, line count) without dumping the output.
//
// err wins over the success path.
func summarizeToolResult(name, output string, err error) string {
	if err != nil {
		return fmt.Sprintf("⎿  ❌ %s failed: %s", name, err.Error())
	}
	switch strings.ToLower(name) {
	case "read":
		return "⎿  📄 Read → " + strconv.Itoa(countLines(output)) + " lines"
	case "write":
		return "⎿  📝 Write → " + strconv.Itoa(len(output)) + " bytes"
	case "edit", "multiedit":
		return "⎿  ✏️  applied"
	case "bash":
		return "⎿  💻 Bash → " + strconv.Itoa(countLines(output)) + " lines"
	case "grep":
		return "⎿  🔍 Grep → " + strconv.Itoa(countLines(output)) +
			" matches across " + strconv.Itoa(countUniqueFiles(output)) + " files"
	case "glob":
		return "⎿  📂 Glob → " + strconv.Itoa(countLines(output)) + " files"
	case "webfetch":
		return "⎿  🌐 WebFetch → " + strconv.Itoa(len(output)) + " chars fetched"
	case "websearch":
		return "⎿  🔎 WebSearch → " + strconv.Itoa(countLines(output)) + " results"
	default:
		// Intentionally do NOT surface any output bytes for
		// un-classified tools. Custom MCP servers and unknown
		// bridges may carry secrets / credentials / PII;
		// reporting only the byte size gives the user the
		// signal without leaking the contents.
		return "⎿  🔧 " + name + " → " + strconv.Itoa(len(output)) + " bytes"
	}
}

// toolResultLine renders the result of a tool call.
//
// The summary is the producer's verdict: the producer knows the text
// is safe to show, so it goes to the chat as it is. A call without a
// summary falls back to the per-tool heuristics over the raw output —
// a coding agent's output may hold file contents, so it is only ever
// summarized, never shown.
func toolResultLine(name, output, summary string, err error) string {
	if err != nil {
		return summarizeToolResult(name, output, err)
	}
	if summary != "" {
		return "⎿  ✅ " + name + " → " + summary
	}
	return summarizeToolResult(name, output, nil)
}

// countLines returns the line count of s. An empty string is zero
// lines; trailing newlines are not counted as separate lines (a
// file ending with "\n" has the same line count as one without).
// "a\nb\nc" → 3, "a\nb\n" → 2, "\n" → 1, "" → 0.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	s = strings.TrimRight(s, "\n")
	return strings.Count(s, "\n") + 1
}

// countUniqueFiles extracts the unique file paths from a Grep-style
// "path:line:match" output. Lines without a colon are ignored.
func countUniqueFiles(s string) int {
	seen := make(map[string]struct{})
	for _, line := range strings.Split(s, "\n") {
		if idx := strings.Index(line, ":"); idx > 0 {
			seen[line[:idx]] = struct{}{}
		}
	}
	return len(seen)
}

// truncate shortens s to at most max bytes, appending "..." when
// the input is longer. max <= 3 short-circuits to returning s
// unchanged. Walks runes (not raw bytes) so a 3-byte CJK codepoint
// or 4-byte emoji is never sliced mid-sequence — the output is
// always valid UTF-8.
//
// Strict budget: output length never exceeds max bytes. We find
// the largest rune-start position <= budget, then append "..."
// (3 bytes) for the indicator. For CJK input with budget=97 we
// keep 32 runes (96 bytes) + "..." = 99 bytes; the 33rd rune
// (bytes 96-98) would push output over the budget so it gets
// dropped.
func truncate(s string, max int) string {
	if max <= 3 || len(s) <= max {
		return s
	}
	budget := max - 3
	cut := 0
	for i := range s {
		if i > budget {
			break
		}
		cut = i
	}
	return s[:cut] + "..."
}

// formatTaskList renders the receipt's task snapshot as a single
// Discord bubble. Output structure:
//
//	📋 Tasks
//
//	1. [ ] task one
//	2. [>] task two (writing unit tests)
//	3. [v] task three
//
// Discord's native renderer does not treat the markers as checkboxes
// (no checkbox widget), but the markdown list shape is stable across
// clients (mobile / desktop / web) and matches the user-visible
// convention Claude Code uses in its terminal output.
//
// The rows keep the order the sender listed them in, and every row
// is the shared row of agent.FormatTaskRow. An empty Items slice
// returns "" so the caller can skip the bubble entirely.
func formatTaskList(items []agent.AgentTaskItem) string {
	if len(items) == 0 {
		return ""
	}
	order := make([]int, 0, len(items))
	for i, it := range items {
		// Filter TaskDeleted defensively: the bridge is supposed to
		// remove deleted tasks before emitting, but a corrupt snapshot
		// must not produce a visible "deleted" row.
		if it.Status == agent.TaskDeleted {
			continue
		}
		order = append(order, i)
	}

	lines := make([]string, 0, len(order))
	total := 0
	rendered := 0
	for n, idx := range order {
		line := agent.FormatTaskRow(n+1, items[idx])
		cost := utf8.RuneCountInString(line) + 1
		if total+cost > taskListBudgetRunes {
			break
		}
		lines = append(lines, line)
		total += cost
		rendered++
	}
	if rendered == 0 {
		return taskListHeader + "\n\n" + taskListOverflowPlaceholder
	}
	body := strings.Join(lines, "\n")
	if rendered < len(order) {
		body += "\n" + taskListMore
	}
	return taskListHeader + "\n\n" + body
}
