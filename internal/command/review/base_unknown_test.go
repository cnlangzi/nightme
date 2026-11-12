package review

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/gitbase"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestReviewBaseUnknown covers the base-unknown exit: the review
// fails distinctly, the outcome names the reason, and the chat is
// told that the committed diff was not reviewed — with the command
// that fixes the cause.
func TestReviewBaseUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason gitbase.Reason
		fix    string
	}{
		{
			name:   "the remote did not answer",
			reason: gitbase.ReasonRemoteUnreachable,
			fix:    "remote set-head origin -a",
		},
		{
			name:   "the tracking ref is missing",
			reason: gitbase.ReasonMissingTrackingRef,
			fix:    "fetch origin",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := newTestAgents(t)
			set.registry.Register(&groupReviewStarter{
				name: "reviewer",
				err:  &agent.BaseUnknownError{Workspace: "/ws", Reason: tc.reason},
			})

			cs, em := newOutcomeChat(t, "review-base-unknown-"+t.Name(), "reviewer")
			workspace := cs.SelectedCwd()
			f := NewFactory()
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
			assertOutcome(t, got, messages.OutcomeFailed, map[string]string{
				"reason": "base_unknown",
				"agent":  "reviewer",
			})

			reply := findReply(em.messages(), "could not determine the base branch")
			if reply == "" {
				t.Fatalf("no reply about the unknown base branch; got %+v", em.messages())
			}
			for _, want := range []string{
				workspace,
				"The committed diff was not reviewed",
				"git -C " + workspace + " " + tc.fix,
			} {
				if !strings.Contains(reply, want) {
					t.Errorf("reply does not contain %q:\n%s", want, reply)
				}
			}
		})
	}
}

// TestBaseUnknownMessage_QuotesTheWorkspacePath covers the fix
// command the user pastes: a workspace path with a space must be
// quoted, a plain one must stay bare.
func TestBaseUnknownMessage_QuotesTheWorkspacePath(t *testing.T) {
	const spaced = "/home/me/my project"
	msg := baseUnknownMessage(spaced,
		&agent.BaseUnknownError{Workspace: spaced, Reason: gitbase.ReasonRemoteUnreachable})
	if !strings.Contains(msg, "git -C "+shellPath(spaced)+" remote set-head origin -a") {
		t.Errorf("the fix command does not carry the quoted workspace path:\n%s", msg)
	}

	const plain = "/home/me/repo"
	msg = baseUnknownMessage(plain,
		&agent.BaseUnknownError{Workspace: plain, Reason: gitbase.ReasonMissingTrackingRef})
	if !strings.Contains(msg, "git -C "+plain+" fetch origin") {
		t.Errorf("the fix command does not carry the plain path:\n%s", msg)
	}
}

// TestShellPath covers the quoting contract: a path made of safe
// characters stays bare, anything else is quoted in the platform's
// shell style — the message is copy-pasted into that shell.
func TestShellPath(t *testing.T) {
	if got := shellPath("/home/me/repo"); got != "/home/me/repo" {
		t.Errorf("shellPath(plain) = %q, want it bare", got)
	}
	got := shellPath("/home/me/my project")
	if got == "/home/me/my project" {
		t.Error("shellPath(path with a space) is not quoted")
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(got, `"`) {
			t.Errorf("shellPath = %q, want double quotes on Windows", got)
		}
		return
	}
	if !strings.HasPrefix(got, "'") {
		t.Errorf("shellPath = %q, want single quotes on Unix", got)
	}
}

// findReply returns the first outbound reply carrying want.
func findReply(msgs []messages.OutboundMessage, want string) string {
	for _, msg := range msgs {
		if msg.Kind == messages.OutReply && strings.Contains(msg.Text, want) {
			return msg.Text
		}
	}
	return ""
}
