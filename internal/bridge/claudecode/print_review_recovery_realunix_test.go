//go:build !windows

package claudecode

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestParsePrintStream_RealRecovery_AskUserQuestion is a data-driven
// regression test: it replays a real Claude Code /code-review turn
// captured on 2026-08-27 where the plugin ran the multi-agent
// pipeline successfully, then closed the turn with
// `Want me to apply the suggested patch (rename package to `calc`,
// add doc comments, fix gofmt, add tests, and `go mod init`)?`
// instead of writing the review into the terminal result event.
//
// Without the AskUserQuestion swap (see parsePrintStream +
// isFollowupQuestion in print.go), the dispatcher's
// `FormatReviewMessage(workspace, "claude", text)` wrapper would
// have wrapped the 168-char follow-up question instead of the
// 3.7 KiB review. The fix recovers the review from the largest
// assistant text block observed in the assistant event stream.
//
// The fixture lives at internal/bridge/claudecode/testdata/
// code_review_askuserquestion.jsonl (a stream-json transcript).
// The test skips cleanly when the fixture is missing so CI on
// fresh checkouts without the bundled testdata still passes.
//
// Captured against claude-code 2.1.220 with the official
// /code-review marketplace plugin, run on a small fixture repo
// with one commit (`feat: add calc`) on top of `main`.
func TestParsePrintStream_RealRecovery_AskUserQuestion(t *testing.T) {
	const fixture = "testdata/code_review_askuserquestion.jsonl"
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Skipf("fixture %s not present: %v", fixture, err)
	}
	got, err := parsePrintStream(context.Background(), strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("parsePrintStream: %v", err)
	}

	// Sanity: the fixture's terminal result event was the
	// "Want me to apply…?" follow-up. If Claude Code's plugin
	// ever changes shape (e.g. embeds the review directly into
	// the result event), this assertion fails and tells the
	// maintainer the heuristic + fixture need updating.
	if !strings.HasPrefix(got.Text, "# Code review") && !strings.HasPrefix(got.Text, "## Code") {
		t.Errorf("recovery failed: result does not start with a review header (got first 80 chars: %q)",
			got.Text[:min(80, len(got.Text))])
	}
	if len(got.Text) < 500 {
		t.Errorf("recovery failed: result too short (%d chars), review not recovered", len(got.Text))
	}
	t.Logf("recovered %d chars of review from assistant stream (assistant_text %d chars)",
		len(got.Text), len(got.AssistantText))
}
