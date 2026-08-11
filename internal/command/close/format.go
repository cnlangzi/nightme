// /close reply rendering — produces the IM-friendly plain-text
// summary the handler returns via command.Reply.
//
// Sibling to internal/chatsession/format.go (FormatResetResults
// for /new), but lives in the close package because the close
// surface is fully owned by command/close (see close.go doc comment).
// Sharing formatReplyByteCap / truncateByBytes / etc. with the
// reset formatter would require a third "format" package; not
// worth it for two callers.
//
// Output templates (selected by the (closed, stale, failed) tuple):
//   - all empty:           "No active agents to close."
//   - all closed:          "Closed N bridge process(es):\n  ✓ <name> @ <cwd>\n..."
//   - all stale:           "Skipped N already-exited bridges:\n  • <name> @ <cwd> ..."
//   - mixed:               "<header>:\n  ✓ ... \n  • ... \n  ✗ ...\n..."
//
// Errors surface per-row (never swallowed). Output capped to 4 KB
// total bytes (Feishu single-message limit) + "...and N more"
// tail. Rows sorted by typed priority (success → failure →
// skipped), then by (Agent, Cwd) for stable display.
//
// Note: /close preserves the AgentSession entry in the pool. The
// header wording reflects this — "Closed N bridge process(es)"
// rather than "Closed N agent session(s)" — because the session
// identity is kept; only the underlying child process is gone.
package close

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// FormatResults produces a human-readable summary of /close's
// per-entry outcomes. The output is suitable for channel.Send
// (plain text, Feishu-renderable).
func FormatResults(results []Result) string {
	if len(results) == 0 {
		return "No active agents to close."
	}

	rows := make([]resultRow, 0, len(results))
	var closed, stale, failed int
	for _, r := range results {
		row, bucket := renderRow(r)
		switch bucket {
		case bucketSuccess:
			closed++
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

	header := buildHeader(closed, stale, failed)
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

// renderRow is FormatResults' per-row branch.
func renderRow(r Result) (resultRow, formatRowBucket) {
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
	case "closed":
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s", r.Agent, r.Cwd),
			priority: bucketSuccess,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSuccess
	case "stale-cleared":
		return resultRow{
			text:     fmt.Sprintf("  • %s @ %s — already exited", r.Agent, r.Cwd),
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
	case "closed":
		return "close"
	case "stale-cleared":
		return "stale"
	default:
		return action
	}
}

// buildHeader mirrors the spec template wording for the
// top-of-reply summary line. Wording reflects the close-only-
// kill semantics: bridge processes are gone, but AgentSession
// entries (and their session IDs) stay in the pool.
func buildHeader(closed, stale, failed int) string {
	if failed == 0 && stale == 0 {
		return fmt.Sprintf("Closed %d bridge process(es) (sessions preserved):", closed)
	}
	if closed == 0 && stale > 0 && failed == 0 {
		return fmt.Sprintf("Skipped %d already-exited bridge(s):", stale)
	}
	parts := make([]string, 0, 3)
	if closed > 0 {
		parts = append(parts, fmt.Sprintf("%d closed", closed))
	}
	if stale > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", stale))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return strings.Join(parts, ", ") + ":"
}

// formatReplyByteCap is the Feishu single-message payload limit.
// Both /close and /new format strings cap here to keep the channel
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

// FormatResetResults produces a human-readable summary of /new's
// per-entry outcomes. Companion to FormatResults; same plain-text
// shape, same byte-based cap, same typed-priority sort.
//
// See docs/feat/F-43-close-new-graceful-and-reset.md §6.2.
func FormatResetResults(results []chatsession.ResetResult) string {
	if len(results) == 0 {
		return "Reset 0 sessions."
	}

	rows := make([]resultRow, 0, len(results))
	var running, dead, failed int
	for _, r := range results {
		row, bucket := renderResetRow(r)
		switch bucket {
		case bucketSuccess:
			running++
		case bucketSkipped:
			dead++
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

	header := buildResetHeader(running, dead, failed)
	return truncateByBytes(header, rows, formatTail)
}

// renderResetRow is FormatResetResults' per-row branch.
func renderResetRow(r chatsession.ResetResult) (resultRow, formatRowBucket) {
	if r.Error != nil {
		return resultRow{
			text: fmt.Sprintf("  ✗ %s @ %s — bridge reset: %v",
				r.Agent, r.Cwd, r.Error),
			priority: bucketFailure,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketFailure
	}
	switch r.Action {
	case "in-place-reset":
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s — reset in-place", r.Agent, r.Cwd),
			priority: bucketSuccess,
			agent:    r.Agent,
			cwd:      r.Cwd,
		}, bucketSuccess
	case "marked-fresh":
		return resultRow{
			text:     fmt.Sprintf("  ✓ %s @ %s — already exited, marked fresh", r.Agent, r.Cwd),
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

// buildResetHeader mirrors the /close header but with /new-specific
// wording. The two commands share the (success / failure / skipped)
// tuple but differ in label vocabulary.
func buildResetHeader(running, dead, failed int) string {
	if failed == 0 && dead == 0 {
		return fmt.Sprintf("Reset %d session(s):", running)
	}
	if running == 0 && dead > 0 && failed == 0 {
		return fmt.Sprintf("Marked %d session(s) fresh for next spawn:", dead)
	}
	parts := make([]string, 0, 3)
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d reset in-place", running))
	}
	if dead > 0 {
		parts = append(parts, fmt.Sprintf("%d marked fresh", dead))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return "Reset " + fmt.Sprintf("%d session(s), %s:", running+dead, strings.Join(parts, ", "))
}