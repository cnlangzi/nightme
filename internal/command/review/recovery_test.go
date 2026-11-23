package review

// The recovery of a review group that did not finish.
//
// A group that reached the agent is resumed; a group that never
// reached the agent is re-run with the prompt it was asked to run.
// The verdict says which of the two the user is looking at, and the
// plan line of the continuation says what it does with each group.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// TestSpec_UsageDescribesTheContinuation: the usage text says what
// the continuation does with a group that never started — the
// continuation is not a resume only.
func TestSpec_UsageDescribesTheContinuation(t *testing.T) {
	usage := NewFactory().Spec().Usage
	if strings.Contains(usage, `sends "continue" to them`) {
		t.Errorf("the usage text still describes the continuation as a resume only:\n%s", usage)
	}
	if !strings.Contains(usage, "re-run") {
		t.Errorf("the usage text does not say a group that never started is re-run:\n%s", usage)
	}
}

// TestHandle_ReviewKeepsTheGroupPrompt: the record of the last
// review holds the prompt each group ran — a group that never reached
// the agent is re-run with it.
func TestHandle_ReviewKeepsTheGroupPrompt(t *testing.T) {
	agents := newTestAgents(t)
	starter := &planReviewStarter{
		name: "review-prompt-agent",
		groups: []agent.ReviewPlanGroup{
			{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
		},
		prompt: "the group's prompt",
		texts:  []string{"the findings"},
	}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-prompt-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	dispatchContinue(t, f, cs, chatID, "review")
	em.waitForOutReply(t, "review")
	// The record is written when the run ends.
	waitUntilIdle(t, f, chatID)

	rec, ok := f.lookup(chatID)
	if !ok {
		t.Fatal("the review kept no record")
	}
	if len(rec.outcomes) != 1 {
		t.Fatalf("the record carries %d outcomes, want 1", len(rec.outcomes))
	}
	if rec.outcomes[0].prompt != "the group's prompt" {
		t.Errorf("the record's outcome carries no prompt: %+v", rec.outcomes[0])
	}
}

// TestHandle_ContinueRerunsTheGroupThatNeverStarted: a group that
// never reached the agent has no conversation to continue — the
// continuation starts a fresh session and sends it the stored prompt.
func TestHandle_ContinueRerunsTheGroupThatNeverStarted(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewSessionStarter{name: "review-rerun-agent", reply: "the re-run findings"}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-rerun-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)
	enableThink(t, cs)

	f := NewFactory()
	f.record(chatID, starter.name, "/recorded/ws", reviewRun{outcomes: []reviewOutcome{
		{pattern: "**/*.go", files: []string{"a.go"}},
		{
			pattern: "**/*.ts",
			files:   []string{"c.ts"},
			err:     errors.New("spawn failed"),
			prompt:  "the stored prompt",
		},
	}})

	dispatchContinue(t, f, cs, chatID, "review", "-c")
	em.waitForOutThinking(t, "the re-run findings")

	cfgs := starter.startedConfigs()
	if len(cfgs) != 1 {
		t.Fatalf("started %d sessions, want 1 (the unfinished group only)", len(cfgs))
	}
	if cfgs[0].SessionID != "" {
		t.Errorf("the re-run carried session id %q, want none (it is a fresh session)", cfgs[0].SessionID)
	}
	if cfgs[0].Workspace != "/recorded/ws" {
		t.Errorf("the re-run workspace = %q, want the recorded workspace", cfgs[0].Workspace)
	}

	blocks := starter.receivedBlocks()
	if len(blocks) != 1 {
		t.Fatalf("the session received %d blocks, want 1", len(blocks))
	}
	if blocks[0].Text != "the stored prompt" {
		t.Errorf("the session received %q, want the stored prompt", blocks[0].Text)
	}
}

// TestHandle_ContinueRecoversAGroupOnlyOnce: a group whose recovery
// fails again is spent — a second continuation does not run it a
// third time.
func TestHandle_ContinueRecoversAGroupOnlyOnce(t *testing.T) {
	agents := newTestAgents(t)
	starter := &reviewSessionStarter{name: "review-once-agent", fail: true}
	agents.registry.Register(starter)

	em := newRecordingEmitter()
	const chatID = "review-once-chat"
	cs, _ := newContinueChatSession(t, chatID, starter.name, t.TempDir(), em)

	f := NewFactory()
	f.record(chatID, starter.name, "/recorded/ws", reviewRun{outcomes: []reviewOutcome{
		{pattern: "**/*.ts", files: []string{"c.ts"}, sessionID: "sess-ts", err: errors.New("boom")},
	}})

	dispatchContinue(t, f, cs, chatID, "review", "-c")
	em.waitForOutReply(t, "already retried")
	started := len(starter.startedConfigs())

	out := dispatchContinue(t, f, cs, chatID, "review", "-c")

	// The review is over — it was delivered with the group that died
	// named in it — so there is nothing to continue.
	if !strings.Contains(out.Reply, "the last review completed") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	if got := len(starter.startedConfigs()); got != started {
		t.Errorf("the spent group ran again: %d sessions, want %d", got, started)
	}
}

// TestReviewVerdict_NamesTheFailedGroupsState: the verdict says
// which failed group can still be finished, and offers the
// continuation only when one of them can.
func TestReviewVerdict_NamesTheFailedGroupsState(t *testing.T) {
	cases := []struct {
		name     string
		outcomes []reviewOutcome
		want     []string
		notWant  []string
	}{
		{
			name: "the group reached the agent",
			outcomes: []reviewOutcome{
				{pattern: "**/*.go", source: "system", sessionID: "sess-go", err: errors.New("boom")},
			},
			want: []string{
				"- `system / **/*.go`",
				"💡 hint: run `/review --continue` to continue the failed groups",
			},
			notWant: []string{"(never started)", "(already retried)"},
		},
		{
			name: "the group never reached the agent",
			outcomes: []reviewOutcome{
				{pattern: "**/*.go", source: "system", prompt: "the prompt", err: errors.New("boom")},
			},
			want: []string{
				"- `system / **/*.go` (never started)",
				"💡 hint: run `/review --continue` to continue the failed groups",
			},
			notWant: []string{"(already retried)"},
		},
		{
			name: "the group's recovery attempt is spent",
			outcomes: []reviewOutcome{
				{pattern: "**/*.go", source: "system", sessionID: "sess-go", spent: true, err: errors.New("boom")},
			},
			want:    []string{"- `system / **/*.go` (already retried)"},
			notWant: []string{"/review --continue", "💡 hint:"},
		},
		{
			name: "an actionable group beside a spent one",
			outcomes: []reviewOutcome{
				{pattern: "**/*.go", source: "system", sessionID: "sess-go", spent: true, err: errors.New("boom")},
				{pattern: "**/*.ts", source: "system", prompt: "the prompt", err: errors.New("boom")},
			},
			want: []string{
				"- `system / **/*.go` (already retried)",
				"- `system / **/*.ts` (never started)",
				"💡 hint: run `/review --continue` to continue the failed groups",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newVerdict(testRecord(tc.outcomes, nil, nil), true).line()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the verdict does not carry %q: %s", want, got)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("the verdict carries %q: %s", notWant, got)
				}
			}
		})
	}
}

// TestRenderPlanLine_MarksTheContinuedGroups: the plan line of a
// continuation says what it does with each group — a plain review
// carries no marker.
func TestRenderPlanLine_MarksTheContinuedGroups(t *testing.T) {
	groups := []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}, Mode: agent.ReviewModeResume},
		{Pattern: agent.PatternSimplify, Label: "Simplify review lens", Files: []string{"a.go"}, Mode: agent.ReviewModeRerun},
	}
	got := renderPlanLine("codex", groups)
	for _, want := range []string{
		"1. `**/*.go` — 1 file (resumed)",
		"2. Simplify review lens — 1 file (re-run)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the plan line does not carry %q:\n%s", want, got)
		}
	}

	plain := renderPlanLine("codex", []agent.ReviewPlanGroup{
		{Pattern: "**/*.go", Label: "**/*.go", Files: []string{"a.go"}},
	})
	if strings.Contains(plain, "resumed") || strings.Contains(plain, "re-run") {
		t.Errorf("a plain review's plan line carries a marker:\n%s", plain)
	}
}

// TestLogFailedGroups_RecordsTheFailure: every failed group is
// logged as it happens — the group's name, the agent, the error and
// whether the group reached the agent.
func TestLogFailedGroups_RecordsTheFailure(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	var seen []agent.ReviewJob
	logFailedGroups("/review", "codex", func(job agent.ReviewJob, err error) {
		seen = append(seen, job)
	})(agent.ReviewJob{
		Pattern:   "**/*.go",
		Source:    "system",
		SessionID: "sess-go",
	}, errors.New("boom"))

	if len(seen) != 1 {
		t.Fatalf("the observer saw %d reports, want 1", len(seen))
	}

	var record map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the log line is not JSON: %v (%s)", err, line)
		}
		if record["msg"] == "/review: group failed" {
			break
		}
		record = nil
	}
	if record == nil {
		t.Fatalf("no failure was logged:\n%s", logs.String())
	}
	if record["level"] != "WARN" {
		t.Errorf("the failure was logged at %v, want WARN", record["level"])
	}
	if record["agent"] != "codex" {
		t.Errorf("the log names agent %v, want codex", record["agent"])
	}
	if record["group"] != "system / **/*.go" {
		t.Errorf("the log names group %v, want the group's name", record["group"])
	}
	if record["reached_agent"] != true {
		t.Errorf("the log says reached_agent=%v, want true", record["reached_agent"])
	}
	if record["err"] != "boom" {
		t.Errorf("the log carries err=%v, want the failure", record["err"])
	}
}

// TestReviewContinueOutcome_SpeaksOfContinuedGroups: the outcome of a
// continuation speaks of the groups it continued.
func TestReviewContinueOutcome_SpeaksOfContinuedGroups(t *testing.T) {
	set := newTestAgents(t)
	set.registry.Register(&resumeStarter{name: "reviewer", failCount: 1})

	cs, em := newOutcomeChat(t, "review-continue-summary", "reviewer")
	f := NewFactory()
	f.record(cs.ChatID, "reviewer", cs.SelectedCwd(), reviewRun{outcomes: []reviewOutcome{
		{pattern: "group-1", files: []string{"a.go"}, sessionID: "sess-1", err: errors.New("boom")},
		{pattern: "group-2", files: []string{"b.go"}, sessionID: "sess-2", err: errors.New("boom")},
	}})

	hold := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), hold)
	out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    cs.ChatID,
		MessageID: "msg-1",
		Text:      "/review -c",
		Args:      []string{"review", "-c"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out == nil {
		t.Fatal("Handle returned nil SlashOutput")
	}
	hold.Release()
	got := waitOutcome(t, em, 5*time.Second)
	assertOutcome(t, got, http.StatusInternalServerError, map[string]string{
		"continued_groups": "2",
		"unfinished":       "1",
		"delivered":        "true",
	})
	if got.StatusText != "1 of 2 groups unfinished" {
		t.Errorf("the summary = %q, want %q", got.StatusText, "1 of 2 groups unfinished")
	}
}

// TestReviewVerdict_NamesTheUnrecoverableGroup: a group the
// continuation cannot finish says so — the offer must not read as if
// it would pick that group up.
func TestReviewVerdict_NamesTheUnrecoverableGroup(t *testing.T) {
	outcomes := []reviewOutcome{
		{pattern: "**/*.go", source: "system", err: errors.New("boom")},
		{pattern: "**/*.ts", source: "system", sessionID: "sess-ts", err: errors.New("boom")},
	}

	got := newVerdict(testRecord(outcomes, nil, nil), true).line()
	if !strings.Contains(got, "- `system / **/*.go` (not recoverable)") {
		t.Errorf("the verdict does not name the unrecoverable group: %s", got)
	}
	if !strings.Contains(got, "💡 hint: run `/review --continue` to continue the failed groups") {
		t.Errorf("the verdict does not offer the continuation for the resumable group: %s", got)
	}
}
