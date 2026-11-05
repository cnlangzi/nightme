package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file covers the resume half of the review fan-out
// (internal/agent/review_continue.go): the sessions are reopened with
// their recorded session id, the given text reaches them, their
// replies merge in the review group format, and their events fold
// into ONE outer lifecycle.

func continueJobs() []ReviewJob {
	return []ReviewJob{
		{Pattern: "**/*.go", Files: []string{"a.go", "b.go"}, SessionID: "sess-go"},
		{Pattern: "**/*.ts", Files: []string{"c.ts"}, SessionID: "sess-ts"},
	}
}

// TestContinueReviewJobs_ResumesAndMerges: resuming two recorded
// sessions sends the given text to both, reopens each with its own
// recorded session id in the given workspace, and merges the replies
// in the review group format — one section per group.
func TestContinueReviewJobs_ResumesAndMerges(t *testing.T) {
	jobs := continueJobs()

	var mu sync.Mutex
	var starts []StartConfig
	var texts []string

	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(cfg StartConfig) *sessionFakeDriver {
			mu.Lock()
			starts = append(starts, cfg)
			mu.Unlock()
			return newSessionFakeDriver(func(blocks []ContentBlock) []AgentEvent {
				mu.Lock()
				if len(blocks) > 0 {
					texts = append(texts, blocks[0].Text)
				}
				mu.Unlock()
				return []AgentEvent{
					sessionReadyEvent(cfg.SessionID, "model"),
					{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "continued findings"}},
				}
			})
		},
	}

	merged, err := ContinueReviewJobs(context.Background(), fs,
		StartConfig{Workspace: "/recorded/ws"}, jobs, "continue")
	if err != nil {
		t.Fatalf("ContinueReviewJobs: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != len(jobs) {
		t.Fatalf("Start called %d times, want %d (one session per group)", len(starts), len(jobs))
	}
	for _, cfg := range starts {
		if cfg.Workspace != "/recorded/ws" {
			t.Errorf("Start received Workspace %q, want the recorded workspace", cfg.Workspace)
		}
	}
	gotSessions := map[string]bool{}
	for _, cfg := range starts {
		gotSessions[cfg.SessionID] = true
	}
	for _, j := range jobs {
		if !gotSessions[j.SessionID] {
			t.Errorf("session %q was not resumed (started: %v)", j.SessionID, gotSessions)
		}
	}
	for _, text := range texts {
		if text != "continue" {
			t.Errorf("resumed session received %q, want %q", text, "continue")
		}
	}

	// One group section per resumed group, in the review format.
	for _, j := range jobs {
		want := "### Group: pattern " + j.Pattern + " — files: " + strings.Join(j.Files, ", ")
		if !strings.Contains(merged.Text, want) {
			t.Errorf("merged text missing %q; got:\n%s", want, merged.Text)
		}
	}
}

// TestContinueReviewJobs_SingleOuterLifecycle: the resumed sessions'
// events flow through the same aggregator a review's groups use, so
// the chat sees ONE review lifecycle — a single Ready and a single
// Result — not one per resumed session.
func TestContinueReviewJobs_SingleOuterLifecycle(t *testing.T) {
	jobs := continueJobs()

	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(cfg StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				return []AgentEvent{
					sessionReadyEvent(cfg.SessionID, "model"),
					{Kind: EventAgentText, Text: "thinking"},
					{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "findings"}},
				}
			})
		},
	}

	var mu sync.Mutex
	var events []AgentEvent
	sink := func(ev AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	if _, err := ContinueReviewJobs(context.Background(), fs,
		StartConfig{Workspace: "/ws"}, jobs, "continue", WithEventSink(sink)); err != nil {
		t.Fatalf("ContinueReviewJobs: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var readies, results int
	for _, ev := range events {
		switch ev.Kind {
		case EventAgentReady:
			readies++
		case EventAgentResult:
			results++
		}
	}
	if readies != 1 {
		t.Errorf("sink saw %d Ready events, want 1 (a single outer lifecycle)", readies)
	}
	if results != 1 {
		t.Errorf("sink saw %d Result events, want 1 (a single outer lifecycle)", results)
	}
}

// TestContinueReviewJobs_FailedGroupIsMarkedInline: a resumed
// session that fails again does not sink the run — the surviving
// group's reply still reaches the merged text and the failed group
// is marked inline.
func TestContinueReviewJobs_FailedGroupIsMarkedInline(t *testing.T) {
	jobs := continueJobs()

	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(cfg StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				if cfg.SessionID == "sess-ts" {
					return []AgentEvent{
						sessionReadyEvent(cfg.SessionID, "model"),
						{Kind: EventAgentError, Err: errBoom},
					}
				}
				return []AgentEvent{
					sessionReadyEvent(cfg.SessionID, "model"),
					{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "go findings"}},
				}
			})
		},
	}

	merged, err := ContinueReviewJobs(context.Background(), fs,
		StartConfig{Workspace: "/ws"}, jobs, "continue")
	if err != nil {
		t.Fatalf("a partial failure should not fail the whole continuation; got: %v", err)
	}
	if !strings.Contains(merged.Text, "go findings") {
		t.Errorf("surviving group lost its findings; got:\n%s", merged.Text)
	}
	if !strings.Contains(merged.Text, "### Group: pattern **/*.ts — failed:") {
		t.Errorf("failed group not marked inline; got:\n%s", merged.Text)
	}
}

// TestContinueReviewJobs_CapsConcurrency: the resume fan-out obeys
// the same concurrency cap as a review — the same core caps both.
func TestContinueReviewJobs_CapsConcurrency(t *testing.T) {
	const groups = 6
	jobs := make([]ReviewJob, 0, groups)
	for i := range groups {
		jobs = append(jobs, ReviewJob{
			Pattern:   fmt.Sprintf("**/*.g%d", i),
			SessionID: fmt.Sprintf("sess-%d", i),
		})
	}

	var mu sync.Mutex
	var live, peak int

	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(cfg StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				mu.Lock()
				live++
				if live > peak {
					peak = live
				}
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				mu.Lock()
				live--
				mu.Unlock()
				return []AgentEvent{
					sessionReadyEvent(cfg.SessionID, "model"),
					{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "ok"}},
				}
			})
		},
	}

	if _, err := ContinueReviewJobs(context.Background(), fs,
		StartConfig{Workspace: "/ws"}, jobs, "continue"); err != nil {
		t.Fatalf("ContinueReviewJobs: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > maxConcurrentReviewJobs {
		t.Errorf("peak concurrency %d exceeds the cap %d", peak, maxConcurrentReviewJobs)
	}
	if peak < 2 {
		t.Errorf("the resume fan-out did not run concurrently (peak %d)", peak)
	}
}

// TestContinueReviewJobs_ReportsPerGroupOutcomes: the observer sees
// one report per resumed group — the group's pattern, its files, the
// session id its session ran under, and the error that ended it.
func TestContinueReviewJobs_ReportsPerGroupOutcomes(t *testing.T) {
	jobs := continueJobs()

	fs := &sessionFakeStarter{
		name: "test-agent",
		newDriver: func(cfg StartConfig) *sessionFakeDriver {
			return newSessionFakeDriver(func([]ContentBlock) []AgentEvent {
				if cfg.SessionID == "sess-ts" {
					return []AgentEvent{
						sessionReadyEvent(cfg.SessionID, "model"),
						{Kind: EventAgentError, Err: errBoom},
					}
				}
				return []AgentEvent{
					sessionReadyEvent(cfg.SessionID, "model"),
					{Kind: EventAgentResult, Result: &AgentResultEvent{Text: "go findings"}},
				}
			})
		},
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

	if _, err := ContinueReviewJobs(context.Background(), fs,
		StartConfig{Workspace: "/ws"}, jobs, "continue", WithReviewJobObserver(observer)); err != nil {
		t.Fatalf("ContinueReviewJobs: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != len(jobs) {
		t.Fatalf("observer saw %d reports, want %d (one per group)", len(reports), len(jobs))
	}
	byPattern := map[string]int{}
	for i, r := range reports {
		byPattern[r.Pattern] = i
	}
	for _, j := range jobs {
		i, ok := byPattern[j.Pattern]
		if !ok {
			t.Errorf("no report for pattern %q; got %+v", j.Pattern, reports)
			continue
		}
		if len(reports[i].Files) != len(j.Files) {
			t.Errorf("report for %q carries files %v, want %v", j.Pattern, reports[i].Files, j.Files)
		}
	}
	// The failed group reports its error AND its session id — the
	// session id is what makes the group recoverable.
	tsIdx := byPattern["**/*.ts"]
	if tsIdx >= 0 {
		if errs[tsIdx] == nil {
			t.Error("the failed group reported no error")
		}
		if reports[tsIdx].SessionID != "sess-ts" {
			t.Errorf("the failed group reported session id %q, want %q", reports[tsIdx].SessionID, "sess-ts")
		}
	}
}
