package review

// The structured outcome every /review exit path reports.
//
// A program that drives nightme reads the outcome, not the card —
// so the review has to say what happened, including how many of its
// groups finished. The group counts come from the observer the
// fan-out installs, never from the review text.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// groupReviewStarter is a fake starter that reports a fixed number
// of review groups through the observer the fan-out installs.
//
// The real fan-out calls the observer once per group; the fake does
// the same so the review's group counts are exercised without a git
// repository and a real bridge.
type groupReviewStarter struct {
	name string
	// groups is how many groups the run reports.
	groups int
	// failed is how many of them report an error.
	failed int
	// err is what the whole run fails with (ErrNoDiff,
	// ErrReviewNotSupported, …). Nil means the run succeeded.
	err error
}

func (s *groupReviewStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "", nil, nil)
}
func (s *groupReviewStarter) Detect() error { return nil }
func (s *groupReviewStarter) Init(string)   {}
func (s *groupReviewStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("groupReviewStarter: Start not implemented")
}
func (s *groupReviewStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("groupReviewStarter: RunOnce not implemented")
}

func (s *groupReviewStarter) Review(_ context.Context, _ agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if s.err != nil {
		return agent.RunResult{}, s.err
	}
	cfg := agent.ParseRunOnceOptions(opts)
	if cfg.OnReviewJob != nil {
		for i := 0; i < s.groups; i++ {
			var err error
			if i < s.failed {
				err = fmt.Errorf("group %d failed", i+1)
			}
			cfg.OnReviewJob(agent.ReviewJob{
				Pattern:   fmt.Sprintf("group-%d", i+1),
				Files:     []string{"a.go"},
				SessionID: fmt.Sprintf("sess-%d", i+1),
			}, err)
		}
	}
	return agent.RunResult{Text: "reviewed"}, nil
}

// resumeStarter resumes review sessions: the first failCount of them
// fail, the rest answer. A run where one group recovers and another
// does not is the partial case the outcome has to name.
type resumeStarter struct {
	name      string
	failCount int

	mu      sync.Mutex
	started int
}

func (s *resumeStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "", nil, nil)
}
func (s *resumeStarter) Detect() error { return nil }
func (s *resumeStarter) Init(string)   {}
func (s *resumeStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("resumeStarter: RunOnce not implemented")
}
func (s *resumeStarter) Review(context.Context, agent.StartConfig, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("resumeStarter: Review not implemented")
}

func (s *resumeStarter) Start(_ context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	s.mu.Lock()
	s.started++
	index := s.started
	s.mu.Unlock()
	events := make(chan agent.AgentEvent, 8)
	d := &resumeDriver{starter: s, index: index, events: events}
	return agent.NewAgent(s.Info(), 0, events, d), nil
}

// resumeDriver answers one resumed session — with an error when the
// session is one of the ones that must fail.
type resumeDriver struct {
	starter *resumeStarter
	index   int
	events  chan agent.AgentEvent
}

func (d *resumeDriver) SendBlocks(_ context.Context, _ []agent.ContentBlock) error {
	go func() {
		d.events <- agent.AgentEvent{
			Kind:      agent.EventAgentReady,
			SessionID: fmt.Sprintf("sess-%d", d.index),
			Model:     "model-x",
		}
		if d.index <= d.starter.failCount {
			d.events <- agent.AgentEvent{
				Kind: agent.EventAgentError,
				Err:  fmt.Errorf("session %d failed", d.index),
			}
			return
		}
		d.events <- agent.AgentEvent{
			Kind:   agent.EventAgentResult,
			Result: &agent.AgentResultEvent{Text: "done"},
		}
	}()
	return nil
}

func (d *resumeDriver) SendPermission(string) error { return nil }
func (d *resumeDriver) Reset(context.Context) error { return nil }
func (d *resumeDriver) Close() error                { return nil }
func (d *resumeDriver) Stop(context.Context) error  { return agent.ErrNotSupported }
func (d *resumeDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// waitOutcome waits for the settled message of a review and returns
// the outcome it carries.
func waitOutcome(t *testing.T, em *recordingEmitter, within time.Duration) *messages.Outcome {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case msg := <-em.sent:
			if msg.Kind == messages.OutMessageSettled {
				return msg.Outcome
			}
		case <-deadline:
			t.Fatal("no settled message")
			return nil
		}
	}
}

// assertOutcome checks the status and the Data keys of a review.
func assertOutcome(t *testing.T, got *messages.Outcome, want messages.OutcomeStatus, wantData map[string]string) {
	t.Helper()
	if got == nil {
		t.Fatal("no settled outcome: the review reported nothing")
	}
	if got.Status != want {
		t.Fatalf("outcome status = %s, want %s (outcome %+v)", got.Status, want, got)
	}
	for k, v := range wantData {
		if got.Data[k] != v {
			t.Errorf("outcome data[%q] = %q, want %q", k, got.Data[k], v)
		}
	}
}

// newOutcomeChat builds the chat a review runs against: the agent
// session is live (so the findings can be injected into it) and the
// prompt-end bus is off (the test does not drive a real readpump —
// the timing of the injected turn is covered by the e2e settle
// test).
func newOutcomeChat(t *testing.T, chatID, agentName string) (*chatsession.ChatSession, *recordingEmitter) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NIGHTME_CONFIG", filepath.Join(dir, "config.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("primary: "+agentName+"\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	em := newRecordingEmitter()
	cs, _ := newContinueChatSession(t, chatID, agentName, dir, em)
	cs.PromptEndBus = nil
	return cs, em
}

// TestReviewOutcome covers the four review verdicts a program
// branches on: every group finished, some group failed, there was no
// diff, and the agent cannot review at all.
func TestReviewOutcome(t *testing.T) {
	cases := []struct {
		name   string
		groups int
		failed int
		err    error
		want   messages.OutcomeStatus
		data   map[string]string
	}{
		{
			name:   "all groups finished",
			groups: 3,
			want:   messages.OutcomeOK,
			data:   map[string]string{"groups": "3", "agent": "reviewer"},
		},
		{
			name:   "one group failed",
			groups: 3,
			failed: 1,
			want:   messages.OutcomeIncomplete,
			data: map[string]string{
				"groups":            "3",
				"unfinished_groups": "1",
				"agent":             "reviewer",
			},
		},
		{
			name: "no diff",
			err:  agent.ErrNoDiff,
			want: messages.OutcomeNoop,
			data: map[string]string{"reason": "no_diff", "agent": "reviewer"},
		},
		{
			name: "not supported",
			err:  agent.ErrReviewNotSupported,
			want: messages.OutcomeFailed,
			data: map[string]string{"reason": "not_supported", "agent": "reviewer"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := newTestAgents(t)
			set.registry.Register(&groupReviewStarter{
				name:   "reviewer",
				groups: tc.groups,
				failed: tc.failed,
				err:    tc.err,
			})

			cs, em := newOutcomeChat(t, "review-outcome-"+t.Name(), "reviewer")
			f := NewFactory()
			// The dispatcher holds the message while the command
			// runs and releases it when the command returns.
			hold := cs.Hold("msg-1")
			ctx := chatsession.WithWork(context.Background(), hold)
			out, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
				ChatID:    cs.ChatID,
				MessageID: "msg-1",
				Text:      "/review",
				Args:      []string{"review"},
			})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if out == nil {
				t.Fatal("Handle returned nil SlashOutput")
			}
			hold.Release()
			got := waitOutcome(t, em, 5*time.Second)
			assertOutcome(t, got, tc.want, tc.data)
		})
	}
}

// TestReviewContinueOutcome covers the three continuation verdicts:
// the resumed groups finished, one of them did not, and there was
// nothing to resume at all.
func TestReviewContinueOutcome(t *testing.T) {
	t.Run("resumed groups finished", func(t *testing.T) {
		set := newTestAgents(t)
		set.registry.Register(&resumeStarter{name: "reviewer"})

		cs, em := newOutcomeChat(t, "review-continue-ok", "reviewer")
		f := NewFactory()
		f.record(cs.ChatID, "reviewer", cs.SelectedCwd(), []reviewOutcome{
			{pattern: "group-1", files: []string{"a.go"}, sessionID: "sess-1", err: errors.New("boom")},
		})

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
		assertOutcome(t, got, messages.OutcomeOK, map[string]string{
			"groups": "1",
			"agent":  "reviewer",
		})
	})

	t.Run("a resumed group failed again", func(t *testing.T) {
		// Two groups failed the first time; one of them recovers,
		// the other does not — the run is partial, not a failure.
		set := newTestAgents(t)
		set.registry.Register(&resumeStarter{name: "reviewer", failCount: 1})

		cs, em := newOutcomeChat(t, "review-continue-fail", "reviewer")
		f := NewFactory()
		f.record(cs.ChatID, "reviewer", cs.SelectedCwd(), []reviewOutcome{
			{pattern: "group-1", files: []string{"a.go"}, sessionID: "sess-1", err: errors.New("boom")},
			{pattern: "group-2", files: []string{"b.go"}, sessionID: "sess-2", err: errors.New("boom")},
		})

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
		assertOutcome(t, got, messages.OutcomeIncomplete, map[string]string{
			"groups":            "2",
			"unfinished_groups": "1",
			"agent":             "reviewer",
		})
	})

	t.Run("nothing to resume", func(t *testing.T) {
		set := newTestAgents(t)
		set.registry.Register(&resumeStarter{name: "reviewer"})

		cs, em := newOutcomeChat(t, "review-continue-nothing", "reviewer")
		f := NewFactory()
		f.record(cs.ChatID, "reviewer", cs.SelectedCwd(), []reviewOutcome{
			{pattern: "group-1", files: []string{"a.go"}, sessionID: "sess-1"},
		})

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
		assertOutcome(t, got, messages.OutcomeNoop, nil)
	})
}
