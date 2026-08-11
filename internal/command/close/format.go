// /close reply rendering — produces the IM-friendly plain-text
// summary the handler returns via command.Reply.
//
// The byte-cap, sort, and tail mechanics live in
// internal/command/format (shared with /new). This file owns
// only the close-specific wording: row rendering and header
// building.
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

	"github.com/cnlangzi/nightme/internal/command/format"
)

// FormatResults produces a human-readable summary of /close's
// per-entry outcomes. The output is suitable for channel.Send
// (plain text, Feishu-renderable).
func FormatResults(results []Result) string {
	if len(results) == 0 {
		return "No active agents to close."
	}
	return format.FormatTable(results, renderCloseRow, buildCloseHeader)
}

// renderCloseRow is FormatResults' per-row branch.
func renderCloseRow(r Result) format.RenderedRow {
	if r.Error != nil {
		return format.RenderedRow{
			Text: fmt.Sprintf("  ✗ %s @ %s — %s: %v",
				r.Agent, r.Cwd, format.HumanAction(r.Action), r.Error),
			Bucket: format.BucketFailure,
			Agent:  r.Agent,
			Cwd:    r.Cwd,
		}
	}
	switch r.Action {
	case "closed":
		return format.RenderedRow{
			Text:   fmt.Sprintf("  ✓ %s @ %s", r.Agent, r.Cwd),
			Bucket: format.BucketSuccess,
			Agent:  r.Agent,
			Cwd:    r.Cwd,
		}
	case "stale-cleared":
		return format.RenderedRow{
			Text:   fmt.Sprintf("  • %s @ %s — already exited", r.Agent, r.Cwd),
			Bucket: format.BucketSkipped,
			Agent:  r.Agent,
			Cwd:    r.Cwd,
		}
	default:
		return format.RenderedRow{
			Text:   fmt.Sprintf("  ✓ %s @ %s — %s", r.Agent, r.Cwd, r.Action),
			Bucket: format.BucketSuccess,
			Agent:  r.Agent,
			Cwd:    r.Cwd,
		}
	}
}

// buildCloseHeader mirrors the spec template wording for the
// top-of-reply summary line. Wording reflects the close-only-
// kill semantics: bridge processes are gone, but AgentSession
// entries (and their session IDs) stay in the pool.
func buildCloseHeader(closed, stale, failed int) string {
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
	return format.JoinCounts(parts) + ":"
}