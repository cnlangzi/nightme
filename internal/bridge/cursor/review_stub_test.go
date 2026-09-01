// review_stub_test.go — end-to-end tests of runCursorReview against
// the same stub `agent` binary used by print_stub_test.go
// (testdata/cursor-stub). Cross-platform: no build tag.
//
// What we lock here against the prototype native-review runner:
//
//   - happy path: Bugbot-style review text → RunResult with Text,
//     Subtype="completed", DurationMs > 0
//   - the /review-bugbot slash command is forwarded to the child
//     verbatim (so cursor-agent dispatches the skill, not a literal
//     prompt) — regression guard for the "-p" arg vs slash name
//   - exit 1 with stderr: stderr is appended to the error
//   - empty stdout: error includes "empty answer"
//   - empty workspace: rejected before spawning (cfg.Workspace required)
//   - per-call sink: receives EventAgentReady up-front, then
//     EventAgentResult + EventAgentDone on success (or
//     EventAgentError on failure)
package cursor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// runCursorReviewAndCollect is a thin helper that runs
// runCursorReview with a fresh sink and returns the captured events
// alongside the result. Centralized so the sink-shape assertions
// don't repeat across tests.
func runCursorReviewAndCollect(t *testing.T, s *Starter, cfg agent.StartConfig) (agent.RunResult, []agent.AgentEvent, error) {
	t.Helper()

	var (
		mu     sync.Mutex
		events []agent.AgentEvent
	)
	sink := func(ev agent.AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := runCursorReview(ctx, s, cfg, agent.WithEventSink(sink))
	return result, events, err
}

// TestRunCursorReview_HappyPath — Bugbot produced review text. The
// runner surfaces it as RunResult.Text verbatim, with the
// minimal-lifecycle Subtype / DurationMs the print-mode wire carries.
func TestRunCursorReview_HappyPath(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "happy", "BUGBOT REVIEW: nothing to flag here.")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	result, events, err := runCursorReviewAndCollect(t, s,
		agent.StartConfig{Workspace: ws})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result.Text != "BUGBOT REVIEW: nothing to flag here." {
		t.Errorf("Text = %q, want stub output verbatim", result.Text)
	}
	if result.Subtype != "completed" {
		t.Errorf("Subtype = %q, want completed", result.Subtype)
	}
	if result.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want > 0", result.DurationMs)
	}
	if len(events) != 3 {
		t.Fatalf("events len = %d, want 3 (Ready + Result + Done); events=%+v",
			len(events), events)
	}
	if events[0].Kind != agent.EventAgentReady {
		t.Errorf("events[0].Kind = %v, want EventAgentReady", events[0].Kind)
	}
	if events[0].Workspace != ws {
		t.Errorf("events[0].Workspace = %q, want %q", events[0].Workspace, ws)
	}
	if events[1].Kind != agent.EventAgentResult {
		t.Errorf("events[1].Kind = %v, want EventAgentResult", events[1].Kind)
	}
	if events[1].Result == nil || events[1].Result.Text != result.Text {
		t.Errorf("events[1].Result.Text = %v, want %q",
			events[1].Result, result.Text)
	}
	if events[2].Kind != agent.EventAgentDone {
		t.Errorf("events[2].Kind = %v, want EventAgentDone", events[2].Kind)
	}
	if events[2].Done == nil || events[2].Done.ExitCode != 0 {
		t.Errorf("events[2].Done = %+v, want ExitCode=0", events[2].Done)
	}
}

// TestRunCursorReview_ForwardsSlashCommand — the /review-bugbot
// slash command MUST reach the child process verbatim. If the argv
// builder ever drops the leading slash or the slash name itself,
// cursor-agent would treat the prompt as a literal model query
// (and Bugbot would never run) — silent regression. The stub's
// "echo" mode prints the value following -p, so we assert the
// printed text equals the slash command exactly.
func TestRunCursorReview_ForwardsSlashCommand(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "echo", "")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	result, _, err := runCursorReviewAndCollect(t, s,
		agent.StartConfig{Workspace: ws})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result.Text != reviewSlashCommand {
		t.Errorf("Text = %q, want %q (slash command MUST reach cursor-agent verbatim)",
			result.Text, reviewSlashCommand)
	}
}

// TestRunCursorReview_ExitOneWithStderr — Bugbot failed (e.g.
// "could not compute a branch-changes diff"). Runner surfaces
// stderr in the wrapped error and emits EventAgentError with a
// populated BridgeDiagnostic so upstream translate doesn't drop
// it.
func TestRunCursorReview_ExitOneWithStderr(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "exit_1", "could not compute a branch-changes diff")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	result, events, err := runCursorReviewAndCollect(t, s,
		agent.StartConfig{Workspace: ws})
	if err == nil {
		t.Fatal("err = nil, want non-nil on exit 1")
	}
	if !strings.Contains(err.Error(), "could not compute a branch-changes diff") {
		t.Errorf("err = %q, want contains stderr", err.Error())
	}
	if !strings.Contains(err.Error(), "cursor review:") {
		t.Errorf("err = %q, want contains 'cursor review:' prefix", err.Error())
	}
	// Sink: Ready + Error (terminal), no Result/Done.
	if len(events) < 2 {
		t.Fatalf("events len = %d, want >= 2 (Ready + Error)", len(events))
	}
	if events[0].Kind != agent.EventAgentReady {
		t.Errorf("events[0].Kind = %v, want EventAgentReady", events[0].Kind)
	}
	last := events[len(events)-1]
	if last.Kind != agent.EventAgentError {
		t.Errorf("last event Kind = %v, want EventAgentError", last.Kind)
	}
	if last.Diagnostic == nil {
		t.Error("Diagnostic = nil, want populated BridgeDiagnostic so translate doesn't drop the event")
	}
	if last.Diagnostic != nil && last.Diagnostic.AgentName != "cursor" {
		t.Errorf("Diagnostic.AgentName = %q, want cursor", last.Diagnostic.AgentName)
	}
	if result.Text != "" {
		t.Errorf("result.Text = %q, want empty on error", result.Text)
	}
}

// TestRunCursorReview_EmptyAnswer — Bugbot exited 0 with no text.
// Runner surfaces "cursor review: empty answer" so the chat channel
// doesn't silently render an empty review card.
func TestRunCursorReview_EmptyAnswer(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "empty", "")

	s := newStubStarter(t, bin)
	ws := t.TempDir()

	_, events, err := runCursorReviewAndCollect(t, s,
		agent.StartConfig{Workspace: ws})
	if err == nil {
		t.Fatal("err = nil, want non-nil on empty answer")
	}
	if !strings.Contains(err.Error(), "empty answer") {
		t.Errorf("err = %q, want contains 'empty answer'", err.Error())
	}
	// Sink: Ready + Error terminal.
	if len(events) < 2 {
		t.Fatalf("events len = %d, want >= 2", len(events))
	}
	if events[len(events)-1].Kind != agent.EventAgentError {
		t.Errorf("last event Kind = %v, want EventAgentError",
			events[len(events)-1].Kind)
	}
}

// TestRunCursorReview_MissingWorkspace — cfg.Workspace is required
// (Bugbot picks the repo from cwd). Reject before spawning.
func TestRunCursorReview_MissingWorkspace(t *testing.T) {
	bin := requireStubCursor(t)
	withStubEnv(t, "happy", "READY")

	s := newStubStarter(t, bin)

	_, err := runCursorReview(context.Background(), s,
		agent.StartConfig{Workspace: ""})
	if err == nil {
		t.Fatal("err = nil, want non-nil on empty workspace")
	}
	if !strings.Contains(err.Error(), "workspace is required") {
		t.Errorf("err = %q, want contains 'workspace is required'", err.Error())
	}
}
