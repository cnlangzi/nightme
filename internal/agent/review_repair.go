// review_repair.go — the schema re-emit: the last tier of the parser.
//
// A group that answered in a shape the parser cannot read contributes
// nothing to the verdict: its findings are real, but the counts and
// the severity histogram do not know about them. The re-emit gives
// such a group one extra turn: the group's own session is reopened
// and asked to state the findings again, in the schema the parser
// reads.
//
// The re-emit is a repair of a group that completed, not a retry of a
// group that failed: it does not consume the group's recovery attempt.

package agent

import (
	"context"
	"log/slog"
)

// reviewReemitInstruction asks the agent to state its findings again,
// in the schema the parser reads.
const reviewReemitInstruction = `Re-emit the findings you just reported, in this exact format — one block per finding, nothing else:

- path: <file path>
- start_line: <line number>
- severity: <critical|high|medium|low>
- category: <category>
- content: <what is wrong and why it matters>

If you found no problems, reply exactly: no findings`

// reemitReviewFindings reopens the group's own session and asks it to
// re-emit its findings in the schema the parser reads.
//
// The turn runs WITHOUT an event sink: the aggregator counts one
// terminal per job, and a second terminal for the same job would
// double-count it. The re-emit therefore does not appear in the
// thread — it feeds the counts only.
//
// It reports whether the re-emit produced a readable answer.
func reemitReviewFindings(ctx context.Context, s Starter, cfg StartConfig, sessionID string) (ReviewReport, bool) {
	cfg.SessionID = sessionID
	res, err := ResumeSession(ctx, s, cfg, []ContentBlock{{
		Type: ContentText,
		Text: reviewReemitInstruction,
	}})
	if err != nil {
		slog.Default().Warn("/review: the schema re-emit failed",
			"session_id", sessionID,
			"err", err,
		)
		return ReviewReport{}, false
	}
	report := ParseReviewFindings(res.Text)
	if !report.Parsed {
		slog.Default().Warn("/review: the schema re-emit is unreadable",
			"session_id", sessionID,
		)
		return ReviewReport{}, false
	}
	return report, true
}
