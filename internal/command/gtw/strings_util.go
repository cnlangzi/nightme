package gtw

import "strings"

// splitNonEmptyLines splits s on \n and drops empty lines.
// Defensive against git status / git log output that pads
// with blank lines or trailing newlines. Returns the
// non-empty subset in order. Shared between close.go (dirty
// detection) and push.go (post-add status check).
func splitNonEmptyLines(s string) []string {
	out := make([]string, 0, 4)
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}