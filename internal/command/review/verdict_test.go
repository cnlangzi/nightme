package review

// The verdict /review judges itself by.
//
// A review finds problems and hands them to the coding agent; the
// review knows two facts: how many findings it collected, and what
// the workspace holds once those findings were acted on.
//
// A review that ran is a normal result. What came of the findings is
// a fact — the `addressed` fact — and a turn that did not end cleanly
// carries its own result, not a verdict about the findings.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/pkg/outcome"
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

// assertAddressed checks the `addressed` fact of an outcome. An empty
// want means the key must not be there at all.
func assertAddressed(t *testing.T, got *outcome.Outcome, want string) {
	t.Helper()
	if got == nil {
		t.Fatal("no settled outcome: the review reported nothing")
	}
	if want == "" {
		if _, ok := got.Data[BoolKeyAddressed]; ok {
			t.Errorf("the outcome carries an %q fact: %v", BoolKeyAddressed, got)
		}
		return
	}
	if got.Data[BoolKeyAddressed] != want {
		t.Errorf("outcome data[%q] = %q, want %q", BoolKeyAddressed, got.Data[BoolKeyAddressed], want)
	}
}

// TestReviewContinueVerdict covers the continuation's judgment: the
// findings it resumes are judged the same way a plain review's are.
func TestReviewContinueVerdict(t *testing.T) {
	cases := []struct {
		name string
		// fix makes the coding agent commit the fixes.
		fix bool
		// addressed is the `addressed` fact the outcome must carry.
		addressed string
	}{
		{
			name:      "the findings were fixed",
			fix:       true,
			addressed: "true",
		},
		{
			name:      "the findings were left unaddressed",
			addressed: "false",
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
			assertOutcome(t, got, http.StatusOK, map[string]string{"agent": "review-continue-verdict"})
			assertAddressed(t, got, tc.addressed)
			if got.Exception() {
				t.Errorf("a normal result is not an exception: %v", got)
			}
		})
	}
}

// TestReviewVerdict covers the verdicts the review judges itself by:
// nothing to fix, the findings addressed, the findings left
// unaddressed, and a turn that did not end cleanly.
func TestReviewVerdict(t *testing.T) {
	cases := []struct {
		name string
		// findings is how many findings the review reports.
		findings int
		// fix makes the coding agent commit the fixes.
		fix bool
		// reason is how the turn the findings went into ended. The
		// zero value is a clean end.
		reason agent.PromptEndReason
		// noGit makes the workspace a directory that is not a git
		// repository: the verdict has no baseline to judge against.
		noGit bool
		want  int
		// wantErr says the outcome must be an exception.
		wantErr bool
		// addressed is the `addressed` fact the outcome must carry.
		// Empty means the key must not be there at all.
		addressed string
	}{
		{
			name: "nothing to fix",
			want: http.StatusOK,
		},
		{
			name:      "the findings were fixed",
			findings:  2,
			fix:       true,
			want:      http.StatusOK,
			addressed: "true",
		},
		{
			name:      "the findings were left unaddressed",
			findings:  2,
			want:      http.StatusOK,
			addressed: "false",
		},
		{
			name:     "the workspace could not be read",
			findings: 2,
			noGit:    true,
			want:     http.StatusOK,
		},
		{
			name:     "the turn ended badly",
			findings: 2,
			reason:   agent.PromptEndError,
			want:     http.StatusInternalServerError,
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := initGitRepo(t)
			if tc.noGit {
				dir = t.TempDir()
			}
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
				Reason:    tc.reason,
			})

			got := waitOutcome(t, em, 5*time.Second)
			assertOutcome(t, got, tc.want, map[string]string{"agent": "reviewer"})
			assertAddressed(t, got, tc.addressed)
			if tc.wantErr {
				if got.Success {
					t.Errorf("an exception is not a success: %v", got)
				}
			} else if got.Exception() {
				t.Errorf("a normal result is not an exception: %v", got)
			}
		})
	}
}
