package agent

// The watchdog inside the review fan-out.
//
// A group whose subagent wedges must be closed on its own, while its
// siblings keep running and their findings still reach the caller.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// timeoutFakeStarter starts one session per group. The group whose
// prompt carries silentPattern gets a session that never says
// anything; every other group gets a session that answers.
type timeoutFakeStarter struct {
	name          string
	silentPattern string

	mu       sync.Mutex
	closed   []string
	sessions int
}

func (s *timeoutFakeStarter) Info() Info {
	return NewInfo(s.name, ModeJSONIO, "fake-cmd", nil, nil)
}
func (s *timeoutFakeStarter) Detect() error { return nil }
func (s *timeoutFakeStarter) Init(string)   {}

func (s *timeoutFakeStarter) Start(_ context.Context, _ StartConfig) (*Agent, error) {
	s.mu.Lock()
	s.sessions++
	s.mu.Unlock()
	events := make(chan AgentEvent, 8)
	d := &timeoutFakeDriver{starter: s, events: events}
	return NewAgent(s.Info(), 0, events, d), nil
}

func (s *timeoutFakeStarter) RunOnce(context.Context, StartConfig, []ContentBlock, ...RunOnceOption) (RunResult, error) {
	return RunResult{}, errors.New("timeoutFakeStarter: RunOnce must not be used on the session path")
}

func (s *timeoutFakeStarter) Review(ctx context.Context, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
	return RunSession(ctx, s, cfg, []ContentBlock{{Type: ContentText, Text: BuiltinPrompt}}, opts...)
}

// closedSessions returns the prompts of the sessions that were closed.
func (s *timeoutFakeStarter) closedSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.closed...)
}

// timeoutFakeDriver answers the groups whose prompt carries a pattern
// and stays silent for the others.
type timeoutFakeDriver struct {
	starter *timeoutFakeStarter
	events  chan AgentEvent

	mu     sync.Mutex
	prompt string
	closed atomic.Bool
}

func (d *timeoutFakeDriver) SendBlocks(_ context.Context, blocks []ContentBlock) error {
	prompt := ""
	if len(blocks) > 0 {
		prompt = blocks[0].Text
	}
	d.mu.Lock()
	d.prompt = prompt
	d.mu.Unlock()
	if strings.Contains(prompt, d.starter.silentPattern) {
		// The silent group: no event, ever. The drain waits for the
		// group's context.
		return nil
	}
	go func() {
		d.events <- sessionReadyEvent("sess-ok", "model")
		d.events <- AgentEvent{
			Kind:   EventAgentResult,
			Result: &AgentResultEvent{Text: reviewReply("the findings")},
		}
		close(d.events)
	}()
	return nil
}

func (d *timeoutFakeDriver) SendPermission(string) error { return nil }
func (d *timeoutFakeDriver) Reset(context.Context) error { return nil }

func (d *timeoutFakeDriver) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	d.mu.Lock()
	prompt := d.prompt
	d.mu.Unlock()
	d.starter.mu.Lock()
	d.starter.closed = append(d.starter.closed, prompt)
	d.starter.mu.Unlock()
	return nil
}

func (d *timeoutFakeDriver) Stop(context.Context) error { return ErrNotSupported }
func (d *timeoutFakeDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// timeoutGroups are the two groups of the fan-out: one answers, one
// never says a word.
func timeoutGroups() []reviewGroup {
	return []reviewGroup{
		{Pattern: "**/*.go", Files: []string{"a.go"}},
		{Pattern: "**/*.ts", Files: []string{"c.ts"}},
	}
}

// timeoutGroupRunner runs one group with its own pattern as the
// prompt — the fake starter keys its behavior off the prompt.
func timeoutGroupRunner(s Starter, groups []reviewGroup) reviewGroupRunner {
	return func(ctx context.Context, groupIdx int, cfg StartConfig, runOpts []RunOnceOption) (RunResult, error) {
		return RunSession(ctx, s, cfg, []ContentBlock{{
			Type: ContentText,
			Text: groups[groupIdx].Pattern,
		}}, runOpts...)
	}
}

// observedGroup is one group's outcome as the observer saw it.
type observedGroup struct {
	job ReviewJob
	err error
}

// collectGroups runs the fan-out and returns what the observer saw.
func collectGroups(t *testing.T, s Starter, groups []reviewGroup, opts ...RunOnceOption) ([]observedGroup, RunResult, error) {
	t.Helper()
	var mu sync.Mutex
	var seen []observedGroup
	observer := func(job ReviewJob, err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, observedGroup{job: job, err: err})
	}
	merged, err := runReviewGroups(context.Background(), s, StartConfig{Workspace: "/nonexistent-repo"},
		groups, append(opts, WithReviewJobObserver(observer)), timeoutGroupRunner(s, groups))
	mu.Lock()
	defer mu.Unlock()
	return seen, merged, err
}

// TestRunReviewGroups_ClosesTheSilentGroup: a group whose subagent
// says nothing is closed on its own, and its failure is a timeout —
// not a cancellation the caller has to interpret.
func TestRunReviewGroups_ClosesTheSilentGroup(t *testing.T) {
	groups := timeoutGroups()
	fs := &timeoutFakeStarter{name: "timeout-agent", silentPattern: "**/*.ts"}

	seen, merged, err := collectGroups(t, fs, groups, WithReviewGroupTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatalf("the healthy group should keep the run alive: %v", err)
	}

	for _, o := range seen {
		switch o.job.Pattern {
		case "**/*.ts":
			var timeout *GroupTimeoutError
			if !errors.As(o.err, &timeout) {
				t.Fatalf("the silent group failed with %v, want a *GroupTimeoutError", o.err)
			}
			if timeout.Timeout != 100*time.Millisecond {
				t.Errorf("the timeout = %s, want 100ms", timeout.Timeout)
			}
			if timeout.Group != "**/*.ts" {
				t.Errorf("the timeout names the group %q, want the group's name", timeout.Group)
			}
		case "**/*.go":
			if o.err != nil {
				t.Errorf("the healthy group failed: %v", o.err)
			}
		default:
			t.Errorf("the observer saw an unexpected group: %+v", o.job)
		}
	}

	// The healthy group's findings still reach the caller; the silent
	// group is marked inline with the timeout.
	if !strings.Contains(merged.Text, "the findings") {
		t.Errorf("the healthy group's findings are missing:\n%s", merged.Text)
	}
	if !strings.Contains(merged.Text, "failed: review group") {
		t.Errorf("the timed-out group is not marked inline:\n%s", merged.Text)
	}

	// The close chain ran: the silent group's session was closed.
	closed := fs.closedSessions()
	found := false
	for _, prompt := range closed {
		if strings.Contains(prompt, "**/*.ts") {
			found = true
		}
	}
	if !found {
		t.Errorf("the silent group's session was not closed; closed: %v", closed)
	}
}

// TestRunReviewGroups_AGroupThatBeatsDoesNotTimeOut: a group that
// produces events inside the window is not closed.
func TestRunReviewGroups_AGroupThatBeatsDoesNotTimeOut(t *testing.T) {
	groups := timeoutGroups()
	fs := &timeoutFakeStarter{name: "beat-agent", silentPattern: "**/*.nonexistent"}

	seen, merged, err := collectGroups(t, fs, groups, WithReviewGroupTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("the fan-out failed: %v", err)
	}
	for _, o := range seen {
		if o.err != nil {
			t.Errorf("the group %q failed: %v", o.job.Pattern, o.err)
		}
	}
	if !strings.Contains(merged.Text, "the findings") {
		t.Errorf("the findings are missing:\n%s", merged.Text)
	}
}

// TestRunReviewGroups_ZeroTimeoutDisablesTheWatchdog: a run with the
// timeout disabled waits for the group — the group's failure is the
// run's cancellation, not a timeout.
func TestRunReviewGroups_ZeroTimeoutDisablesTheWatchdog(t *testing.T) {
	groups := timeoutGroups()
	fs := &timeoutFakeStarter{name: "no-timeout-agent", silentPattern: "**/*.ts"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	var mu sync.Mutex
	var seen []observedGroup
	observer := func(job ReviewJob, err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, observedGroup{job: job, err: err})
	}
	_, err := runReviewGroups(ctx, fs, StartConfig{Workspace: "/nonexistent-repo"}, groups,
		[]RunOnceOption{
			WithReviewGroupTimeout(0),
			WithReviewJobObserver(observer),
		}, timeoutGroupRunner(fs, groups))
	if err != nil {
		t.Fatalf("the healthy group should keep the run alive: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	silent := 0
	for _, o := range seen {
		if o.job.Pattern != "**/*.ts" {
			continue
		}
		silent++
		if o.err == nil {
			t.Error("the silent group did not fail")
		}
		if errors.Is(o.err, ErrGroupTimeout) {
			t.Errorf("the timeout fired although it was disabled: %v", o.err)
		}
	}
	if silent != 1 {
		t.Errorf("the observer saw %d silent groups, want 1", silent)
	}
}
