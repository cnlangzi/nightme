package gtw

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestPreflightOrphanYml_CleanRepo is the happy path: SelectedCwd
// is the main repo, no yml anywhere. preflightOrphanYml must
// return nil.
func TestPreflightOrphanYml_CleanRepo(t *testing.T) {
	repoRoot := initTempRepo(t)
	// Create one worktree with NO yml (the typical state).
	wt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-clean")
	mustGit(t, repoRoot, "worktree", "add", "-b", "feat/clean", wt, "HEAD")

	if err := preflightOrphanYml(repoRoot); err != nil {
		t.Fatalf("preflightOrphanYml on clean repo: %v", err)
	}
}

// TestPreflightOrphanYml_ActiveCwdIsFixWorktree covers the ONE
// case preflightOrphanYml still rejects: the user is sitting
// inside a fix worktree (i.e. .nightme/gtw.yml exists at
// SelectedCwd). The new fix would inherit / contradict the old.
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

	err := preflightOrphanYml(wt)
	if err == nil {
		t.Fatal("preflightOrphanYml: want error for yml-at-SelectedCwd, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want 'already exists' phrase", err.Error())
	}
	if !strings.Contains(err.Error(), "/gtw close") {
		t.Errorf("error should mention /gtw close recovery: %q", err.Error())
	}
}

// TestPreflightOrphanYml_SiblingWorktreeYml_AllowedFromMain is
// the v1.x invariant: a sibling worktree holding an orphan yml
// must NOT block /gtw fix in the main repo. Parallel fixes
// across separate worktrees are the explicit design goal —
// the previous "one fix per repo" check was removed because it
// defeated the purpose of git worktrees.
func TestPreflightOrphanYml_SiblingWorktreeYml_AllowedFromMain(t *testing.T) {
	repoRoot := initTempRepo(t)

	// Sibling worktree with a yml from a previous (unclosed) fix.
	wtB := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-b")
	mustGit(t, repoRoot, "worktree", "add", "-b", "fix/42", wtB, "HEAD")
	if err := os.MkdirAll(filepath.Join(wtB, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtB, nightmeDirName, gtwYmlName),
		[]byte("mode: remote\nissue: 42\n"), 0o600); err != nil {
		t.Fatalf("write yml: %v", err)
	}

	// SelectedCwd is the MAIN repo (no yml here). The sibling
	// yml must be ignored.
	if err := preflightOrphanYml(repoRoot); err != nil {
		t.Fatalf("preflightOrphanYml: sibling yml must not block /gtw fix from main, got: %v", err)
	}
}

// TestPreflightOrphanYml_SiblingWorktreeYml_AllowedFromAnotherSibling
// is the multi-worktree case: even when SelectedCwd is itself a
// sibling worktree (different from the one with the yml), the
// preflight must not block.
func TestPreflightOrphanYml_SiblingWorktreeYml_AllowedFromAnotherSibling(t *testing.T) {
	repoRoot := initTempRepo(t)

	wtA := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-a")
	mustGit(t, repoRoot, "worktree", "add", "-b", "feat/a", wtA, "HEAD")

	wtB := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-b")
	mustGit(t, repoRoot, "worktree", "add", "-b", "fix/42", wtB, "HEAD")
	if err := os.MkdirAll(filepath.Join(wtB, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtB, nightmeDirName, gtwYmlName),
		[]byte("mode: remote\nissue: 42\n"), 0o600); err != nil {
		t.Fatalf("write yml: %v", err)
	}

	// SelectedCwd is wtA (no yml); wtB has an orphan yml.
	// /gtw fix starting from wtA must not be blocked.
	if err := preflightOrphanYml(wtA); err != nil {
		t.Fatalf("preflightOrphanYml: sibling yml must not block /gtw fix from another sibling, got: %v", err)
	}
}

// TestPreflightOrphanYml_NotAGitRepo must NOT fail even if
// SelectedCwd isn't a git repo. preflightOrphanYml is a guard,
// not a gate; downstream PreflightWorktreeCreate handles the
// "not in a git repo" failure.
func TestPreflightOrphanYml_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	if err := preflightOrphanYml(dir); err != nil {
		t.Fatalf("preflightOrphanYml on non-git dir: %v", err)
	}
}

// TestPreflightOrphanYml_RunFixIntegration_AllowsParallel
// exercises the end-to-end flow: a sibling worktree has an
// orphan yml, and the user runs /gtw fix from the main repo.
// v1.x must allow this — the new fix creates a fresh worktree
// at a fresh path, writes its own yml, and leaves the orphan
// yml in the sibling alone. This is the test that would have
// failed under v1's "one fix per repo" gate.
func TestPreflightOrphanYml_RunFixIntegration_AllowsParallel(t *testing.T) {
	repoRoot := initTempRepo(t)

	// ch unused after F-45 refactor
	cs, _ := chatsession.New("chat-parallel", "test-agent")
	_ = cs.SetSelectedCwd(repoRoot)

	// Seed an orphan yml in a sibling worktree.
	orphanWt := filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+".wt-orphan")
	mustGit(t, repoRoot, "worktree", "add", "-b", "fix/99", orphanWt, "HEAD")
	if err := os.MkdirAll(filepath.Join(orphanWt, nightmeDirName), 0o755); err != nil {
		t.Fatalf("mkdir .nightme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanWt, nightmeDirName, gtwYmlName),
		[]byte("mode: local\nissue: -1\n"), 0o600); err != nil {
		t.Fatalf("write yml: %v", err)
	}

	// Capture the orphan's yml content to verify untouched-ness
	// after the second fix.
	orphanYmlBefore, err := os.ReadFile(filepath.Join(orphanWt, nightmeDirName, gtwYmlName))
	if err != nil {
		t.Fatalf("read orphan yml before: %v", err)
	}

	deps := HandlerDeps{
		Git:                      ExecGitRunner{},
		Now:                      nil,
		SkipRefreshDefaultBranch: true,
	}
	slot := &memSlot{}

	// /gtw fix --name fix/cwd (ModeLocal, no remote needed) —
	// must succeed despite the sibling yml.
	res, err := RunFix(
		context.Background(), ModeLocal, cs, slot, newMemDrafts(), deps,
		cs.ChatID, "msg", []string{"--name", "fix/cwd"},
		false, /* force */
	)
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if !res.Consumed {
		t.Errorf("Result.Consumed = false")
	}

	// A NEW worktree must have been created (in addition to
	// the seed orphan). Expected count: 3 = main repo + orphan
	// seed + new fix.
	wtOut, _ := mustGitOut(t, repoRoot, "worktree", "list", "--porcelain")
	if c := strings.Count(wtOut, "worktree "); c != 3 {
		t.Errorf("worktree count = %d, want 3 (main + orphan + new fix):\n%s", c, wtOut)
	}

	// The orphan yml must be UNCHANGED.
	orphanYmlAfter, err := os.ReadFile(filepath.Join(orphanWt, nightmeDirName, gtwYmlName))
	if err != nil {
		t.Fatalf("read orphan yml after: %v", err)
	}
	if string(orphanYmlAfter) != string(orphanYmlBefore) {
		t.Errorf("orphan yml was modified — sibling state must be untouched:\nbefore: %q\nafter:  %q",
			string(orphanYmlBefore), string(orphanYmlAfter))
	}
}
