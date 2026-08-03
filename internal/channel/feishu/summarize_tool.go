package feishu

import (
	"fmt"
	"strings"
)

const (
	toolArgsMaxBytes       = 80
	toolOutputPreviewBytes = 200
)

// summarizeToolEnd produces a one-line summary of a tool's result.
// Per-tool-type heuristics so the user sees the signal (file path,
// line count, exit status) instead of a wall of raw output.
// Falls back to byte truncation for unknown tools. Err wins over
// the success path.
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
// unchanged.
func truncate(s string, max int) string {
	if max <= 3 || len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
