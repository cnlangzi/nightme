// /new reply rendering — produces the IM-friendly plain-text
// summary the handler returns via command.Reply.
//
// The byte-cap, sort, and tail mechanics live in
// internal/command/format (shared with /close). This file owns
// only the /new-specific wording: row rendering and header
// building for chatsession.ResetResult.
//
// Output templates (selected by the (running, dead, failed) tuple):
//   - empty:               "No agent session in current workspace to reset..."
//     (handled in cmd.go; FormatResetResults returns a header)
//   - all reset:           "Reset N session(s):\n  ✓ <name> @ <cwd> — reset in-place\n..."
//   - all stale:           "Marked N session(s) fresh for next spawn:\n  ✓ ... already exited ..."
//   - mixed:               "Reset N session(s), M reset in-place, ..."
//
// Errors surface per-row (never swallowed). Output capped to 4 KB
// total bytes (Feishu single-message limit) + "...and N more"
// tail. Rows sorted by typed priority (success → failure →
// skipped), then by (Agent, Cwd) for stable display.
//
// See docs/feat/F-43-close-new-graceful-and-reset.md §6.2.
package newcmd

import (
	"fmt"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command/format"
)

// FormatResetResults produces a human-readable summary of /new's
// per-entry outcomes. Companion to close.FormatResults; same
// plain-text shape, same byte-based cap, same typed-priority
// sort — all delegated to internal/command/format.
func FormatResetResults(results []chatsession.ResetResult) string {
	if len(results) == 0 {
		return "Reset 0 sessions."
	}
	return format.FormatTable(results, renderResetRow, buildResetHeader)
}

// renderResetRow is FormatResetResults' per-row branch.
func renderResetRow(r chatsession.ResetResult) format.RenderedRow {
	if r.Error != nil {
		return format.RenderedRow{
			Text: fmt.Sprintf("  ✗ %s @ %s — bridge reset: %v",
				r.Agent, r.Cwd, r.Error),
			Bucket: format.BucketFailure,
			Agent:  r.Agent,
			Cwd:    r.Cwd,
		}
	}
	switch r.Action {
	case "in-place-reset":
		return format.RenderedRow{
			Text:   fmt.Sprintf("  ✓ %s @ %s — reset in-place", r.Agent, r.Cwd),
			Bucket: format.BucketSuccess,
			Agent:  r.Agent,
			Cwd:    r.Cwd,
		}
	case "marked-fresh":
		return format.RenderedRow{
			Text:   fmt.Sprintf("  ✓ %s @ %s — already exited, marked fresh", r.Agent, r.Cwd),
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
	return "Reset " + fmt.Sprintf("%d session(s), %s:", running+dead, format.JoinCounts(parts))
}