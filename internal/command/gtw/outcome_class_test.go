package gtw

// The classification of the outcomes /gtw fix, /gtw commit,
// /gtw push and /gtw pr report.
//
// Two kinds of failure sit on one status: a normal branch (there was
// nothing to do) and an exception (something broke). The error
// description is the axis between them — a normal branch carries
// none.
//
// The commands that fall back to a second check — "the work may in
// fact have been done" — are covered here too: the fallback decides
// the class, not the primary signal.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// settledOutcome returns the outcome of the settled message the
// channel recorded, or nil when the message has not settled.
func settledOutcomeOf(ch *recordingCh) *outcome.Outcome {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	for _, m := range ch.sends {
		if m.Kind == messages.OutMessageSettled {
			return m.Outcome
		}
	}
	return nil
}

// assertNoError asserts that the outcome is a normal branch: a 2xx.
func assertNoError(t *testing.T, got *outcome.Outcome) {
	t.Helper()
	if got == nil {
		t.Fatal("no settled outcome")
	}
	if !got.Success {
		t.Errorf("a normal branch is a success, got status %d", got.Status)
	}
}

// assertError asserts that the outcome is an exception: a 4xx or a
// 5xx.
func assertError(t *testing.T, got *outcome.Outcome) {
	t.Helper()
	if got == nil {
		t.Fatal("no settled outcome")
	}
	if got.Success {
		t.Errorf("an exception is not a success, got status %d", got.Status)
	}
}

// TestGtwOutcome_CommitCreated covers the success path of /gtw
// commit: the agent committed the work — a normal result that
// carries no error description.
func TestGtwOutcome_CommitCreated(t *testing.T) {
	git := newPushGit()
	// Status sequence: entry dirty → re-snapshot clean.
	git.onArgsSeq(statusCmd,
		pushGitResp{"## wt-dirty...origin/wt-dirty\n M foo.go\n", "", nil},
		pushGitResp{"## wt-dirty...origin/wt-dirty\n", "", nil},
	)
	// rev-parse HEAD sequence: entry headBefore → verify headAfter.
	git.onArgsSeq([]string{"rev-parse", "HEAD"},
		pushGitResp{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "", nil},
		pushGitResp{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "", nil},
	)
	git.onArgs([]string{"--no-optional-locks", "status", "--porcelain"}, "", "", nil)
	git.onArgs([]string{"rev-parse", "--abbrev-ref", "HEAD"}, "wt-dirty\n", "", nil)
	git.on("log", "bbbbbbbb feat: agent did the thing\n", "", nil)

	pi := &recordingAgent{name: "pi"}
	withAgent(t, pi)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree: mustPwd(t),
		Branch:   "wt-dirty",
		RepoRoot: mustPwd(t),
		Issue:    7,
	})

	p := newOutcomeProbe(t)
	p.cs.SetSelectedCwd(mustPwd(t))
	p.cs.SetSelectedAgent("pi")
	if _, err := dispatchCommit(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, Config{}); err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, map[string]string{
		"branch":  "wt-dirty",
		"commits": "1",
	})
	assertNoError(t, got)
}

// TestGtwOutcome_CommitAlreadyCommitted covers the enhanced
// judgment of /gtw commit: the worktree is clean, but the branch
// carries a commit the coding agent made before the command ran.
//
// The outcome is a normal branch — there was nothing to commit, and
// nothing broke — and it names the commit that is already there.
func TestGtwOutcome_CommitAlreadyCommitted(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)
	git.onArgs([]string{"log", "--format=%ae\t%h %s", "base000..HEAD"},
		"test@example.com\tabc1234 feat: the work\n", "", nil)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree:    mustPwd(t),
		Branch:      "wt-clean",
		RepoRoot:    mustPwd(t),
		InitialHead: "base000",
	})

	p := newOutcomeProbe(t)
	p.cs.SetSelectedCwd(mustPwd(t))
	if _, err := dispatchCommit(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, Config{}); err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, map[string]string{"branch": "wt-clean",
		"commits":    "1",
		"commit_log": "abc1234 feat: the work"})
	assertNoError(t, got)
	if !strings.Contains(got.StatusText, "already committed") {
		t.Errorf("outcome summary = %q, want it to say the work was committed before", got.StatusText)
	}
}

// TestGtwOutcome_CommitNothingCommitted covers the other side of the
// enhanced judgment: the branch carries no commit since the initial
// head, so there is genuinely nothing to commit — an exception.
func TestGtwOutcome_CommitNothingCommitted(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)
	git.onArgs([]string{"log", "--format=%ae\t%h %s", "base000..HEAD"}, "", "", nil)

	withCwd(t, t.TempDir())
	writeYml(t, mustPwd(t), Context{
		Worktree:    mustPwd(t),
		Branch:      "wt-clean",
		RepoRoot:    mustPwd(t),
		InitialHead: "base000",
	})

	p := newOutcomeProbe(t)
	p.cs.SetSelectedCwd(mustPwd(t))
	if _, err := dispatchCommit(p.ctx, p.cs, HandlerDeps{Git: git}, "chat", "msg", commitArgs{}, Config{}); err != nil {
		t.Fatalf("dispatchCommit: %v", err)
	}
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, map[string]string{"branch": "wt-clean"})
	assertNoError(t, got)
}

// TestGtwOutcome_CommitAheadOfTheDefaultBranch covers the fallback
// baseline: the state file does not carry the initial head, so the
// local default branch is the baseline — a branch ahead of it is the
// same normal result.
func TestGtwOutcome_CommitAheadOfTheDefaultBranch(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)
	git.onArgs([]string{"symbolic-ref", "--short", "refs/remotes/origin/HEAD"},
		"origin/main\n", "", nil)
	// The default branch's name is resolved to a commit before it is
	// used as a rev range.
	git.onArgs([]string{"rev-parse", "--verify", "main^{commit}"},
		"base000\n", "", nil)
	git.onArgs([]string{"log", "--format=%ae\t%h %s", "base000..HEAD"},
		"test@example.com\tabc1234 feat: the work\n", "", nil)

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
	got := p.settle(t)
	assertOutcome(t, got, http.StatusOK, map[string]string{"commit_log": "abc1234 feat: the work"})
	assertNoError(t, got)
}

// TestGtwOutcome_PushAlreadyOnRemote covers the enhanced judgment of
// /gtw push: the branch carries no commit the remote does not have,
// and the remote's tip is the local head — the coding agent pushed
// the branch itself.
//
// The outcome is a normal branch: there was nothing to push.
func TestGtwOutcome_PushAlreadyOnRemote(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)
	git.onArgs([]string{"rev-list", "--count", "origin/wt-clean..HEAD"}, "0\n", "", nil)

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
	assertOutcome(t, got, http.StatusOK, map[string]string{"branch": "wt-clean"})
	assertNoError(t, got)
}

// TestGtwOutcome_PushBehindTheRemote covers a branch the remote has
// moved past: the local carries no commit the remote does not have,
// so there is nothing to push — a normal branch.
//
// The count is what tells the two apart: a branch that is behind its
// remote has the same HEAD as "up to date" would, and a plain SHA
// comparison would call it a branch with commits to push.
func TestGtwOutcome_PushBehindTheRemote(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean [behind 1]\n", "", nil)
	git.onArgs([]string{"rev-list", "--count", "origin/wt-clean..HEAD"}, "0\n", "", nil)

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
	assertOutcome(t, got, http.StatusOK, map[string]string{"branch": "wt-clean"})
	assertNoError(t, got)
}

// TestGtwOutcome_PushLocalAheadOfRemote covers the other side of the
// enhanced judgment: the local head is not the remote's tip, so the
// local still carries commits the remote does not have — an
// exception.
func TestGtwOutcome_PushLocalAheadOfRemote(t *testing.T) {
	git := newPushGit()
	git.onArgs(statusCmd, "## wt-clean...origin/wt-clean\n", "", nil)
	git.onArgs([]string{"rev-list", "--count", "origin/wt-clean..HEAD"}, "2\n", "", nil)

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
	assertOutcome(t, got, http.StatusConflict, map[string]string{"branch": "wt-clean"})
	assertError(t, got)
}

// TestGtwOutcome_PRExistsIsANormalBranch covers /gtw pr on a branch
// that already has a pull request: the pull request the command was
// asked for is there, so the outcome is a normal branch — no error
// description rides along.
func TestGtwOutcome_PRExistsIsANormalBranch(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt"})
	setupPRGit(rig, "wt", 0)
	rig.prov.SetFindOpenPRResp(&PR{
		Number: 42,
		URL:    "https://github.com/octocat/hello/pull/42",
		State:  "open",
	})
	rig.installDeps()

	work := rig.cs.Hold("msg")
	ctx := chatsession.WithWork(context.Background(), work)
	ch := captureCh(t, rig.cs)
	if _, err := dispatchPR(ctx, rig.cs, rig.deps, "chat", "msg", prArgs{}, Config{}); err != nil {
		t.Fatalf("dispatchPR: %v", err)
	}
	work.Release()

	got := settledOutcomeOf(ch)
	assertOutcome(t, got, http.StatusOK, map[string]string{"pr_number": "42"})
	assertNoError(t, got)
}

// TestJudgeFixWork_CountsWhatTheAgentLeftBehind covers the work
// /gtw fix counts when it judges its own outcome: the changes the
// tree holds plus the commits the agent made.
func TestJudgeFixWork_CountsWhatTheAgentLeftBehind(t *testing.T) {
	repo := initTempRepo(t)
	deps := HandlerDeps{Git: ExecGitRunner{}}
	base, err := headSHA(context.Background(), repo, deps)
	if err != nil {
		t.Fatalf("headSHA: %v", err)
	}

	// Nothing yet: the agent left the worktree unchanged.
	got := judgeFixWork(context.Background(), deps, repo, "main", repo, base, nil)
	if got.Status != http.StatusInternalServerError {
		t.Errorf("outcome status = %d, want %d", got.Status, http.StatusInternalServerError)
	}
	assertError(t, &got)

	// One untracked file: the agent produced something.
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write new.go: %v", err)
	}
	got = judgeFixWork(context.Background(), deps, repo, "main", repo, base, nil)
	if got.Status != http.StatusOK {
		t.Fatalf("outcome = %+v, want ok — the agent produced a file", got)
	}
	if got.Data["changes"] != "1" || got.Data["untracked"] != "1" {
		t.Errorf("outcome data = %v, want one untracked change", got.Data)
	}
}

// TestJudgeFixWork_CountsTheAgentsCommits covers the committed half
// of the work /gtw fix counts: the commits the agent made since the
// branch was cut from its initial head.
//
// The commit gtw itself lands on the branch (the .gitignore commit)
// is not the agent's work and must not be counted.
func TestJudgeFixWork_CountsTheAgentsCommits(t *testing.T) {
	repo := initTempRepo(t)
	deps := HandlerDeps{Git: ExecGitRunner{}}
	base, err := headSHA(context.Background(), repo, deps)
	if err != nil {
		t.Fatalf("headSHA: %v", err)
	}

	// gtw's own tool commit: not the agent's work.
	mustGit(t, repo, "-c", "user.name=nightme-gtw", "-c", "user.email=gtw@localhost",
		"commit", "--allow-empty", "-m", "gtw: ignore .nightme/")
	got := judgeFixWork(context.Background(), deps, repo, "main", repo, base, nil)
	if got.Status != http.StatusInternalServerError {
		t.Fatalf("outcome = %+v, want failed — only gtw's own commit is on the branch", got)
	}

	// The agent's commit: the work landed.
	mustGit(t, repo, "commit", "--allow-empty", "-m", "feat: the work")
	got = judgeFixWork(context.Background(), deps, repo, "main", repo, base, nil)
	if got.Status != http.StatusOK {
		t.Fatalf("outcome = %+v, want ok — the agent committed", got)
	}
	if got.Data["commits"] != "1" {
		t.Errorf("outcome commits = %q, want 1", got.Data["commits"])
	}
}
