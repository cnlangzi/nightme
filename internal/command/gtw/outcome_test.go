package gtw

// The structured outcome every /gtw exit path reports.
//
// A program that drives nightme reads the outcome, not the card. So
// each exit path has to say what happened — a test that only
// checked the card would let a path report nothing at all and still
// pass.
//
// The probe below runs one command against a chat session and reads
// the settled message that comes out the other end.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// outcomeProbe runs a command against a chat session and reads the
// outcome it reported.
type outcomeProbe struct {
	cs   *chatsession.ChatSession
	ch   *recordingCh
	work *chatsession.Work
	ctx  context.Context
}

// newOutcomeProbe builds a chat session whose outbound messages are
// recorded, and takes the hold the inbound dispatcher would take for
// the message the command answers.
func newOutcomeProbe(t *testing.T) *outcomeProbe {
	t.Helper()
	cs := &chatsession.ChatSession{}
	ch := &recordingCh{}
	cs.WithEmitter(ch)
	work := cs.Hold("msg-1")
	return &outcomeProbe{
		cs:   cs,
		ch:   ch,
		work: work,
		ctx:  chatsession.WithWork(context.Background(), work),
	}
}

// settle releases the message's work — the dispatcher does it when
// the command returns — and returns the outcome that went out.
func (p *outcomeProbe) settle(t *testing.T) *messages.Outcome {
	t.Helper()
	p.work.Release()
	p.ch.mu.Lock()
	defer p.ch.mu.Unlock()
	var out *messages.Outcome
	for _, m := range p.ch.sends {
		if m.Kind != messages.OutMessageSettled {
			continue
		}
		if out != nil {
			t.Fatalf("more than one settled message")
		}
		out = m.Outcome
	}
	return out
}

// assertOutcome checks the status and the Data keys a command
// reported. A missing report fails the test — that is the whole
// point of the probe.
func assertOutcome(t *testing.T, got *messages.Outcome, want messages.OutcomeStatus, wantData map[string]string) {
	t.Helper()
	if got == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
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

// TestGtwOutcome_Usage covers the two factory-level paths that never
// reach a subcommand: no subcommand at all, and a subcommand that
// does not exist.
func TestGtwOutcome_Usage(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"no subcommand", []string{"gtw"}, "usage"},
		{"unknown subcommand", []string{"gtw", "nope"}, "unknown_subcommand"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newOutcomeProbe(t)
			f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
			_, err := f.Handle(p.ctx, command.RuntimeServices{}, nil, p.cs, command.SlashInput{
				ChatID:    "chat",
				MessageID: "msg-1",
				Text:      "/gtw",
				Args:      tc.argv,
			})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			got := p.settle(t)
			assertOutcome(t, got, messages.OutcomeFailed, map[string]string{"reason": tc.want})
		})
	}
}

// TestGtwOutcome_AbortNothingRunning covers /gtw abort with no
// command in flight: there was nothing to do.
func TestGtwOutcome_AbortNothingRunning(t *testing.T) {
	p := newOutcomeProbe(t)
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
	_, err := f.Handle(p.ctx, command.RuntimeServices{}, nil, p.cs, command.SlashInput{
		ChatID:    "chat",
		MessageID: "msg-1",
		Text:      "/gtw abort",
		Args:      []string{"gtw", "abort"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := p.settle(t)
	assertOutcome(t, got, messages.OutcomeNoop, nil)
}

// TestGtwOutcome_Push covers the /gtw push exit paths a program
// branches on: the refusals, the "nothing to push" noop, and the
// success that names the branch, the commit count and the remote
// head the push left behind.
func TestGtwOutcome_Push(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		git := newPushGit()
		git.onArgs(statusCmd, "## wt-dirty...origin/wt-dirty\n M file.go\n", "", nil)

		withCwd(t, t.TempDir())
		writeYml(t, mustPwd(t), Context{
			Worktree: mustPwd(t),
			Branch:   "wt-dirty",
			RepoRoot: mustPwd(t),
		})

		p := newOutcomeProbe(t)
		p.cs.SetSelectedCwd(mustPwd(t))
		if _, err := dispatchPush(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", pushArgs{}); err != nil {
			t.Fatalf("dispatchPush: %v", err)
		}
		assertOutcome(t, p.settle(t), messages.OutcomeFailed, map[string]string{"reason": "dirty"})
	})

	t.Run("detached", func(t *testing.T) {
		git := newPushGit()
		git.onArgs(statusCmd, "## HEAD (no branch)\n", "", nil)

		withCwd(t, t.TempDir())
		writeYml(t, mustPwd(t), Context{
			Worktree: mustPwd(t),
			Branch:   "wt-detached",
			RepoRoot: mustPwd(t),
		})

		p := newOutcomeProbe(t)
		p.cs.SetSelectedCwd(mustPwd(t))
		if _, err := dispatchPush(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", pushArgs{}); err != nil {
			t.Fatalf("dispatchPush: %v", err)
		}
		assertOutcome(t, p.settle(t), messages.OutcomeFailed, map[string]string{"reason": "detached"})
	})

	t.Run("nothing to push", func(t *testing.T) {
		git := newPushGit()
		git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)

		withCwd(t, t.TempDir())
		writeYml(t, mustPwd(t), Context{
			Worktree: mustPwd(t),
			Branch:   "wt-clean",
			RepoRoot: mustPwd(t),
		})

		p := newOutcomeProbe(t)
		p.cs.SetSelectedCwd(mustPwd(t))
		if _, err := dispatchPush(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", pushArgs{}); err != nil {
			t.Fatalf("dispatchPush: %v", err)
		}
		assertOutcome(t, p.settle(t), messages.OutcomeNoop, map[string]string{"branch": "wt-clean"})
	})

	t.Run("pushed", func(t *testing.T) {
		git := newPushGit()
		git.onArgs(statusCmd, "## wt-pushed...origin/wt-pushed [ahead 2]\n", "", nil)
		// The remote tip is read twice: before the push (the left
		// side of the rev range) and after it (the head the push
		// left behind).
		git.onArgsSeq([]string{"rev-parse", "--verify", "origin/wt-pushed"},
			pushGitResp{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil},
			pushGitResp{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "", nil},
		)
		git.onSeq("rev-list",
			pushGitResp{"2", "", nil},
			pushGitResp{"0", "", nil},
		)
		git.on("push", "To origin", "", nil)
		git.onArgs([]string{"log", "--oneline", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..origin/wt-pushed"},
			"abc1234 commit 1\ndef5678 commit 2\n", "", nil)

		withCwd(t, t.TempDir())
		writeYml(t, mustPwd(t), Context{
			Worktree: mustPwd(t),
			Branch:   "wt-pushed",
			RepoRoot: mustPwd(t),
		})

		p := newOutcomeProbe(t)
		p.cs.SetSelectedCwd(mustPwd(t))
		if _, err := dispatchPush(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", pushArgs{}); err != nil {
			t.Fatalf("dispatchPush: %v", err)
		}
		assertOutcome(t, p.settle(t), messages.OutcomeOK, map[string]string{
			"branch":      "wt-pushed",
			"commits":     "2",
			"remote_head": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		})
	})
}

// TestGtwOutcome_CommitNothingToDo covers /gtw commit on a clean
// worktree: there was nothing to commit.
func TestGtwOutcome_CommitNothingToDo(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-clean",
		RepoRoot: mustPwd(t),
	})

	p := newOutcomeProbe(t)
	p.cs.SetSelectedCwd(mustPwd(t))
	if _, err := dispatchCommit(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, Config{}); err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	assertOutcome(t, p.settle(t), messages.OutcomeNoop, map[string]string{"branch": "wt-clean"})
}

// TestGtwOutcome_ConfigQuery covers /gtw config without a value: it
// reports the current setting and changes nothing.
func TestGtwOutcome_ConfigQuery(t *testing.T) {
	p := newOutcomeProbe(t)
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
	_, err := f.Handle(p.ctx, command.RuntimeServices{}, nil, p.cs, command.SlashInput{
		ChatID:    "chat",
		MessageID: "msg-1",
		Text:      "/gtw config lang",
		Args:      []string{"gtw", "config", "lang"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := p.settle(t)
	assertOutcome(t, got, messages.OutcomeNoop, nil)
	if got.Data["scope"] == "" {
		t.Errorf("outcome data = %v, want a scope", got.Data)
	}
}

// TestGtwOutcome_ConfigWrite covers /gtw config with a value: the
// setting changed.
func TestGtwOutcome_ConfigWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("NIGHTME_CONFIG", dir+"/config.yaml")

	p := newOutcomeProbe(t)
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
	_, err := f.Handle(p.ctx, command.RuntimeServices{}, nil, p.cs, command.SlashInput{
		ChatID:    "chat",
		MessageID: "msg-1",
		Text:      "/gtw config -g lang English",
		Args:      []string{"gtw", "config", "-g", "lang", "English"},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := p.settle(t)
	assertOutcome(t, got, messages.OutcomeOK, map[string]string{
		"lang":  "English",
		"scope": "global",
	})
}

// TestGtwOutcome_ReportHappensOnce guards the merge: a command that
// reports twice must still settle once.
func TestGtwOutcome_ReportHappensOnce(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-clean",
		RepoRoot: mustPwd(t),
	})

	p := newOutcomeProbe(t)
	p.cs.SetSelectedCwd(mustPwd(t))
	if _, err := dispatchPush(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", pushArgs{}); err != nil {
		t.Fatalf("dispatchPush: %v", err)
	}
	got := p.settle(t)
	if got == nil {
		t.Fatal("no settled outcome")
	}
	if strings.TrimSpace(got.Summary) == "" {
		t.Error("outcome summary is empty; every outcome names what happened")
	}
}

// TestGtwOutcome_NewCreatePartial covers the /gtw new -y run that
// opened some issues and then stopped: the card says ⚠️ and the
// outcome must not say ok.
//
// The record of the opened issue is written where a directory sits —
// the issue is opened, the record cannot be written, the run stops,
// and the remaining drafts stay on disk.
func TestGtwOutcome_NewCreatePartial(t *testing.T) {
	dir := t.TempDir()
	writeDrafts(t, dir, "01.md", "# feat(gtw): archive chats\n\n## Requirements\n- R1\n")

	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetCreateIssueFunc(func(title, body string) (*Issue, error) {
		// A directory where the record file belongs — writing it
		// fails once the issue is open.
		if err := os.MkdirAll(filepath.Join(dir, "01.created"), 0o755); err != nil {
			return nil, err
		}
		return &Issue{ID: 42, Title: title, URL: "https://github.com/o/r/issues/42"}, nil
	})

	card, outcome := createIssuesFromDrafts(context.Background(), dir, resolveTo(prov))
	if !strings.HasPrefix(card, "⚠️") {
		t.Fatalf("card = %q, want a partial-success card", card)
	}
	if outcome.Status != messages.OutcomeIncomplete {
		t.Fatalf("outcome = %+v, want incomplete — the card says ⚠️", outcome)
	}
	if outcome.Data["issues"] != "42" {
		t.Errorf("outcome issues = %q, want 42", outcome.Data["issues"])
	}
}

// TestGtwOutcome_AbortedRunKeepsItsFacts covers the abort guard.
//
// A cancelled run reports `cancelled`, and the `failed` verdict the
// cancellation caused is dropped — `failed` outranks `cancelled` in
// the merge and would report the cancellation as a failure. The
// facts of the work the run did BEFORE the cancellation still reach
// the program: a /gtw close that removed the worktree and was
// aborted during its sync did remove the worktree.
func TestGtwOutcome_AbortedRunKeepsItsFacts(t *testing.T) {
	p := newOutcomeProbe(t)
	ctx, cancel := context.WithCancelCause(p.ctx)
	cancel(ErrAborted)

	// The failure the cancellation caused: dropped.
	reportOutcome(ctx, command.FailedOutcome("rejected", "the push was rejected"))
	// The work the run did before the cancellation: kept.
	reportOutcome(ctx, messages.Outcome{
		Status:  messages.OutcomeIncomplete,
		Summary: "the worktree was closed but the sync failed",
		Data:    map[string]string{"branch": "feat/x", "worktree": "/tmp/wt"},
	})
	// The cancellation itself.
	chatsession.ReportOutcome(p.ctx, messages.Outcome{
		Status:  messages.OutcomeCancelled,
		Summary: "the command was cancelled by /gtw abort",
		Data:    map[string]string{"reason": "aborted"},
	})

	got := p.settle(t)
	if got == nil {
		t.Fatal("no settled outcome")
	}
	if got.Status != messages.OutcomeCancelled {
		t.Fatalf("outcome = %+v, want cancelled", got)
	}
	if got.Data["branch"] != "feat/x" {
		t.Errorf("outcome data = %v, want the branch the run worked on", got.Data)
	}
	if got.Data["reason"] == "rejected" {
		t.Errorf("outcome data = %v, want the cancellation's reason, not the failure's", got.Data)
	}
}
