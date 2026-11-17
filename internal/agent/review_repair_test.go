package agent

// The schema re-emit: the last tier of the parser.
//
// A group whose output the parser cannot read contributes nothing to
// the verdict's counts. The re-emit gives such a group one extra
// turn: its own session is reopened and asked to state the findings
// again, in the schema the parser reads.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// repairStarter is a fake starter whose sessions answer differently
// the first time and when they are resumed.
type repairStarter struct {
	name string
	// first is the answer of a group's own turn.
	first string
	// reemitted is the answer of the re-emit turn.
	reemitted string

	mu       sync.Mutex
	prompts  []string
	resumes  int
	sessions []string
}

func (s *repairStarter) Info() Info    { return NewInfo(s.name, ModeJSONIO, "fake-cmd", nil, nil) }
func (s *repairStarter) Detect() error { return nil }
func (s *repairStarter) Init(string)   {}

func (s *repairStarter) Start(_ context.Context, cfg StartConfig) (*Agent, error) {
	d := newSessionFakeDriver(func(blocks []ContentBlock) []AgentEvent {
		text := ""
		if len(blocks) > 0 {
			text = blocks[0].Text
		}
		s.mu.Lock()
		s.prompts = append(s.prompts, text)
		resumed := cfg.SessionID != ""
		if resumed {
			s.resumes++
		}
		s.mu.Unlock()
		answer := s.first
		if resumed {
			answer = s.reemitted
		}
		return []AgentEvent{
			sessionReadyEvent("sess-group", "model"),
			{Kind: EventAgentResult, Result: &AgentResultEvent{Text: answer}},
		}
	})
	return NewAgent(s.Info(), 0, d.events, d), nil
}

func (s *repairStarter) RunOnce(context.Context, StartConfig, []ContentBlock, ...RunOnceOption) (RunResult, error) {
	return RunResult{}, errors.New("RunOnce must not be used on the session path")
}

func (s *repairStarter) Review(context.Context, StartConfig, ...RunOnceOption) (RunResult, error) {
	return RunResult{}, errors.New("Review must not be used on the session path")
}

// promptCount returns how many prompts the sessions received.
func (s *repairStarter) promptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

// resumeCount returns how many sessions were resumed.
func (s *repairStarter) resumeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumes
}

// promptAt returns the prompt the n-th turn received.
func (s *repairStarter) promptAt(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= len(s.prompts) {
		return ""
	}
	return s.prompts[n]
}

// repairRun runs one review group through the fan-out and returns the
// report the observer saw.
func repairRun(t *testing.T, starter Starter) (ReviewJob, error, string) {
	t.Helper()
	pre := reviewContext{
		workspace:     "/nonexistent-repo",
		defaultBranch: "origin/main",
		mergeBase:     "abc123",
		ocrGroups:     []reviewGroup{{Pattern: "**/*.go", Files: []string{"a.go"}}},
	}
	var mu sync.Mutex
	var reports []ReviewJob
	var errs []error
	observer := func(job ReviewJob, err error) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, job)
		errs = append(errs, err)
	}
	merged, err := delegateReviewMultiJob(context.Background(), starter,
		StartConfig{Workspace: pre.workspace}, pre, pre.ocrGroups, WithReviewJobObserver(observer))
	if err != nil {
		t.Fatalf("delegateReviewMultiJob: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("the observer saw %d reports, want 1", len(reports))
	}
	return reports[0], errs[0], merged.Text
}

// TestReviewGroups_ReemitReadsTheFindings: a group whose output the
// parser cannot read is resumed once and sent the re-emit
// instruction — and the reply's findings reach the counts.
func TestReviewGroups_ReemitReadsTheFindings(t *testing.T) {
	starter := &repairStarter{
		name:      "test-agent",
		first:     "I looked at the diff and it seems fine to me.",
		reemitted: "- path: a.go\n- severity: high\n- content: the problem",
	}

	report, err, merged := repairRun(t, starter)

	if err != nil {
		t.Fatalf("the group reported an error: %v", err)
	}
	if !report.Report.Parsed {
		t.Fatalf("the re-emitted findings did not reach the report: %+v", report.Report)
	}
	if len(report.Report.Findings) != 1 {
		t.Fatalf("the report carries %d findings, want 1: %+v",
			len(report.Report.Findings), report.Report.Findings)
	}
	finding := report.Report.Findings[0]
	if finding.Path != "a.go" || finding.Severity != SeverityHigh {
		t.Errorf("the re-emitted finding = %+v, want a.go at high", finding)
	}
	if starter.resumeCount() != 1 {
		t.Errorf("the group was resumed %d times, want 1", starter.resumeCount())
	}
	if got := starter.promptAt(1); got != reviewReemitInstruction {
		t.Errorf("the re-emit prompt = %q, want the re-emit instruction", got)
	}
	// The merged document keeps the group's raw output — it is the
	// main agent's input and must carry what the agent said.
	if !strings.Contains(merged, "I looked at the diff") {
		t.Errorf("the merged text does not carry the group's raw output:\n%s", merged)
	}
}

// TestReviewGroups_ReemitStaysUnparsed: a group whose re-emit is
// unreadable too stays unparsed — and the run does not try a second
// time.
func TestReviewGroups_ReemitStaysUnparsed(t *testing.T) {
	starter := &repairStarter{
		name:      "test-agent",
		first:     "I looked at the diff and it seems fine to me.",
		reemitted: "still just prose",
	}

	report, err, _ := repairRun(t, starter)

	if err != nil {
		t.Fatalf("the group reported an error: %v", err)
	}
	if report.Report.Parsed {
		t.Errorf("an unreadable group was reported as parsed: %+v", report.Report)
	}
	if len(report.Report.Findings) != 0 {
		t.Errorf("an unreadable group produced findings: %+v", report.Report.Findings)
	}
	if starter.resumeCount() != 1 {
		t.Errorf("the group was resumed %d times, want 1 (a run re-emits at most once per group)",
			starter.resumeCount())
	}
}

// TestReviewGroups_NoReemitForAReadableOutput: a group whose output
// the parser reads is not resumed — the re-emit is a repair, not a
// second turn every group pays.
func TestReviewGroups_NoReemitForAReadableOutput(t *testing.T) {
	starter := &repairStarter{
		name:      "test-agent",
		first:     "- path: a.go\n- severity: high\n- content: the problem",
		reemitted: "- path: b.go\n- severity: low\n- content: another problem",
	}

	report, err, _ := repairRun(t, starter)

	if err != nil {
		t.Fatalf("the group reported an error: %v", err)
	}
	if len(report.Report.Findings) != 1 {
		t.Fatalf("the report carries %d findings, want 1: %+v",
			len(report.Report.Findings), report.Report.Findings)
	}
	if starter.resumeCount() != 0 {
		t.Errorf("a readable group was resumed %d times, want 0", starter.resumeCount())
	}
}
