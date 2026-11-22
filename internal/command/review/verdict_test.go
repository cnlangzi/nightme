package review

// The verdict /review judges itself by.
//
// A review finds problems and hands them to the coding agent; the
// fixes land in the turn the findings were injected into. The review
// knows two facts: how many findings it collected, and what the
// workspace holds once those findings were acted on.
//
// Nothing to fix is a normal result. Findings that were addressed are
// a normal result too. Findings that were not addressed are an
// exception: the review's work did not land.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// initGitRepo creates a throwaway git repository with one commit, so
// the review has a workspace it can read a head from.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping the real-git review test")
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}
	mustGit(t, dir, "add", "a.go")
	mustGit(t, dir, "commit", "-q", "-m", "feat: the base")
	return dir
}

// mustGit runs a git command in dir and fails the test when it does
// not succeed.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// waitForInjection waits until the findings reached the agent.
func waitForInjection(t *testing.T, d *recordingDriver) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(d.blocks()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the findings never reached the agent")
}

// TestReviewContinueVerdict covers the continuation's judgment: the
// findings it resumes are judged the same way a plain review's are.
func TestReviewContinueVerdict(t *testing.T) {
	cases := []struct {
		name string
		// fix makes the coding agent commit the fixes.
		fix  bool
		want messages.OutcomeStatus
		// reason is the reason code the outcome must carry.
		reason string
		// wantErr says the outcome must carry an error description.
		wantErr bool
	}{
		{
			name: "the findings were fixed",
			fix:  true,
			want: messages.OutcomeOK,
		},
		{
			name:    "the findings were left unaddressed",
			want:    messages.OutcomeFailed,
			reason:  "not_fixed",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := initGitRepo(t)
			agents := newTestAgents(t)
			recorded := &reviewSessionStarter{
				name:  "review-continue-verdict",
				reply: "- **critical**: a.go:1 — the problem",
			}
			chatRunner := &reviewSessionStarter{name: "review-chat-agent", reply: "must not run"}
			agents.registry.Register(recorded)
			agents.registry.Register(chatRunner)

			em := newRecordingEmitter()
			const chatID = "review-continue-verdict"
			cs, asDriver := newContinueChatSession(t, chatID, chatRunner.name, dir, em)
			enableThink(t, cs)

			f := NewFactory()
			f.record(chatID, recorded.name, dir, reviewRun{outcomes: []reviewOutcome{
				{pattern: "**/*.go", files: []string{"a.go"}, sessionID: "sess-go", err: errors.New("boom")},
			}})

			hold := cs.Hold("msg-1")
			ctx := chatsession.WithWork(context.Background(), hold)
			if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
				ChatID:    cs.ChatID,
				MessageID: "msg-1",
				Text:      "/review -c",
				Args:      []string{"review", "-c"},
			}); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			hold.Release()

			// The findings reached the agent.
			waitForBlocks(t, asDriver, 1)
			if tc.fix {
				if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\n// fixed\n"), 0o644); err != nil {
					t.Fatalf("write the fix: %v", err)
				}
				mustGit(t, dir, "add", "a.go")
				mustGit(t, dir, "commit", "-q", "-m", "fix: the findings")
			}

			// The turn the findings went into ends.
			cs.PromptEndBus.Publish(chatsession.PromptEndedEvent{
				UserMsgID: "msg-1",
				Reason:    agent.PromptEndClean,
			})

			got := waitOutcome(t, em, 5*time.Second)
			if got.Status != tc.want {
				t.Fatalf("outcome status = %s, want %s (outcome %+v)", got.Status, tc.want, got)
			}
			if tc.reason != "" && got.Data["reason"] != tc.reason {
				t.Errorf("outcome reason = %q, want %q", got.Data["reason"], tc.reason)
			}
			if tc.wantErr {
				if got.Data["error"] == "" {
					t.Errorf("an exception carries an error description, got none: %v", got.Data)
				}
			} else if desc, ok := got.Data["error"]; ok {
				t.Errorf("a normal result carries no error description, got %q", desc)
			}
		})
	}
}

// TestReviewVerdict covers the three verdicts the review judges
// itself by: nothing to fix, the findings addressed, the findings
// left unaddressed.
func TestReviewVerdict(t *testing.T) {
	cases := []struct {
		name string
		// findings is how many findings the review reports.
		findings int
		// fix makes the coding agent commit the fixes.
		fix  bool
		want messages.OutcomeStatus
		// reason is the reason code the outcome must carry.
		reason string
		// wantErr says the outcome must carry an error description.
		wantErr bool
	}{
		{
			name: "nothing to fix",
			want: messages.OutcomeOK,
		},
		{
			name:     "the findings were fixed",
			findings: 2,
			fix:      true,
			want:     messages.OutcomeOK,
		},
		{
			name:     "the findings were left unaddressed",
			findings: 2,
			want:     messages.OutcomeFailed,
			reason:   "not_fixed",
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := initGitRepo(t)
			set := newTestAgents(t)
			set.registry.Register(&groupReviewStarter{
				name:     "reviewer",
				groups:   1,
				findings: tc.findings,
			})

			em := newRecordingEmitter()
			cs, driver := newContinueChatSession(t, "review-verdict-"+t.Name(), "reviewer", dir, em)
			f := NewFactory()
			// The dispatcher holds the message while the command
			// runs and releases it when the command returns.
			hold := cs.Hold("msg-1")
			ctx := chatsession.WithWork(context.Background(), hold)
			if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
				ChatID:    cs.ChatID,
				MessageID: "msg-1",
				Text:      "/review",
				Args:      []string{"review"},
			}); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			hold.Release()

			// The review hands its findings to the agent.
			waitForInjection(t, driver)
			if tc.fix {
				// The coding agent fixed the findings: a new
				// commit lands on the branch.
				if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\n// fixed\n"), 0o644); err != nil {
					t.Fatalf("write the fix: %v", err)
				}
				mustGit(t, dir, "add", "a.go")
				mustGit(t, dir, "commit", "-q", "-m", "fix: the findings")
			}

			// The turn the findings went into ends: the review
			// judges what the work left behind.
			cs.PromptEndBus.Publish(chatsession.PromptEndedEvent{
				UserMsgID: "msg-1",
				Reason:    agent.PromptEndClean,
			})

			got := waitOutcome(t, em, 5*time.Second)
			assertOutcome(t, got, tc.want, map[string]string{"agent": "reviewer"})
			if tc.reason != "" {
				assertOutcome(t, got, tc.want, map[string]string{"reason": tc.reason})
			}
			if tc.wantErr {
				if got.Data["error"] == "" {
					t.Errorf("an exception carries an error description, got none: %v", got.Data)
				}
			} else if desc, ok := got.Data["error"]; ok {
				t.Errorf("a normal result carries no error description, got %q", desc)
			}
		})
	}
}
