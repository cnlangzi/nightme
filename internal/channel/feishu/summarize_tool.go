package feishu

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// toolCallArgsMaxBytes caps args shown in the call line
	// (`● Tool(args)`) so a long Edit/Write replacement or a
	// sprawling Bash command collapse with `...` instead of
	// overflowing the Feishu thread reply width. Mirrors Claude
	// Code's terminal UX where long tool args truncate to one
	// line with `…`. Rune-safe via truncate().
	toolCallArgsMaxBytes = 100

	// toolOutputPreviewBytes is intentionally unused — the
	// default-branch fallback no longer dumps output (PII risk).
	// Kept as a named constant for callers that want to opt in.
	toolOutputPreviewBytes = 200
)

// formatToolStartCall produces the "call" line for the thread
// reply, matching Claude Code's terminal UX:
//
//	● Bash(go build ./... 2>&1; echo "EXIT=$?")
//	● Read(/tmp/foo.go)
//
// Long args collapse with "..." so the line stays scannable in
// the Feishu thread. Args are truncated rune-safely (see
// truncate) so CJK paths / emoji filenames never get sliced
// mid-codepoint.
//
// Two-line UX: the Adapter posts this as one thread reply on
// OutToolStart, then a separate "result" reply (see
// summarizeToolResult) on OutToolEnd. The user sees the call
// appear immediately when the agent invokes the tool, and the
// result line lands in the same thread position when the tool
// returns.
func formatToolStartCall(name, args string) string {
	if args == "" {
		return "● " + name
	}
	return "● " + name + "(" + truncate(args, toolCallArgsMaxBytes) + ")"
}

// summarizeToolResult produces the "result" line for the thread
// reply, matching Claude Code's `⎿  …` continuation:
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

// summarizeToolEnd is the legacy single-line summary. Kept for
// backward compat with tests that pin the v0.2.x format. New
// code should use formatToolStartCall + summarizeToolResult.
// Deprecated: use formatToolStartCall + summarizeToolResult
// for the Claude Code-style two-line UX.
func summarizeToolEnd(name, args, output string, err error) string {
	if err != nil {
		return fmt.Sprintf("❌ %s failed: %s", name, err.Error())
	}
	switch strings.ToLower(name) {
	case "read":
		return fmt.Sprintf("📄 %s %s → %d lines", name, args, countLines(output))
	case "write":
		return fmt.Sprintf("📝 %s %s → %d bytes", name, args, len(output))
	case "edit", "multiedit":
		return fmt.Sprintf("✏️ %s %s → applied", name, args)
	case "bash":
		cmd := truncate(args, toolArgsMaxBytes)
		return fmt.Sprintf("💻 Bash `%s` → %d lines", cmd, countLines(output))
	case "grep":
		return fmt.Sprintf("🔍 Grep → %d matches across %d files",
			countLines(output), countUniqueFiles(output))
	case "glob":
		return fmt.Sprintf("📂 Glob → %d files", countLines(output))
	case "webfetch":
		return fmt.Sprintf("🌐 WebFetch %s → %d chars fetched", args, len(output))
	case "websearch":
		return fmt.Sprintf("🔎 WebSearch %q → %d results",
			truncate(args, toolArgsMaxBytes), countLines(output))
	default:
		return fmt.Sprintf("🔧 %s → %s", name, truncate(output, toolOutputPreviewBytes))
	}
}

// toolArgsMaxBytes is the legacy truncate budget for summarizeToolEnd.
// New code should use toolCallArgsMaxBytes (call line) or no
// truncation (result line is summary-only).
const toolArgsMaxBytes = 80

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
