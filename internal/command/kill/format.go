// /kill reply rendering — produces the IM-friendly plain-text
// summary the handler returns via command.Reply.
//
// Sibling to internal/chatsession/format.go (FormatResetResults
// for /new), but lives in the kill package because the kill
// surface is fully owned by command/kill (see kill.go doc comment).
// Sharing formatReplyByteCap / truncateByBytes / etc. with the
// reset formatter would require a third "format" package; not
// worth it for two callers.
//
// Output templates (selected by the (killed, stale, failed) tuple):
//   - all empty:           "No active agents to kill."
//   - all killed:          "Stopped N agent session(s):\n  ✓ <name> @ <cwd>\n..."
//   - all stale:           "Cleared N stale agent session(s) ...:\n  • <name> @ <cwd> ..."
//   - mixed:               "<header>:\n  ✓ ... \n  • ... \n  ✗ ...\n..."
//
// Errors surface per-row (never swallowed). Output capped to 4 KB
// total bytes (Feishu single-message limit) + "...and N more"
// tail. Rows sorted by typed priority (success → failure →
// skipped), then by (Agent, Cwd) for stable display.
package kill

import (
	"fmt"
	"sort"
	"strings"
)

// FormatKillResults produces a human-readable summary of /kill's
// per-entry outcomes. The output is suitable for channel.Send
// (plain text, Feishu-renderable).
func FormatKillResults(results []Result) string {
	if len(results) == 0 {
		return "No active agents to kill."
	}

	rows := make([]resultRow, 0, len(results))
	var killed, stale, failed int
	for _, r := range results {
		row, bucket := renderKillRow(r)
		switch bucket {
		case bucketSuccess:
			killed++
		case bucketSkipped:
			stale++
		case bucketFailure:
			failed++
		}
		rows = append(rows, row)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].priority != rows[j].priority {
			return rows[i].priority < rows[j].priority
		}
		if rows[i].agent != rows[j].agent {
			return rows[i].agent < rows[j].agent
		}
		return rows[i].cwd < rows[j].cwd
	})

	header := buildKillHeader(killed, stale, failed)
	return truncateByBytes(header, rows, formatTail)
}

// formatRowBucket is the typed priority bucket for a formatter row.
// Lower values sort first. success < failure < skipped so the user
// sees ✓ first, then ✗, then • (previous alphabetical sort on the
// rendered strings placed `•` (U+2022) before `✓` (U+2713) before
// `✗` (U+2717), inverting the spec).
type formatRowBucket int

const (
	bucketSuccess formatRowBucket = iota
	bucketFailure
	bucketSkipped
)

// resultRow is one formatted line + the structured fields the
// sorter needs.
type resultRow struct {
	text       string
	priority   formatRowBucket
	agent, cwd string
}

// renderKillRow is FormatKillResults' per-row branch.
func renderKillRow(r Result) (resultRow, formatRowBucket) {
	if r.Error != nil {
		return resultRow{
			text: fmt.Sprintf("  ✗ %s @ %s — %s: %v",
				r.Agent, r.Cwd, humanAction(r.Action), r.Error),
			priority: bucketFailure,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketFailure
	}
	switch r.Action {
	case "killed":
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s", r.Agent, r.Cwd),
			priority: bucketSuccess,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSuccess
	case "stale-cleared":
		return resultRow{
			text:     fmt.Sprintf("  • %s @ %s — already exited, entry cleaned", r.Agent, r.Cwd),
			priority: bucketSkipped,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSkipped
	default:
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s — %s", r.Agent, r.Cwd, r.Action),
			priority: bucketSuccess,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSuccess
	}
}

// humanAction returns a short human-readable verb for an Action
// string (used in error messages).
func humanAction(action string) string {
	switch action {
	case "killed":
		return "kill"
	case "stale-cleared":
		return "stale-clear"
	default:
		return action
	}
}

// buildKillHeader mirrors the spec template wording for the
// top-of-reply summary line.
func buildKillHeader(killed, stale, failed int) string {
	if failed == 0 && stale == 0 {
		return fmt.Sprintf("Stopped %d agent session(s):", killed)
	}
	if killed == 0 && stale > 0 && failed == 0 {
		return fmt.Sprintf("Cleared %d stale agent session(s) (no live processes):", stale)
	}
	parts := make([]string, 0, 3)
	if killed > 0 {
		parts = append(parts, fmt.Sprintf("Stopped %d", killed))
	}
	if stale > 0 {
		parts = append(parts, fmt.Sprintf("%d stale entry cleared", stale))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return strings.Join(parts, ", ") + ":"
}

// formatReplyByteCap is the Feishu single-message payload limit.
// Both /kill and /new format strings cap here to keep the channel
// side from rejecting the message outright.
const formatReplyByteCap = 4096

// formatTail is the "...and N more" suffix appended when the
// output would otherwise exceed the byte cap.
const formatTail = "  ... and %d more"

// truncateByBytes joins rows with "\n" and caps the total byte
// length at formatReplyByteCap. Lines that would push the output
// over the cap are truncated and replaced with the formatTail
// summary. The header is always included.
func truncateByBytes(header string, rows []resultRow, tailFmt string) string {
	out := header
	for i, r := range rows {
		candidate := out + "\n" + r.text
		if len(candidate)+len(tailFmtFor(i, len(rows))) > formatReplyByteCap {
			hidden := len(rows) - i
			out = out + "\n" + fmt.Sprintf(tailFmt, hidden)
			return out
		}
		out = candidate
	}
	return out
}

// tailFmtFor returns the byte-length the tail would consume for
// the given (i, total) under the standard formatTail template.
func tailFmtFor(i, total int) string {
	return fmt.Sprintf(formatTail, total-i)
}