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
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
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
	// findings is how many findings each group reports.
	findings int
	// allGroupsFail makes the run fail with every group: the fan-out
	// returns an error when no group survives.
	allGroupsFail bool
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
		report := agent.ReviewReport{Parsed: true}
		for i := 0; i < s.findings; i++ {
			report.Findings = append(report.Findings, agent.ReviewFinding{
				Path:     "a.go",
				Line:     "1",
				Severity: agent.SeverityHigh,
				Content:  "the problem",
			})
		}
		for i := 0; i < s.groups; i++ {
			var err error
			if i < s.failed {
				err = fmt.Errorf("group %d failed", i+1)
			}
			cfg.OnReviewJob(agent.ReviewJob{
				Pattern:   fmt.Sprintf("group-%d", i+1),
				Files:     []string{"a.go"},
				SessionID: fmt.Sprintf("sess-%d", i+1),
				Report:    report,
			}, err)
		}
	}
	if s.allGroupsFail {
		return agent.RunResult{}, fmt.Errorf("all %d review groups failed", s.groups)
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

// Ping answers the daemon's liveness probe. The fake is
// responsive unless the test says otherwise.
func (d *resumeDriver) Ping(context.Context) error { return nil }

// waitOutcome waits for the settled message of a review and returns
// the outcome it carries.
func waitOutcome(t *testing.T, em *recordingEmitter, within time.Duration) *outcome.Outcome {
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
func assertOutcome(t *testing.T, got *outcome.Outcome, want int, wantData map[string]string) {
	t.Helper()
	if got == nil {
		t.Fatal("no settled outcome: the review reported nothing")
	}
	if got.Status != want {
		t.Fatalf("outcome status = %d, want %d (outcome %+v)", got.Status, want, got)
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

// TestReviewOutcome covers the review verdicts a program branches on:
// every group finished, some group failed, every group failed, there
// was no diff, and the agent cannot review at all.
func TestReviewOutcome(t *testing.T) {
	cases := []struct {
		name   string
		groups int
		failed int
		err    error
		// allGroupsFail makes the fan-out fail with every group.
		allGroupsFail bool
		want          int
		data          map[string]string
		// wantErr says the outcome must carry an error description —
		// the review's failures are exceptions, not normal branches.
		wantErr bool
		// noUnfinished says the outcome must carry no unfinished
		// count: the fan-out never ran, so there is no work to pick
		// up.
		noUnfinished bool
	}{
		{
			name:   "all groups finished",
			groups: 3,
			want:   http.StatusOK,
			data: map[string]string{
				"groups": "3", "unfinished": "0", "agent": "reviewer", "delivered": "true",
			},
		},
		{
			name:    "one group failed",
			groups:  3,
			failed:  1,
			want:    http.StatusInternalServerError,
			wantErr: true,
			data: map[string]string{
				"groups": "3", "unfinished": "1", "agent": "reviewer", "delivered": "false",
			},
		},
		{
			name:          "every group failed",
			groups:        2,
			failed:        2,
			allGroupsFail: true,
			want:          http.StatusInternalServerError,
			wantErr:       true,
			data: map[string]string{
				"unfinished": "2", "agent": "reviewer", "delivered": "false",
			},
		},
		{
			name:         "no diff",
			err:          agent.ErrNoDiff,
			want:         http.StatusNotFound,
			data:         map[string]string{"agent": "reviewer", "delivered": "false"},
			wantErr:      true,
			noUnfinished: true,
		},
		{
			name:         "not supported",
			err:          agent.ErrReviewNotSupported,
			want:         http.StatusNotImplemented,
			data:         map[string]string{"agent": "reviewer", "delivered": "false"},
			wantErr:      true,
			noUnfinished: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := newTestAgents(t)
			set.registry.Register(&groupReviewStarter{
				name:          "reviewer",
				groups:        tc.groups,
				failed:        tc.failed,
				allGroupsFail: tc.allGroupsFail,
				err:           tc.err,
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
			if tc.wantErr {
				if got.Success {
					t.Errorf("an exception is not a success: %v", got)
				}
			} else if got.Exception() {
				t.Errorf("a normal result is not an exception: %v", got)
			}
			if tc.noUnfinished {
				if n, ok := got.Data["unfinished"]; ok {
					t.Errorf("the outcome carries the unfinished count %q, want none", n)
				}
			}
		})
	}
}

// TestReviewContinueOutcome covers the continuation verdicts: the
// resumed groups finished, one of them did not, every one of them
// failed again, and there was nothing to resume at all.
func TestReviewContinueOutcome(t *testing.T) {
	t.Run("resumed groups finished", func(t *testing.T) {
		set := newTestAgents(t)
		set.registry.Register(&resumeStarter{name: "reviewer"})

		cs, em := newOutcomeChat(t, "review-continue-ok", "reviewer")
		f := NewFactory()
		f.record(cs.ChatID, "reviewer", cs.SelectedCwd(), reviewRun{outcomes: []reviewOutcome{
			{pattern: "group-1", files: []string{"a.go"}, sessionID: "sess-1", err: errors.New("boom")},
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
		assertOutcome(t, got, http.StatusOK, map[string]string{
			"continued_groups": "1",
			"agent":            "reviewer",
			"delivered":        "true",
		})
	})

	t.Run("a resumed group failed again", func(t *testing.T) {
		// Two groups failed the first time; one of them recovers,
		// the other does not — the review is delivered with the
		// group that died named in it.
		set := newTestAgents(t)
		set.registry.Register(&resumeStarter{name: "reviewer", failCount: 1})

		cs, em := newOutcomeChat(t, "review-continue-fail", "reviewer")
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
			"agent":            "reviewer",
			"delivered":        "true",
		})
	})

	t.Run("every continued group failed again", func(t *testing.T) {
		// The groups failed again: their recovery attempt is spent,
		// so the review is over. The card names the groups that
		// died.
		set := newTestAgents(t)
		set.registry.Register(&resumeStarter{name: "reviewer", failCount: 2})

		cs, em := newOutcomeChat(t, "review-continue-all-fail", "reviewer")
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
			"unfinished": "2",
			"agent":      "reviewer",
			"delivered":  "true",
		})
	})

	t.Run("nothing to resume", func(t *testing.T) {
		set := newTestAgents(t)
		set.registry.Register(&resumeStarter{name: "reviewer"})

		cs, em := newOutcomeChat(t, "review-continue-nothing", "reviewer")
		f := NewFactory()
		f.record(cs.ChatID, "reviewer", cs.SelectedCwd(), reviewRun{outcomes: []reviewOutcome{
			{pattern: "group-1", files: []string{"a.go"}, sessionID: "sess-1"},
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
		assertOutcome(t, got, http.StatusOK, nil)
	})
}
