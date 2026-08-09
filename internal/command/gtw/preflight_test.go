package gtw

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)



// TestPreflightOrphanYml_CleanRepo is the happy path: ActiveCwd
// is the main repo, no orphan yml anywhere. preflightOrphanYml
// must return nil.
func TestPreflightOrphanYml_CleanRepo(t *testing.T) {
	repoRoot := initTempRepo(t)
	// Create one worktree with NO yml (the typical state).
	wt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-clean")
	mustGit(t, repoRoot, "worktree", "add", "-b", "feat/clean", wt, "HEAD")

	if err := preflightOrphanYml(context.Background(), repoRoot, ExecGitRunner{}); err != nil {
		t.Fatalf("preflightOrphanYml on clean repo: %v", err)
	}
}

// TestPreflightOrphanYml_ActiveCwdIsFixWorktree covers case 1
// from preflightOrphanYml's doc: the user is sitting inside a
// fix worktree (i.e. .nightme/gtw.yml exists at ActiveCwd).
// preflightOrphanYml must reject.
func TestPreflightOrphanYml_ActiveCwdIsFixWorktree(t *testing.T) {
	repoRoot := initTempRepo(t)
	wt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-fix42")
	mustGit(t, repoRoot, "worktree", "add", "-b", "fix/42", wt, "HEAD")

	// Seed a yml in the worktree (mimicking an in-flight fix).
	if err := os.MkdirAll(filepath.Join(wt, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, nightmeDirName, gtwYmlName),
		[]byte("mode: remote\nissue: 42\n"), 0o600); err != nil {
		t.Fatalf("write yml: %v", err)
	}

	err := preflightOrphanYml(context.Background(), wt, ExecGitRunner{})
	if err == nil {
		t.Fatal("preflightOrphanYml: want error for yml-at-ActiveCwd, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want 'already exists' phrase", err.Error())
	}
	if !strings.Contains(err.Error(), "/gtw close") {
		t.Errorf("error should mention /gtw close recovery: %q", err.Error())
	}
}

// TestPreflightOrphanYml_SiblingWorktreeHasYml covers case 2:
// ActiveCwd is the main repo (no yml here), but a sibling
// worktree holds an orphan yml.
func TestPreflightOrphanYml_SiblingWorktreeHasYml(t *testing.T) {
	repoRoot := initTempRepo(t)

	// Worktree A: clean, no yml.
	wtA := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-a")
	mustGit(t, repoRoot, "worktree", "add", "-b", "feat/a", wtA, "HEAD")

	// Worktree B: HAS an orphan yml (e.g. /gtw fix that didn't
	// close).
	wtB := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-b")
	mustGit(t, repoRoot, "worktree", "add", "-b", "fix/42", wtB, "HEAD")
	if err := os.MkdirAll(filepath.Join(wtB, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtB, nightmeDirName, gtwYmlName),
		[]byte("mode: remote\nissue: 42\n"), 0o600); err != nil {
		t.Fatalf("write yml: %v", err)
	}

	err := preflightOrphanYml(context.Background(), repoRoot, ExecGitRunner{})
	if err == nil {
		t.Fatal("preflightOrphanYml: want error for sibling yml, got nil")
	}
	if !strings.Contains(err.Error(), "sibling") {
		t.Errorf("error = %q, want 'sibling' phrase", err.Error())
	}
	if !strings.Contains(err.Error(), wtB) {
		t.Errorf("error should name the sibling worktree %q:\n%s", wtB, err.Error())
	}
}

// TestPreflightOrphanYml_NotAGitRepo must NOT fail even if
// ActiveCwd isn't a git repo. preflightOrphanYml is a guard,
// not a gate; downstream PreflightWorktreeCreate handles the
// "not in a git repo" failure.
func TestPreflightOrphanYml_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	if err := preflightOrphanYml(context.Background(), dir, ExecGitRunner{}); err != nil {
		t.Fatalf("preflightOrphanYml on non-git dir: %v", err)
	}
}

// TestParseWorktreePaths sanity-checks the porcelain parser
// against the canonical `git worktree list --porcelain` shape.
// Used by preflightOrphanYml — important to keep stable because
// the function's correctness depends on it.
func TestParseWorktreePaths(t *testing.T) {
	porcelain := "" +
		"worktree /home/user/repo\n" +
		"HEAD abc123\n" +
		"branch refs/heads/main\n" +
		"\n" +
		"worktree /home/user/repo.wt-fix-42\n" +
		"HEAD def456\n" +
		"branch refs/heads/fix/42-foo\n" +
		"\n" // trailing blank line, common from `git worktree list`

	got := parseWorktreePaths(porcelain)
	want := []string{"/home/user/repo", "/home/user/repo.wt-fix-42"}
	if len(got) != len(want) {
		t.Fatalf("got %d paths, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] got %q, want %q", i, got[i], w)
		}
	}
}

// TestPreflightOrphanYml_RunFixIntegration verifies the end-
// to-end integration: a real /gtw fix flow that picks up an
// orphan yml must short-circuit at RunFix's preflight rather
// than silently overwriting. This is the test that would have
// caught the original bug if it had existed when the orphan
// case was first introduced.
func TestPreflightOrphanYml_RunFixIntegration(t *testing.T) {
	repoRoot := initTempRepo(t)
	mustGit(t, repoRoot, "remote", "add", "origin",
		"https://github.com/cnlangzi/nightme.git")
	mustGit(t, repoRoot, "symbolic-ref", "refs/remotes/origin/HEAD",
		"refs/remotes/origin/main")

	cs, _ := chatsession.New("chat-preflight", "test-agent", newTestChannel())
	_ = cs.SetActiveCwd(repoRoot)

	// Simulate an orphan yml: create a worktree with a yml in
	// it. The user is sitting in the main repo (not the
	// worktree), but git knows about the worktree via
	// `git worktree list`.
	orphanWt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-orphan")
	mustGit(t, repoRoot, "worktree", "add", "-b", "fix/99", orphanWt, "HEAD")
	if err := os.MkdirAll(filepath.Join(orphanWt, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanWt, nightmeDirName, gtwYmlName),
		[]byte("mode: local\nissue: -1\n"), 0o600); err != nil {
		t.Fatalf("write yml: %v", err)
	}

	var sentTexts []string
	deps := HandlerDeps{
		Git: ExecGitRunner{},
		Now: nil,
		Send: func(_ context.Context, m OutMsg) error {
			sentTexts = append(sentTexts, m.Text)
			return nil
		},
		SkipRefreshDefaultBranch: true,
	}
	slot := &memSlot{}

	// /gtw fix 1 — should hit preflightOrphanYml and abort.
	res, err := RunFix(
		context.Background(), ModeRemote, cs, slot, newMemDrafts(), deps,
		cs.ChatID, "msg", []string{"1"},
		false, /* force */
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Errorf("Result.Consumed = false")
	}
	last := sentTexts[len(sentTexts)-1]
	if !strings.Contains(last, "sibling") || !strings.Contains(last, orphanWt) {
		t.Errorf("reply missing sibling-yaml hint:\n%s", last)
	}
	// No NEW worktree should have been created.
	wtOut, _ := mustGitOut(t, repoRoot, "worktree", "list", "--porcelain")
	if c := strings.Count(wtOut, "worktree "); c != 2 {
		// 2 = main repo + the orphan we seeded; we expect no
		// new worktree from the rejected /gtw fix.
		t.Errorf("worktree count = %d, want 2 (no new from rejected /gtw fix):\n%s", c, wtOut)
	}
}